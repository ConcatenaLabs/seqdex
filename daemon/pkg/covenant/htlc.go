package covenant

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// The swap HTLC and its fee pot: the Sequentia side of a restricted (OpenAMP)
// asset traded for on-chain BTC. The scripts are openampd's (openamp,
// openampd/internal/elements/htlc.go), pinned by htlc_test.go to its vectors.
//
// The buyer of the asset, who pays the bitcoin, holds a secret s with
// H = SHA256(s). The seller locks the asset in the HTLC, and a little of an
// accepted fee asset in the pot, in one transaction its policy server
// co-signs. The buyer claims both together with s, the HTLC with the policy
// server's signature, and the pot's witness reveals s in plain tapscript. After
// T_seq, a time, anyone can send each coin home: the HTLC to the seller's
// enclave, the pot to the seller's own v1 program, at input 0 and output 0.
//
//	HTLC, P2TR(NUMS, [claim, [refund, claw]]) or P2TR(NUMS, [claim, refund]):
//	claim : OP_SIZE 32 OP_EQUALVERIFY OP_SHA256 <H> OP_EQUALVERIFY
//	        <BuyerX> OP_CHECKSIGVERIFY <PolicyX> OP_CHECKSIG
//	refund: <TSeq> OP_CHECKLOCKTIMEVERIFY OP_DROP <home: the seller's enclave>
//	claw  : <IssuerX> OP_CHECKSIGVERIFY <PolicyX> OP_CHECKSIG
//
//	Fee pot, P2TR(NUMS, [claim, refund]):
//	claim : OP_SIZE 32 OP_EQUALVERIFY OP_SHA256 <H> OP_EQUALVERIFY <BuyerX> OP_CHECKSIG
//	refund: <TSeq> OP_CHECKLOCKTIMEVERIFY OP_DROP <home: the seller's v1 program>

// MinTimeLock is the smallest lock time read as a Unix time. Every lock of a
// restricted swap is a time, so both chains run on one clock.
const MinTimeLock = 500_000_000

const opSize = 0x82
const opSHA256 = 0xa8

// SwapHTLC is everything a swap HTLC commits to.
type SwapHTLC struct {
	Hash          [32]byte
	BuyerX        [32]byte  // the buyer's enclave key (its account's first key)
	PolicyX       [32]byte  // the asset's policy key
	IssuerX       *[32]byte // the clawback key, only where the asset elected clawback
	TSeq          uint32    // the refund opens at this median time past
	SellerProgram [32]byte  // the seller's enclave program
	Asset         [32]byte  // internal byte order
	Value         uint64
}

// FeePot is everything a fee pot commits to.
type FeePot struct {
	Hash          [32]byte
	BuyerX        [32]byte
	TSeq          uint32
	SellerProgram [32]byte // the seller's own v1 program; never v0 (a staking key's P2WPKH)
	FeeAsset      [32]byte // internal byte order
	Value         uint64
}

// TapLeaf is one leaf of a built tree: its script and its control block.
type TapLeaf struct {
	Script  []byte
	Control []byte
}

// TapTree is a built NUMS-keyed taproot tree.
type TapTree struct {
	ScriptPubKey []byte
	OutputKey    [32]byte
	Leaves       map[string]TapLeaf
}

type namedLeaf struct {
	name   string
	script []byte
	hash   [32]byte
	branch []byte
}

// buildTree lays the leaves out as openampd does: the first half on the left,
// recursively, so [a, b, c] is [a, [b, c]].
func buildTree(leaves ...namedLeaf) (*TapTree, error) {
	ls := make([]*namedLeaf, len(leaves))
	for i := range leaves {
		l := leaves[i]
		l.hash = LeafHash(l.script)
		ls[i] = &l
	}
	root := buildNode(ls)
	out, neg, err := tweakOutputKey(NUMS, root)
	if err != nil {
		return nil, err
	}
	t := &TapTree{OutputKey: out, ScriptPubKey: append([]byte{op1, 0x20}, out[:]...), Leaves: map[string]TapLeaf{}}
	for _, l := range ls {
		ctl := append([]byte{byte(leafVersion) + neg}, NUMS[:]...)
		t.Leaves[l.name] = TapLeaf{Script: l.script, Control: append(ctl, l.branch...)}
	}
	return t, nil
}

func buildNode(ls []*namedLeaf) [32]byte {
	if len(ls) == 1 {
		return ls[0].hash
	}
	split := len(ls) / 2
	left, right := buildNode(ls[:split]), buildNode(ls[split:])
	for _, l := range ls[:split] {
		l.branch = append(l.branch, right[:]...)
	}
	for _, r := range ls[split:] {
		r.branch = append(r.branch, left[:]...)
	}
	return branch(left, right)
}

func hashLock(h [32]byte) []byte {
	s := []byte{opSize, 0x01, 0x20, opEqualVerify, opSHA256, 0x20}
	s = append(s, h[:]...)
	return append(s, opEqualVerify)
}

func timeLock(t uint32) []byte {
	s := &scriptBuilder{}
	s.pushNum(int64(t))
	s.ops(opCheckLockTimeVerify, opDrop)
	return s.b
}

// home: the coin is input 0, and output 0 pays exactly value of asset to the
// v1 program.
func home(program [32]byte, asset [32]byte, value uint64) []byte {
	s := []byte{opPushCurrentInputIndex, op0, opEqualVerify}
	s = append(s, opPushCurrentInputIndex, opDup, opDup, opInspectOutputScriptPubkey, op1, opEqualVerify, 0x20)
	s = append(s, program[:]...)
	s = append(s, opEqualVerify, opInspectOutputAsset, op1, opEqualVerify, 0x20)
	s = append(s, asset[:]...)
	s = append(s, opEqualVerify, opInspectOutputValue, op1, opEqualVerify, 0x08)
	var v [8]byte
	binary.LittleEndian.PutUint64(v[:], value)
	s = append(s, v[:]...)
	return append(s, opEqual)
}

// CheckSigPair is <a> OP_CHECKSIGVERIFY <b> OP_CHECKSIG.
func CheckSigPair(a, b [32]byte) []byte {
	s := append([]byte{0x20}, a[:]...)
	s = append(s, opCheckSigVerify, 0x20)
	s = append(s, b[:]...)
	return append(s, opChecksig)
}

// Tree builds the HTLC: leaves "claim", "refund" and, with clawback, "claw".
func (h SwapHTLC) Tree() (*TapTree, error) {
	if h.TSeq < MinTimeLock {
		return nil, fmt.Errorf("T_seq %d is a height: every swap lock is a time (>= %d)", h.TSeq, MinTimeLock)
	}
	if h.Value == 0 {
		return nil, fmt.Errorf("an HTLC of zero atoms")
	}
	leaves := []namedLeaf{
		{name: "claim", script: append(hashLock(h.Hash), CheckSigPair(h.BuyerX, h.PolicyX)...)},
		{name: "refund", script: append(timeLock(h.TSeq), home(h.SellerProgram, h.Asset, h.Value)...)},
	}
	if h.IssuerX != nil {
		leaves = append(leaves, namedLeaf{name: "claw", script: CheckSigPair(*h.IssuerX, h.PolicyX)})
	}
	return buildTree(leaves...)
}

// Tree builds the fee pot: leaves "claim" and "refund".
func (p FeePot) Tree() (*TapTree, error) {
	if p.TSeq < MinTimeLock {
		return nil, fmt.Errorf("T_seq %d is a height: every swap lock is a time (>= %d)", p.TSeq, MinTimeLock)
	}
	if p.Value == 0 {
		return nil, fmt.Errorf("a fee pot of zero atoms")
	}
	claim := append(hashLock(p.Hash), 0x20)
	claim = append(claim, p.BuyerX[:]...)
	claim = append(claim, opChecksig)
	return buildTree(
		namedLeaf{name: "claim", script: claim},
		namedLeaf{name: "refund", script: append(timeLock(p.TSeq), home(p.SellerProgram, p.FeeAsset, p.Value)...)},
	)
}

// EnclaveTree is an OpenAMP holder's enclave, P2TR(NUMS, [transfer, claw]):
// transfer <UserX> CHECKSIGVERIFY <PolicyX> CHECKSIG, and the claw where the
// asset elected clawback.
func EnclaveTree(userX, policyX [32]byte, issuerX *[32]byte) (*TapTree, error) {
	leaves := []namedLeaf{{name: "transfer", script: CheckSigPair(userX, policyX)}}
	if issuerX != nil {
		leaves = append(leaves, namedLeaf{name: "claw", script: CheckSigPair(*issuerX, policyX)})
	}
	return buildTree(leaves...)
}

// PotSecret reads s from a spend of a fee pot by its claim leaf: the witness
// is <buyer sig> <s> <claim leaf> <control>, and s must hash to want. A refund
// (no secret) or anything else returns false.
func PotSecret(witness [][]byte, want [32]byte) ([]byte, bool) {
	for _, item := range witness {
		if len(item) == 32 && sha256Sum(item) == want {
			return append([]byte(nil), item...), true
		}
	}
	return nil, false
}

// IsHTLCClaimLeaf reports whether a leaf script is a swap-HTLC or pot claim
// for H (it begins with the hash lock).
func IsHTLCClaimLeaf(leaf []byte, h [32]byte) bool {
	return bytes.HasPrefix(leaf, hashLock(h))
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
