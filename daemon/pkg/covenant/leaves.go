package covenant

import (
	"bytes"
	"fmt"
)

// Leaf is the covenant a resting order rests under, as a fill sees it: the
// tapscript FILL/REFUND order (Order) or the Simplicity order (SimOrder). Both
// bind the order spent at input k to its maker's credit at 2k and its rest at
// 2k+1, so a planner, a taker and a reader treat them alike through this.
type Leaf interface {
	// Sells is asset A, the asset the order holds; PaysIn is asset B, the asset
	// a fill pays in. Internal byte order.
	Sells() [32]byte
	PaysIn() [32]byte
	// Lot is the order's min_lot; Rate its price, RateNum atoms of B for every
	// RateDen atoms of A.
	Lot() uint64
	Rate() (num, den uint64)
	// OrderScript is the order's own scriptPubKey, derived from its terms;
	// CreditScript the scriptPubKey every fill pays.
	OrderScript() ([]byte, error)
	CreditScript() ([]byte, error)
	// VerifyScript refuses a coin whose scriptPubKey is not the one the terms
	// make (and, for the tapscript order, a maker-cancellable internal key
	// unless allowed).
	VerifyScript(spk []byte, allowMakerCancellable bool) error
	// PlanFill is the recipe for filling filled of locked atoms at input k.
	PlanFill(locked, filled uint64, k uint32) (*FillPlan, error)
	// Judge reads outs as the order's leaf does for a fill at input k of a coin
	// holding locked atoms of A, the fill partial or whole as planned.
	Judge(outs []Output, k uint32, locked uint64, partial bool) (*Checked, error)
	// WholeFillReadsRest is true when a whole fill's 2k+1 must not hold asset
	// A (the tapscript leaf reads an asset-A output there as a remainder);
	// false when the leaf leaves 2k+1 alone in a whole fill.
	WholeFillReadsRest() bool
}

// Order as a Leaf.
func (o Order) Sells() [32]byte          { return o.AssetA }
func (o Order) PaysIn() [32]byte         { return o.AssetB }
func (o Order) Lot() uint64              { return o.MinLot }
func (o Order) Rate() (uint64, uint64)   { return o.RateNum, o.RateDen }
func (o Order) WholeFillReadsRest() bool { return true }

func (o Order) OrderScript() ([]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	return t.ScriptPubKey, nil
}

func (o Order) CreditScript() ([]byte, error) { return makerCreditSPK(o) }

func (o Order) VerifyScript(spk []byte, allowMakerCancellable bool) error {
	_, err := o.VerifyAgainstSPK(spk, allowMakerCancellable)
	return err
}

// Judge is CheckFill; the tapscript leaf reads the path from 2k+1 itself.
func (o Order) Judge(outs []Output, k uint32, locked uint64, _ bool) (*Checked, error) {
	return o.CheckFill(outs, k, locked)
}

// SimOrder as a Leaf.
func (o SimOrder) Sells() [32]byte          { return o.AssetA }
func (o SimOrder) PaysIn() [32]byte         { return o.AssetB }
func (o SimOrder) Lot() uint64              { return o.MinLot }
func (o SimOrder) Rate() (uint64, uint64)   { return o.RateNum, o.RateDen }
func (o SimOrder) WholeFillReadsRest() bool { return false }

func (o SimOrder) OrderScript() ([]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	return t.ScriptPubKey, nil
}

func (o SimOrder) CreditScript() ([]byte, error) {
	if len(o.MakerScript) == 0 {
		return nil, fmt.Errorf("no maker script")
	}
	return append([]byte{}, o.MakerScript...), nil
}

// VerifyScript: the Simplicity order has no key path to allow (its internal key
// is the template's NUMS key, which Derive uses).
func (o SimOrder) VerifyScript(spk []byte, _ bool) error {
	if err := o.Validate(); err != nil {
		return err
	}
	want, err := o.OrderScript()
	if err != nil {
		return err
	}
	if !bytes.Equal(want, spk) {
		return fmt.Errorf("reconstructed spk %x != on-chain spk %x", want, spk)
	}
	return nil
}

// Judge is CheckFill on a coin at the order's own script holding locked atoms
// of A explicitly: the caller has read the coin from the chain.
func (o SimOrder) Judge(outs []Output, k uint32, locked uint64, partial bool) (*Checked, error) {
	spk, err := o.OrderScript()
	if err != nil {
		return nil, err
	}
	return o.CheckFill(Explicit(o.AssetA, locked, spk), outs, k, partial)
}

var (
	_ Leaf = Order{}
	_ Leaf = SimOrder{}
)
