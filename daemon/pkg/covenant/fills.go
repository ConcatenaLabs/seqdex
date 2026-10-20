package covenant

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// A transaction may fill several covenant orders at once: a taker taking two
// books in one spend, a settler crossing two orders, or an Arca operator's
// round filling every order its takers asked for. Each order's FILL leaf reads
// only the outputs bound to its own input index k: the maker's credit at 2k
// and, at 2k+1, the order resting again under its identical script after a
// partial fill, or, after a full fill, an explicit output in any asset but the
// one the order sells (a "gap"). So the orders take inputs 0..n-1 and outputs
// 0..2n-1, and everything else in the transaction follows them.
//
// This file plans such a transaction (PlanFills, BuildFillTx) and judges one
// the way each order's leaf will (CheckFill), so a builder fails closed on a
// layout the chain would refuse and a reader can tell what a confirmed fill
// paid without trusting whoever built it.

// Take is one covenant order taken in a transaction that may take others. Txid
// (display hex) and Vout locate the order's output; Locked is what it holds of
// asset A and Filled what this fill takes of it.
type Take struct {
	Order  Order
	Txid   string
	Vout   uint32
	Locked uint64
	Filled uint64
}

// PlanFills plans takes spent together, take k at input k: its credit at
// output 2k and its remainder (or, for a full fill, a gap) at 2k+1. It refuses
// an order taken twice.
func PlanFills(takes []Take) ([]*FillPlan, error) {
	if len(takes) == 0 {
		return nil, fmt.Errorf("no order to fill")
	}
	seen := map[string]bool{}
	plans := make([]*FillPlan, len(takes))
	for k, t := range takes {
		op := fmt.Sprintf("%s:%d", t.Txid, t.Vout)
		if t.Txid != "" && seen[op] {
			return nil, fmt.Errorf("take %d: order %s is taken twice", k, op)
		}
		seen[op] = true
		p, err := t.Order.PlanFill(t.Locked, t.Filled, uint32(k))
		if err != nil {
			return nil, fmt.Errorf("take %d: %w", k, err)
		}
		plans[k] = p
	}
	return plans, nil
}

// Output is what a FILL leaf can read of one transaction output: its asset and
// its value when they are explicit, and its scriptPubKey. Asset ids are in
// internal byte order, as introspection returns them.
type Output struct {
	Asset         [32]byte
	AssetExplicit bool
	Value         uint64
	ValueExplicit bool
	Script        []byte
}

// Explicit is an output with an explicit asset and value.
func Explicit(asset [32]byte, value uint64, script []byte) Output {
	return Output{Asset: asset, AssetExplicit: true, Value: value, ValueExplicit: true, Script: script}
}

// OutputsOf reads a transaction's outputs as a FILL leaf reads them. A
// confidential asset or value is marked as not explicit.
func OutputsOf(tx *transaction.Transaction) []Output {
	outs := make([]Output, len(tx.Outputs))
	for i, o := range tx.Outputs {
		var out Output
		if len(o.Asset) == 33 && o.Asset[0] == 0x01 {
			copy(out.Asset[:], o.Asset[1:])
			out.AssetExplicit = true
		}
		if v, err := elementsutil.ValueFromBytes(o.Value); err == nil {
			out.Value = v
			out.ValueExplicit = true
		}
		out.Script = o.Script
		outs[i] = out
	}
	return outs
}

// Checked is what an order's FILL leaf read of a transaction it accepts.
type Checked struct {
	InputIndex     uint32
	CreditIndex    uint32
	RemainderIndex uint32
	Filled         uint64 // atoms of asset A taken: locked less the remainder
	Remainder      uint64 // atoms of asset A resting again at 2k+1 (0 for a full fill)
	Partial        bool
	Required       uint64 // ceil(filled * rate_num / rate_den)
	Credited       uint64 // what output 2k pays the maker (>= Required)
}

// CheckFill judges outs as the FILL leaf of this order spent at input k, the
// order's output holding locked atoms of asset A. It returns nil error exactly
// when the leaf's script succeeds, and otherwise names the step that fails,
// in the leaf's own order:
//
//   - at 2k+1, when that output exists: its asset must be explicit; in asset A
//     it is the remainder, which must pay the order's own script, show its
//     value and hold at least min_lot; in any other asset the fill is full;
//   - the fill (locked less the remainder) must be at least min_lot, and
//     filled*rate_num + rate_den - 1 must fit the leaf's signed 64 bits;
//   - output 2k must exist, hold asset B explicitly, pay OP_1 <maker_prog>,
//     and show a value of at least the ceiling price.
func (o Order) CheckFill(outs []Output, k uint32, locked uint64) (*Checked, error) {
	tap, err := o.Derive()
	if err != nil {
		return nil, err
	}
	ci, ri := 2*uint64(k), 2*uint64(k)+1
	c := &Checked{InputIndex: k, CreditIndex: uint32(ci), RemainderIndex: uint32(ri)}
	if ri < uint64(len(outs)) {
		r := outs[ri]
		if !r.AssetExplicit {
			return nil, fmt.Errorf("output %d (2k+1 for input %d) hides its asset; the FILL leaf reads it explicit", ri, k)
		}
		if r.Asset == o.AssetA {
			if !bytes.Equal(r.Script, tap.ScriptPubKey) {
				return nil, fmt.Errorf("output %d holds the asset the order sells but does not pay the order's own script, so the leaf reads it as a remainder that does not rest again", ri)
			}
			if !r.ValueExplicit {
				return nil, fmt.Errorf("output %d rests the order again but hides its value", ri)
			}
			if r.Value < o.MinLot {
				return nil, fmt.Errorf("output %d rests %d atoms again, below min_lot %d", ri, r.Value, o.MinLot)
			}
			c.Remainder, c.Partial = r.Value, true
		}
	}
	if c.Remainder > locked || locked-c.Remainder < o.MinLot {
		return nil, fmt.Errorf("the fill takes %d of %d atoms (remainder %d), below min_lot %d", int64(locked)-int64(c.Remainder), locked, c.Remainder, o.MinLot)
	}
	c.Filled = locked - c.Remainder
	hi, lo := bits.Mul64(c.Filled, o.RateNum)
	lo, carry := bits.Add64(lo, o.RateDen-1, 0)
	if hi+carry != 0 || lo > MaxLeafProduct {
		return nil, fmt.Errorf("filled %d * rate_num %d overflows the leaf's signed 64-bit arithmetic", c.Filled, o.RateNum)
	}
	c.Required = CeilPrice(c.Filled, o.RateNum, o.RateDen)
	if ci >= uint64(len(outs)) {
		return nil, fmt.Errorf("no output %d (2k for input %d) to credit the maker", ci, k)
	}
	cr := outs[ci]
	if !cr.AssetExplicit || cr.Asset != o.AssetB {
		return nil, fmt.Errorf("output %d does not hold the asset the order is paid in explicitly", ci)
	}
	if want := append([]byte{op1, 0x20}, o.MakerProg...); o.MakerVer != 1 || !bytes.Equal(cr.Script, want) {
		return nil, fmt.Errorf("output %d does not pay the maker's program", ci)
	}
	if !cr.ValueExplicit {
		return nil, fmt.Errorf("output %d hides the maker's credit", ci)
	}
	if cr.Value < c.Required {
		return nil, fmt.Errorf("output %d credits the maker %d atoms, below the ceiling price %d", ci, cr.Value, c.Required)
	}
	c.Credited = cr.Value
	return c, nil
}

// Outpoint is an input the fill transaction spends besides the orders: the
// taker's own coins, an operator's, a settler's. Txid is display hex.
type Outpoint struct {
	Txid string `json:"txid"`
	Vout uint32 `json:"vout"`
}

// FillTx is a transaction filling Takes at inputs 0..n-1.
type FillTx struct {
	Takes []Take
	// Credits, when given, is each take's credit to its maker (at least the
	// ceiling price; a builder may fold its own dust into it). 0 = the price.
	Credits []uint64
	// Gaps holds, for each FULL fill, the explicit output that stands at its
	// 2k+1: any asset but the one the order sells. Entries for partial fills
	// are ignored (the order rests again there).
	Gaps []*Output
	// Inputs follow the orders, in order.
	Inputs []Outpoint
	// Tail follows the orders' 2n outputs, in order.
	Tail []Output
	// Sequence of every input (0 = 0xffffffff).
	Sequence uint32
}

// BuildFillTx lays out the fill transaction: version 2, lock time 0, the
// orders at inputs 0..n-1 each with its FILL witness [leaf, control block],
// then Inputs unsigned; outputs credit_k at 2k, rest_k or gap_k at 2k+1, then
// Tail. Every order's leaf is run over the result (CheckFill) before it is
// returned, so a layout the chain would refuse is an error here.
func BuildFillTx(f FillTx) (*transaction.Transaction, []*FillPlan, error) {
	plans, err := PlanFills(f.Takes)
	if err != nil {
		return nil, nil, err
	}
	if len(f.Credits) != 0 && len(f.Credits) != len(f.Takes) {
		return nil, nil, fmt.Errorf("%d credits for %d takes", len(f.Credits), len(f.Takes))
	}
	if len(f.Gaps) != 0 && len(f.Gaps) != len(f.Takes) {
		return nil, nil, fmt.Errorf("%d gaps for %d takes", len(f.Gaps), len(f.Takes))
	}
	seq := f.Sequence
	if seq == 0 {
		seq = transaction.DefaultSequence
	}
	tx := transaction.NewTx(2)
	var outs []Output
	for k, t := range f.Takes {
		p := plans[k]
		if t.Txid == "" {
			return nil, nil, fmt.Errorf("take %d names no order outpoint", k)
		}
		h, err := elementsutil.TxIDToBytes(t.Txid)
		if err != nil || len(h) != 32 {
			return nil, nil, fmt.Errorf("take %d: order txid %q", k, t.Txid)
		}
		in := transaction.NewTxInput(h, t.Vout)
		in.Sequence = seq
		in.Witness = p.Witness()
		tx.AddInput(in)

		credit := p.RequiredB
		if len(f.Credits) != 0 && f.Credits[k] != 0 {
			credit = f.Credits[k]
		}
		if credit < p.RequiredB {
			return nil, nil, fmt.Errorf("take %d: credit %d below the ceiling price %d", k, credit, p.RequiredB)
		}
		makerSPK, err := makerCreditSPK(t.Order)
		if err != nil {
			return nil, nil, fmt.Errorf("take %d: %w", k, err)
		}
		outs = append(outs, Explicit(t.Order.AssetB, credit, makerSPK))
		if p.Partial {
			outs = append(outs, Explicit(t.Order.AssetA, p.Remainder, p.OrderSPK))
			continue
		}
		var gap *Output
		if len(f.Gaps) != 0 {
			gap = f.Gaps[k]
		}
		if gap == nil {
			return nil, nil, fmt.Errorf("take %d fills its order whole and has no gap output for 2k+1 (%d)", k, p.RemainderIndex)
		}
		if !gap.AssetExplicit || !gap.ValueExplicit {
			return nil, nil, fmt.Errorf("take %d: the gap at %d must be explicit", k, p.RemainderIndex)
		}
		if gap.Asset == t.Order.AssetA {
			return nil, nil, fmt.Errorf("take %d: the gap at %d holds the asset the order sells, which the leaf reads as a remainder", k, p.RemainderIndex)
		}
		outs = append(outs, *gap)
	}
	for _, in := range f.Inputs {
		h, err := elementsutil.TxIDToBytes(in.Txid)
		if err != nil || len(h) != 32 {
			return nil, nil, fmt.Errorf("input txid %q", in.Txid)
		}
		ti := transaction.NewTxInput(h, in.Vout)
		ti.Sequence = seq
		tx.AddInput(ti)
	}
	outs = append(outs, f.Tail...)
	for i, o := range outs {
		if !o.AssetExplicit || !o.ValueExplicit {
			return nil, nil, fmt.Errorf("output %d is not explicit; a fill transaction's outputs are", i)
		}
		v, err := elementsutil.ValueToBytes(o.Value)
		if err != nil {
			return nil, nil, err
		}
		asset := append([]byte{0x01}, o.Asset[:]...)
		tx.AddOutput(transaction.NewTxOutput(asset, v, o.Script))
	}
	for k, t := range f.Takes {
		if _, err := t.Order.CheckFill(outs, uint32(k), t.Locked); err != nil {
			return nil, nil, fmt.Errorf("take %d: the order's FILL leaf would refuse this transaction: %w", k, err)
		}
	}
	return tx, plans, nil
}
