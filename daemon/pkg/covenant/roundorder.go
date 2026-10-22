package covenant

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// RoundOrder is an order under the Simplicity template seqdex/round-order
// (sequentia-contracts, templates/seqdex_round_order): a maker's coin of
// AssetA that fills, in part or in whole, only in a clearing round its Clearer
// signs, at the one price the round publishes in its last output, and never
// below its limit; the refund key takes it back after Expiry.
//
// At input k, output 2k pays MakerScript exactly ceil(fill * n / d) of AssetB,
// n/d being the round's price as AssetB per AssetA; a partial fill leaves the
// rest at 2k+1 under the same script. The Clearer signs ClearingDigest of the
// whole transaction. Every path's pruned program is a constant of the vendored
// seqdex_round_order/fills.json (see seqdex_round_order/PIN.json).
type RoundOrder struct {
	AssetA, AssetB     [32]byte // sells AssetA, is paid in AssetB; internal byte order
	LimitNum, LimitDen uint64   // the least it accepts: LimitNum of AssetB per LimitDen of AssetA
	MinLot             uint64   // floors every fill and every remainder
	MakerScript        []byte   // the scriptPubKey every credit pays; the MAKER term is its SHA256
	Expiry             uint32   // refund lock: below 500,000,000 a height, else a median time
	RefundKey          [32]byte // x-only
	Clearer            [32]byte // x-only: the only key whose rounds fill the order
}

//go:embed seqdex_round_order/fills.json
var roundOrderFillsJSON []byte

var roundOrderTemplate = loadFills("seqdex_round_order", roundOrderFillsJSON,
	"fill", "fill_flip", "fill_whole", "fill_whole_flip", "refund_height", "refund_time")

// RoundOrderTemplateHash and RoundOrderCMR are the vendored template's.
var (
	RoundOrderTemplateHash = roundOrderTemplate.TemplateHash
	RoundOrderCMR          = roundOrderTemplate.CMR
)

// RoundOrderTermsLen is the data leaf's size.
const RoundOrderTermsLen = 188

// Terms is the data leaf: the terms in the template's order, big-endian.
func (o RoundOrder) Terms() []byte {
	b := make([]byte, 0, RoundOrderTermsLen)
	b = append(b, o.AssetA[:]...)
	b = append(b, o.AssetB[:]...)
	var n [8]byte
	for _, v := range []uint64{o.LimitNum, o.LimitDen, o.MinLot} {
		binary.BigEndian.PutUint64(n[:], v)
		b = append(b, n[:]...)
	}
	m := sha256.Sum256(o.MakerScript)
	b = append(b, m[:]...)
	binary.BigEndian.PutUint32(n[:4], o.Expiry)
	b = append(b, n[:4]...)
	b = append(b, o.RefundKey[:]...)
	return append(b, o.Clearer[:]...)
}

// Validate refuses terms no order should carry: a limit of zero (the order
// would accept any price, nothing included), a minimum lot of zero, one asset on
// both sides, no maker script, or a key no signature satisfies.
func (o RoundOrder) Validate() error {
	if o.LimitNum < 1 || o.LimitDen < 1 {
		return fmt.Errorf("limit %d/%d: numerator and denominator must be at least 1", o.LimitNum, o.LimitDen)
	}
	if o.MinLot < 1 {
		return fmt.Errorf("min_lot must be at least 1")
	}
	if o.AssetA == o.AssetB {
		return fmt.Errorf("the order sells and is paid in the same asset")
	}
	if len(o.MakerScript) == 0 || len(o.MakerScript) > 10000 {
		return fmt.Errorf("maker script of %d bytes", len(o.MakerScript))
	}
	if _, err := schnorr.ParsePubKey(o.RefundKey[:]); err != nil {
		return fmt.Errorf("refund key is not an x-only point: %w", err)
	}
	if _, err := schnorr.ParsePubKey(o.Clearer[:]); err != nil {
		return fmt.Errorf("clearer key is not an x-only point: %w", err)
	}
	return nil
}

// Derive computes the order's output from its terms and the pinned root.
func (o RoundOrder) Derive() (*SimTree, error) { return deriveDataTree(RoundOrderCMR, o.Terms()) }

// Script is the order's scriptPubKey.
func (o RoundOrder) Script() ([]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	return t.ScriptPubKey, nil
}

// Rate is the price p (N of Quote per D of Base) as this order's rate, AssetB
// per AssetA, and whether the order reads the clearing output flipped; ok is
// false when the price is not for this order's two assets.
func (o RoundOrder) Rate(p Price) (n, d uint64, flip, ok bool) {
	switch {
	case p.Base == o.AssetA && p.Quote == o.AssetB:
		return p.N, p.D, false, true
	case p.Base == o.AssetB && p.Quote == o.AssetA:
		return p.D, p.N, true, true
	}
	return 0, 0, false, false
}

// Accepts reports whether the order takes the price p: the rate is for its
// assets, d is at least one, and the rate is at least its limit.
func (o RoundOrder) Accepts(p Price) bool {
	n, d, _, ok := o.Rate(p)
	return ok && d >= 1 && mulLE(o.LimitNum, d, n, o.LimitDen)
}

// Credit is what a fill of filled atoms of AssetA is credited at p: exactly
// ceil(filled * n / d) of AssetB.
func (o RoundOrder) Credit(filled uint64, p Price) (uint64, error) {
	n, d, _, ok := o.Rate(p)
	if !ok {
		return 0, fmt.Errorf("the price is not for the order's assets")
	}
	return ceilMulDiv(filled, n, d)
}

// FillWitness is the order input's witness stack for a fill in the round
// priced p, given the clearer's BIP340 signature over the round's
// ClearingDigest.
func (o RoundOrder) FillWitness(partial bool, p Price, sig [64]byte) ([][]byte, error) {
	_, _, flip, ok := o.Rate(p)
	if !ok {
		return nil, fmt.Errorf("the price is not for the order's assets")
	}
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	w := &bitWriter{}
	w.bytes(t.Terms)
	w.bit(false)
	w.bit(partial)
	w.bit(flip)
	w.u64(p.N)
	w.u64(p.D)
	w.bytes(sig[:])
	path := "fill"
	if !partial {
		path = "fill_whole"
	}
	if flip {
		path += "_flip"
	}
	return [][]byte{w.b, roundOrderTemplate.Programs[path], RoundOrderCMR[:], t.ControlBlock}, nil
}

// RefundWitness is the order input's witness stack for the refund, given the
// refund key's BIP340 signature over the input's SimSigAllHash. The
// transaction's lock time must be of Expiry's kind and at least Expiry, with a
// non-final sequence.
func (o RoundOrder) RefundWitness(sig [64]byte) ([][]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	w := &bitWriter{}
	w.bytes(t.Terms)
	w.bit(true)
	w.bytes(sig[:])
	path := "refund_height"
	if o.Expiry >= LocktimeThreshold {
		path = "refund_time"
	}
	return [][]byte{w.b, roundOrderTemplate.Programs[path], RoundOrderCMR[:], t.ControlBlock}, nil
}

// RoundOrderPath reads the path of a spend from its witness stack: whether it
// is a fill, whether a partial one, and the price it read. A stack that is not
// this template's is no fill.
func RoundOrderPath(stack [][]byte) (fill, partial bool) {
	if len(stack) < 4 || !bytes.Equal(stack[2], RoundOrderCMR[:]) || len(stack[0]) <= RoundOrderTermsLen {
		return false, false
	}
	for path, part := range map[string]bool{"fill": true, "fill_flip": true, "fill_whole": false, "fill_whole_flip": false} {
		if bytes.Equal(stack[1], roundOrderTemplate.Programs[path]) {
			return true, part
		}
	}
	return false, false
}

// RoundFill is a fill of a round order as its program reads it.
type RoundFill struct {
	K         uint32
	Partial   bool
	Filled    uint64
	Remainder uint64
	Credited  uint64
	Price     Price
}

// CheckFill judges outs as the order's program does for a fill at input k of
// coin, read from the chain, partial being the PATH the spend chose. The
// round's price is read from the last output, as the program reads it. It
// checks everything but the clearer's signature (CheckClearerSig) and returns
// nil error exactly when the program would accept the rest.
func (o RoundOrder) CheckFill(coin Output, outs []Output, k uint32, partial bool) (*RoundFill, error) {
	spk, err := o.Script()
	if err != nil {
		return nil, err
	}
	if !coin.AssetExplicit || !coin.ValueExplicit || coin.Asset != o.AssetA || !bytes.Equal(coin.Script, spk) {
		return nil, fmt.Errorf("the coin is not the order's: explicit %x at the order's script", o.AssetA)
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("no outputs")
	}
	last := outs[len(outs)-1]
	p, ok := ReadClearing(last.Script)
	if !ok {
		return nil, fmt.Errorf("the last output (%d) is not a clearing output", len(outs)-1)
	}
	n, d, _, ok := o.Rate(p)
	if !ok {
		return nil, fmt.Errorf("the clearing output prices another pair than the order's")
	}
	if d == 0 {
		return nil, fmt.Errorf("the price's divisor is zero")
	}
	if !mulLE(o.LimitNum, d, n, o.LimitDen) {
		return nil, fmt.Errorf("the round's price %d/%d is below the order's limit %d/%d", n, d, o.LimitNum, o.LimitDen)
	}
	f := &RoundFill{K: k, Partial: partial, Price: p}
	ci, ri := 2*uint64(k), 2*uint64(k)+1
	if partial {
		if ri >= uint64(len(outs)) {
			return nil, fmt.Errorf("no output %d for the remainder", ri)
		}
		r := outs[ri]
		switch {
		case !r.AssetExplicit || !r.ValueExplicit:
			return nil, fmt.Errorf("output %d (the remainder) is confidential", ri)
		case r.Asset != o.AssetA:
			return nil, fmt.Errorf("output %d (the remainder) holds another asset", ri)
		case !bytes.Equal(r.Script, spk):
			return nil, fmt.Errorf("output %d (the remainder) does not pay the order's own script", ri)
		case r.HasNonce:
			return nil, fmt.Errorf("output %d (the remainder) carries a nonce", ri)
		case r.Value < o.MinLot:
			return nil, fmt.Errorf("output %d rests %d, below min_lot %d", ri, r.Value, o.MinLot)
		case r.Value > coin.Value:
			return nil, fmt.Errorf("output %d rests %d, more than the coin's %d", ri, r.Value, coin.Value)
		}
		f.Remainder = r.Value
	}
	f.Filled = coin.Value - f.Remainder
	if f.Filled < o.MinLot || f.Filled == 0 {
		return nil, fmt.Errorf("the fill takes %d, below min_lot %d", f.Filled, o.MinLot)
	}
	if ci >= uint64(len(outs)) {
		return nil, fmt.Errorf("no output %d to credit the maker", ci)
	}
	c := outs[ci]
	switch {
	case !c.AssetExplicit || !c.ValueExplicit:
		return nil, fmt.Errorf("output %d (the credit) is confidential", ci)
	case c.Asset != o.AssetB:
		return nil, fmt.Errorf("output %d (the credit) holds another asset", ci)
	case !bytes.Equal(c.Script, o.MakerScript):
		return nil, fmt.Errorf("output %d (the credit) does not pay the maker's script", ci)
	case c.HasNonce:
		return nil, fmt.Errorf("output %d (the credit) carries a nonce", ci)
	}
	want, err := ceilMulDiv(f.Filled, n, d)
	if err != nil {
		return nil, err
	}
	if c.Value != want {
		return nil, fmt.Errorf("output %d credits %d, not the %d the published price %d/%d gives", ci, c.Value, want, n, d)
	}
	f.Credited = c.Value
	return f, nil
}

// CheckClearerSig verifies the clearer's signature on a round.
func (o RoundOrder) CheckClearerSig(digest [32]byte, sig [64]byte) error {
	pk, err := schnorr.ParsePubKey(o.Clearer[:])
	if err != nil {
		return err
	}
	s, err := schnorr.ParseSignature(sig[:])
	if err != nil {
		return err
	}
	if !s.Verify(digest[:], pk) {
		return fmt.Errorf("the round is not signed by the order's clearer")
	}
	return nil
}
