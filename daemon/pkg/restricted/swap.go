package restricted

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// Swap is one swap HTLC and its fee pot: everything both scripts commit to.
// Assets are in internal byte order.
type Swap struct {
	Hash          [32]byte
	BuyerX        [32]byte
	PolicyX       [32]byte
	IssuerX       *[32]byte
	TSeq          uint32
	SellerEnclave [32]byte // the seller's enclave program: the HTLC's home
	Asset         [32]byte
	Value         uint64
	PotAsset      [32]byte
	PotValue      uint64
	SellerPayout  [32]byte // the seller's own v1 program: the pot's home
}

// Trees builds the HTLC and the pot.
func (s Swap) Trees() (htlc, pot *covenant.TapTree, err error) {
	if htlc, err = (covenant.SwapHTLC{Hash: s.Hash, BuyerX: s.BuyerX, PolicyX: s.PolicyX, IssuerX: s.IssuerX,
		TSeq: s.TSeq, SellerProgram: s.SellerEnclave, Asset: s.Asset, Value: s.Value}).Tree(); err != nil {
		return nil, nil, err
	}
	if pot, err = (covenant.FeePot{Hash: s.Hash, BuyerX: s.BuyerX, TSeq: s.TSeq, SellerProgram: s.SellerPayout,
		FeeAsset: s.PotAsset, Value: s.PotValue}).Tree(); err != nil {
		return nil, nil, err
	}
	return htlc, pot, nil
}

// Coin is an explicit coin an input spends.
type Coin struct {
	Txid   string // display hex
	Vout   uint32
	Asset  [32]byte // internal byte order
	Value  uint64
	Script []byte
}

func (c Coin) spent() covenant.SpentCoin { return covenant.ExplicitCoin(c.Asset, c.Value, c.Script) }

// Out is an explicit output; an empty Script is the fee.
type Out struct {
	Asset  [32]byte
	Value  uint64
	Script []byte
}

// Unsigned is a transaction together with the coins it spends, in input order.
type Unsigned struct {
	Tx    *transaction.Transaction
	Coins []Coin
}

// Build lays out a version-2 transaction with final sequences, or one below
// final with a lock time (CLTV needs a non-final input). Neither signals
// BIP125, and a swap's lock, claim and refunds must never: the claim carries
// the secret, and a lock that could be replaced would, after an anchor reorg,
// take the buyer's claim (its child) out of a mempool with it once the secret
// is public, leaving the seller free to claim the bitcoin and keep the asset.
func Build(coins []Coin, outs []Out, locktime uint32) (*Unsigned, error) {
	tx := transaction.NewTx(2)
	tx.Locktime = locktime
	for _, c := range coins {
		h, err := elementsutil.TxIDToBytes(c.Txid)
		if err != nil || len(h) != 32 {
			return nil, fmt.Errorf("txid %q", c.Txid)
		}
		in := transaction.NewTxInput(h, c.Vout)
		in.Sequence = 0xffffffff
		if locktime != 0 {
			in.Sequence = 0xfffffffe
		}
		tx.AddInput(in)
	}
	for i, o := range outs {
		if o.Value == 0 {
			return nil, fmt.Errorf("output %d of zero", i)
		}
		v, err := elementsutil.ValueToBytes(o.Value)
		if err != nil {
			return nil, err
		}
		tx.AddOutput(transaction.NewTxOutput(append([]byte{0x01}, o.Asset[:]...), v, o.Script))
	}
	return &Unsigned{Tx: tx, Coins: coins}, nil
}

// Sighash is the BIP341 (Elements) sighash of input i spending the given leaf,
// SIGHASH_DEFAULT. genesis is in internal byte order.
func (u *Unsigned) Sighash(i int, leaf []byte, genesis [32]byte) [32]byte {
	scripts := make([][]byte, len(u.Coins))
	assets := make([][]byte, len(u.Coins))
	values := make([][]byte, len(u.Coins))
	for k, c := range u.Coins {
		sc := c.spent()
		scripts[k], assets[k], values[k] = sc.Script, sc.Asset, sc.Value
	}
	g := chainhash.Hash(genesis)
	lh := chainhash.Hash(covenant.LeafHash(leaf))
	return u.Tx.HashForWitnessV1(i, scripts, assets, values, txscript.SigHashDefault, &g, &lh, nil)
}

// Txid is the transaction's id, display hex.
func (u *Unsigned) Txid() string { return u.Tx.TxHash().String() }

// Hex serialises the transaction with whatever witnesses it holds.
func (u *Unsigned) Hex() (string, error) { return u.Tx.ToHex() }

// SenderSig is a sender's request for a co-sign: BIP340 by its enclave key over
// tagged_hash("openamp-cosign-v1", txid), txid the 32 bytes of the display id.
func SenderSig(key *btcec.PrivateKey, txid string) (string, error) {
	b, err := hex.DecodeString(txid)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("txid %q", txid)
	}
	msg := chainhash.TaggedHash([]byte("openamp-cosign-v1"), b)
	sig, err := schnorr.Sign(key, msg[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// Sign is a BIP340 signature of a sighash.
func Sign(key *btcec.PrivateKey, h [32]byte) ([]byte, error) {
	sig, err := schnorr.Sign(key, h[:])
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

// XOnly is a key's x-only public key.
func XOnly(key *btcec.PrivateKey) [32]byte {
	var x [32]byte
	copy(x[:], schnorr.SerializePubKey(key.PubKey()))
	return x
}

// CheckedPolicySig returns the policy server's signature for input i once its
// sighash, leaf and control are the ones computed here, and the signature
// verifies under the policy key. Anything else is refused: a wallet never
// signs a sighash it did not compute.
func (u *Unsigned) CheckedPolicySig(r *CosignResponse, i int, leaf, control []byte, policyX [32]byte, genesis [32]byte) ([]byte, [32]byte, error) {
	h := u.Sighash(i, leaf, genesis)
	for _, s := range r.Sigs {
		if s.Input != i {
			continue
		}
		if s.Sighash != hex.EncodeToString(h[:]) {
			return nil, h, fmt.Errorf("input %d: the policy server's sighash %s is not the one computed here, %x", i, s.Sighash, h)
		}
		if s.Leaf != hex.EncodeToString(leaf) || s.Control != hex.EncodeToString(control) {
			return nil, h, fmt.Errorf("input %d: the policy server signed another leaf", i)
		}
		sig, err := hex.DecodeString(s.PolicySig)
		if err != nil || len(sig) != 64 {
			return nil, h, fmt.Errorf("input %d: policy signature %q", i, s.PolicySig)
		}
		if err := verify(policyX, h, sig); err != nil {
			return nil, h, fmt.Errorf("input %d: the policy signature does not verify: %w", i, err)
		}
		return sig, h, nil
	}
	return nil, h, fmt.Errorf("the policy server returned no signature for input %d", i)
}

func verify(x [32]byte, h [32]byte, sig []byte) error {
	pk, err := schnorr.ParsePubKey(x[:])
	if err != nil {
		return err
	}
	s, err := schnorr.ParseSignature(sig)
	if err != nil {
		return err
	}
	if !s.Verify(h[:], pk) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

// LockPlan is the seller's lock: its enclave coins of the asset, then coins of
// the pot's asset that also pay the fee. Outputs: 0 the HTLC, 1 the pot, then
// the asset's change home to the seller's enclave, then the fee asset's change,
// then the fee.
type LockPlan struct {
	Swap         Swap
	EnclaveCoins []Coin // the seller's coins of the asset, at its enclave
	FeeCoins     []Coin // coins of Swap.PotAsset, which pays the pot and the fee
	FeeChange    []byte // where the fee asset's change goes
	Fee          uint64 // in Swap.PotAsset
	// Reserve outputs, of the pot's asset, follow the asset's change: coins the
	// seller sets aside to pay its refunds' fees.
	Reserve []Out
}

// BuildLock lays out the lock.
func BuildLock(p LockPlan) (*Unsigned, error) {
	htlc, pot, err := p.Swap.Trees()
	if err != nil {
		return nil, err
	}
	var haveA, haveF uint64
	for _, c := range p.EnclaveCoins {
		if c.Asset != p.Swap.Asset {
			return nil, fmt.Errorf("enclave coin %s:%d is not the asset", c.Txid, c.Vout)
		}
		haveA += c.Value
	}
	for _, c := range p.FeeCoins {
		if c.Asset != p.Swap.PotAsset {
			return nil, fmt.Errorf("fee coin %s:%d is not the pot's asset", c.Txid, c.Vout)
		}
		haveF += c.Value
	}
	if haveA < p.Swap.Value {
		return nil, fmt.Errorf("the enclave coins hold %d of the %d to lock", haveA, p.Swap.Value)
	}
	var reserved uint64
	for _, r := range p.Reserve {
		if r.Asset != p.Swap.PotAsset {
			return nil, fmt.Errorf("a reserve output is not the pot's asset")
		}
		reserved += r.Value
	}
	if haveF < p.Swap.PotValue+p.Fee+reserved {
		return nil, fmt.Errorf("the fee coins hold %d, short of the pot %d, the reserve %d and the fee %d", haveF, p.Swap.PotValue, reserved, p.Fee)
	}
	enclaveSPK := append([]byte{0x51, 0x20}, p.Swap.SellerEnclave[:]...)
	outs := []Out{{p.Swap.Asset, p.Swap.Value, htlc.ScriptPubKey}, {p.Swap.PotAsset, p.Swap.PotValue, pot.ScriptPubKey}}
	if haveA > p.Swap.Value {
		outs = append(outs, Out{p.Swap.Asset, haveA - p.Swap.Value, enclaveSPK})
	}
	outs = append(outs, p.Reserve...)
	if ch := haveF - p.Swap.PotValue - p.Fee - reserved; ch > 0 {
		outs = append(outs, Out{p.Swap.PotAsset, ch, p.FeeChange})
	}
	outs = append(outs, Out{p.Swap.PotAsset, p.Fee, nil})
	return Build(append(append([]Coin{}, p.EnclaveCoins...), p.FeeCoins...), outs, 0)
}

// ClaimPlan is the buyer's claim: the HTLC at input 0, the pot at input 1. The
// HTLC's whole amount goes to the buyer's enclave at output 0; the pot pays the
// fee and its change goes to the buyer. A claim is final: lock time 0, final
// sequences, so it is valid at once.
type ClaimPlan struct {
	Swap         Swap
	HTLC, Pot    Coin
	BuyerEnclave []byte // the buyer's enclave scriptPubKey for the asset
	PotChange    []byte // the buyer's own script for what the pot leaves
	Fee          uint64 // in the pot's asset
}

// BuildClaim lays out the claim.
func BuildClaim(p ClaimPlan) (*Unsigned, error) {
	if p.Fee >= p.Swap.PotValue {
		return nil, fmt.Errorf("the fee %d does not fit in the pot %d", p.Fee, p.Swap.PotValue)
	}
	if p.HTLC.Asset != p.Swap.Asset || p.HTLC.Value != p.Swap.Value || p.Pot.Asset != p.Swap.PotAsset || p.Pot.Value != p.Swap.PotValue {
		return nil, fmt.Errorf("the coins are not the swap's HTLC and pot")
	}
	return Build([]Coin{p.HTLC, p.Pot}, []Out{
		{p.Swap.Asset, p.Swap.Value, p.BuyerEnclave},
		{p.Swap.PotAsset, p.Swap.PotValue - p.Fee, p.PotChange},
		{p.Swap.PotAsset, p.Fee, nil},
	}, 0)
}

// FinishClaim puts the witnesses in: the HTLC's <policy sig> <buyer sig> <s>
// <claim leaf> <control>, the pot's <buyer sig> <s> <claim leaf> <control>.
// It refuses a claim that is not final now, or a secret that is not the swap's.
func FinishClaim(u *Unsigned, s Swap, secret []byte, policySig []byte, buyer *btcec.PrivateKey, genesis [32]byte) error {
	if sha256.Sum256(secret) != s.Hash {
		return fmt.Errorf("the secret is not the swap's")
	}
	if u.Tx.Locktime != 0 {
		return fmt.Errorf("a claim carries no lock time")
	}
	for i, in := range u.Tx.Inputs {
		if in.Sequence != 0xffffffff {
			return fmt.Errorf("input %d of a claim is not final", i)
		}
	}
	if XOnly(buyer) != s.BuyerX {
		return fmt.Errorf("the key is not the buyer's")
	}
	htlc, pot, err := s.Trees()
	if err != nil {
		return err
	}
	hc, pc := htlc.Leaves["claim"], pot.Leaves["claim"]
	h0 := u.Sighash(0, hc.Script, genesis)
	if err := verify(s.PolicyX, h0, policySig); err != nil {
		return fmt.Errorf("the policy signature does not verify for this claim: %w", err)
	}
	b0, err := Sign(buyer, h0)
	if err != nil {
		return err
	}
	b1, err := Sign(buyer, u.Sighash(1, pc.Script, genesis))
	if err != nil {
		return err
	}
	u.Tx.Inputs[0].Witness = [][]byte{policySig, b0, secret, hc.Script, hc.Control}
	u.Tx.Inputs[1].Witness = [][]byte{b1, secret, pc.Script, pc.Control}
	return nil
}

// RefundPlan sends an HTLC or a pot home after T_seq: the coin at input 0,
// home at output 0 with its whole value, then a fee coin the refunder set
// aside for this refund, all of which is the fee. Nobody's key is on the
// coin's path.
type RefundPlan struct {
	Coin    Coin
	Home    []byte // the v1 program script the leaf pins
	Leaf    covenant.TapLeaf
	FeeCoin Coin // its whole value is the fee: a change output would be dust
	TSeq    uint32
}

// BuildRefund lays out a refund; the fee coin's input is left to sign.
func BuildRefund(p RefundPlan) (*Unsigned, error) {
	u, err := Build([]Coin{p.Coin, p.FeeCoin}, []Out{
		{p.Coin.Asset, p.Coin.Value, p.Home},
		{p.FeeCoin.Asset, p.FeeCoin.Value, nil},
	}, p.TSeq)
	if err != nil {
		return nil, err
	}
	u.Tx.Inputs[0].Witness = [][]byte{p.Leaf.Script, p.Leaf.Control}
	return u, nil
}

// EnclaveWitness completes an enclave input spent by its transfer leaf.
func EnclaveWitness(policySig, userSig, leaf, control []byte) [][]byte {
	return [][]byte{policySig, userSig, leaf, control}
}

// ReverseHex turns a display hex id into internal byte order, and back.
func ReverseHex(h string) ([32]byte, error) {
	b, err := hex32(h)
	if err != nil {
		return b, err
	}
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

// Display is an internal-order id as display hex.
func Display(b [32]byte) string {
	r := b
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return hex.EncodeToString(r[:])
}

// ProgramOf returns the 32-byte program of a v1 script.
func ProgramOf(spk []byte) ([32]byte, error) {
	var p [32]byte
	if len(spk) != 34 || spk[0] != 0x51 || spk[1] != 0x20 {
		return p, fmt.Errorf("script %x is not a v1 program", spk)
	}
	copy(p[:], spk[2:])
	return p, nil
}

// SameScript reports whether two scripts are equal.
func SameScript(a, b []byte) bool { return bytes.Equal(a, b) }

func txFromHex(h string) (*transaction.Transaction, error) { return transaction.NewTxFromHex(h) }
