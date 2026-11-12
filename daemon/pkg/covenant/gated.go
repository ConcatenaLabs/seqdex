package covenant

import (
	"bytes"
	"fmt"
)

// GatedOrder is a holder's resting offer to sell a restricted (OpenAMP) asset:
// the tapscript FILL covenant behind the asset's policy key, so every fill is
// the policy server's decision, which it co-signs and broadcasts in one step.
// The scripts are openampd's (openamp, openampd/internal/elements/gated.go),
// pinned by gated_test.go to its vectors:
//
//	P2TR(NUMS, [fill, [refund, claw]]), or P2TR(NUMS, [fill, refund]) without clawback
//	fill  : <PolicyX> OP_CHECKSIGVERIFY <BuildFillLeaf(...)>
//	refund: <Expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
//	        OP_PUSHCURRENTINPUTINDEX OP_0 OP_EQUALVERIFY
//	        OP_0 OP_INSPECTOUTPUTSCRIPTPUBKEY OP_1 OP_EQUALVERIFY <MakerEnclave> OP_EQUALVERIFY
//	        OP_0 OP_INSPECTOUTPUTASSET OP_1 OP_EQUALVERIFY <AssetA> OP_EQUALVERIFY
//	        OP_0 OP_INSPECTOUTPUTVALUE OP_1 OP_EQUALVERIFY
//	        OP_0 OP_INSPECTINPUTVALUE OP_1 OP_EQUALVERIFY OP_EQUAL
//	claw  : <IssuerX> OP_CHECKSIGVERIFY <PolicyX> OP_CHECKSIG
//
// The fill pays the maker at output 2k and keeps the remainder at 2k+1, as a
// covenant order does. The refund is keyless after Expiry: the order at input 0,
// output 0 carrying its whole value home to the maker's enclave.
type GatedOrder struct {
	AssetA, AssetB   [32]byte // internal byte order: sold, and paid for it
	RateNum, RateDen uint64
	MinLot           uint64
	MakerProg        []byte // maker credit program (32 bytes, v1)
	PolicyX          [32]byte
	IssuerX          *[32]byte // nil without clawback
	Expiry           uint32
	MakerEnclave     [32]byte // the maker's enclave program, the refund's home
}

// GatedTree is a built gated order.
type GatedTree struct {
	FillLeaf, RefundLeaf, ClawLeaf []byte // ClawLeaf nil without clawback
	ScriptPubKey                   []byte
	FillControl                    []byte
	RefundControl                  []byte // the keyless refund's control block
}

const (
	opCheckSigVerify = 0xad
	opInspectInput   = opInspectInputValue
	// gatedTimeLock is the smallest expiry read as a time.
	gatedTimeLock = 500_000_000
)

// GatedFillPrefix is the policy gate a gated fill leaf begins with.
func GatedFillPrefix(policyX [32]byte) []byte {
	return append(append([]byte{0x20}, policyX[:]...), opCheckSigVerify)
}

// Tree builds the order from its terms.
func (g GatedOrder) Tree() (*GatedTree, error) {
	fill, err := BuildFillLeaf(g.AssetA, g.AssetB, g.RateNum, g.RateDen, g.MinLot, g.MakerProg, 1)
	if err != nil {
		return nil, err
	}
	return GatedTreeFromFill(g.PolicyX, fill, g.IssuerX, g.Expiry, g.MakerEnclave, g.AssetA)
}

// GatedTreeFromFill builds a gated order around any fill covenant: what
// openampd builds from a placement's terms.
func GatedTreeFromFill(policyX [32]byte, fill []byte, issuerX *[32]byte, expiry uint32, enclave, asset [32]byte) (*GatedTree, error) {
	if expiry < gatedTimeLock {
		return nil, fmt.Errorf("expiry %d is a height: a gated order's expiry is a time (>= %d)", expiry, gatedTimeLock)
	}
	if len(fill) == 0 || len(fill) > 1000 {
		return nil, fmt.Errorf("the fill covenant must be 1 to 1000 bytes")
	}
	t := &GatedTree{FillLeaf: append(GatedFillPrefix(policyX), fill...)}
	s := &scriptBuilder{}
	s.pushNum(int64(expiry))
	s.ops(opCheckLockTimeVerify, opDrop)
	s.ops(opPushCurrentInputIndex, op0, opEqualVerify)
	s.ops(op0, opInspectOutputScriptPubkey, op1, opEqualVerify)
	s.push(enclave[:])
	s.ops(opEqualVerify, op0, opInspectOutputAsset, op1, opEqualVerify)
	s.push(asset[:])
	s.ops(opEqualVerify, op0, opInspectOutputValue, op1, opEqualVerify)
	s.ops(op0, opInspectInput, op1, opEqualVerify, opEqual)
	t.RefundLeaf = s.b
	fillH, refundH := LeafHash(t.FillLeaf), LeafHash(t.RefundLeaf)
	sibling := refundH
	if issuerX != nil {
		c := &scriptBuilder{}
		c.push(issuerX[:])
		c.op(opCheckSigVerify)
		c.push(policyX[:])
		c.op(opChecksig)
		t.ClawLeaf = c.b
		sibling = branch(refundH, LeafHash(t.ClawLeaf))
	}
	root := branch(fillH, sibling)
	outKey, neg, err := tweakOutputKey(NUMS, root)
	if err != nil {
		return nil, err
	}
	t.ScriptPubKey = append([]byte{op1, 0x20}, outKey[:]...)
	t.FillControl = append(append([]byte{byte(leafVersion) + neg}, NUMS[:]...), sibling[:]...)
	t.RefundControl = append([]byte{byte(leafVersion) + neg}, NUMS[:]...)
	if t.ClawLeaf != nil {
		clawH := LeafHash(t.ClawLeaf)
		t.RefundControl = append(t.RefundControl, clawH[:]...)
	}
	t.RefundControl = append(t.RefundControl, fillH[:]...)
	return t, nil
}

// branch is TapBranch/elements over two hashes in ascending order.
func branch(a, b [32]byte) [32]byte {
	if bytes.Compare(b[:], a[:]) < 0 {
		a, b = b, a
	}
	return taggedHash("TapBranch/elements", a[:], b[:])
}

// GatedFillPath reports whether a spend's witness took the fill leaf: its
// leaf (second to last item) begins with the policy gate. Anything else is the
// refund or the claw, which fill nothing.
func GatedFillPath(witness [][]byte, policyX [32]byte) bool {
	if len(witness) < 3 {
		return false
	}
	return bytes.HasPrefix(witness[len(witness)-2], GatedFillPrefix(policyX))
}
