package watcher

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A resting order rests under one of two covenants: the tapscript FILL/REFUND
// order (CovEntry.Terms) or the Simplicity order of the template seqdex/order
// (CovEntry.Sim). The watcher reconciles both alike; these two read what
// differs: the program the order's outpoint must pay, and what a spend of it
// did.

// expect is the program the order's outpoint must pay, derived from its terms.
func (c CovEntry) expect() (Expect, error) {
	if c.Round != nil {
		o, err := offer.RoundOrderOf(c.Round)
		if err != nil {
			return Expect{}, err
		}
		spk, err := o.Script()
		if err != nil {
			return Expect{}, err
		}
		return Expect{OrderSPKHex: hex.EncodeToString(spk), AssetADisplay: reverseHex(c.Round.GetAssetA()), MinLot: o.MinLot}, nil
	}
	if c.Sim != nil {
		o, err := offer.SimOrderOf(c.Sim)
		if err != nil {
			return Expect{}, err
		}
		t, err := o.Derive()
		if err != nil {
			return Expect{}, err
		}
		return Expect{OrderSPKHex: hex.EncodeToString(t.ScriptPubKey), AssetADisplay: reverseHex(c.Sim.GetAssetA()), MinLot: o.MinLot}, nil
	}
	return expectFromTerms(c.Terms)
}

// readFill reads what sp did to the order.
func (c CovEntry) readFill(sp *SpenderTx, locked uint64) (FillReading, error) {
	if c.Round != nil {
		o, err := offer.RoundOrderOf(c.Round)
		if err != nil {
			return FillReading{}, err
		}
		return ReadRoundFill(o, sp, locked)
	}
	if c.Sim != nil {
		o, err := offer.SimOrderOf(c.Sim)
		if err != nil {
			return FillReading{}, err
		}
		return ReadSimFill(o, sp, locked)
	}
	order, err := orderFromTerms(c.Terms)
	if err != nil {
		return FillReading{}, err
	}
	return ReadFill(order, sp, locked)
}

// ReadSimFill reads what sp did to a Simplicity order it spent, which held
// locked atoms of asset A. The spend's witness says its path: a partial or
// whole fill, or anything else (the refund key taking the coin back), which is
// no fill. A partial fill's rest is at 2k+1; a whole fill's 2k+1, when present,
// is whatever the taker put there.
func ReadSimFill(o covenant.SimOrder, sp *SpenderTx, locked uint64) (FillReading, error) {
	r := FillReading{Input: sp.Input}
	t, err := o.Derive()
	if err != nil {
		return r, err
	}
	fill, partial := covenant.SimFillPath(sp.Witness)
	if !fill {
		r.Refund = true
		return r, nil
	}
	k := uint64(sp.Input)
	outs := sp.Outputs
	if 2*k >= uint64(len(outs)) {
		return r, fmt.Errorf("spender %s has no output %d to credit the maker of input %d", sp.Txid, 2*k, k)
	}
	r.Credit = fillOutput(uint32(2*k), outs[2*k])
	if 2*k+1 < uint64(len(outs)) {
		at := outs[2*k+1]
		fo := fillOutput(uint32(2*k+1), at)
		if partial && at.AssetExplicit && at.Asset == o.AssetA && bytes.Equal(at.Script, t.ScriptPubKey) {
			r.Rest, r.Remainder, r.Partial = &fo, at.Value, true
		} else {
			r.Gap = &fo
		}
	}
	if r.Remainder <= locked {
		r.Filled = locked - r.Remainder
	}
	if _, err := o.Judge(outs, sp.Input, locked, partial); err != nil {
		r.Disagrees = err.Error()
	}
	return r, nil
}

// ReadRoundFill reads what sp did to a round order it spent, which held locked
// atoms of asset A: a fill in a clearing round (the price its last output
// publishes, the credit at 2k, a partial fill's rest at 2k+1), or anything else
// (the refund key taking the coin back), which is no fill.
func ReadRoundFill(o covenant.RoundOrder, sp *SpenderTx, locked uint64) (FillReading, error) {
	r := FillReading{Input: sp.Input}
	spk, err := o.Script()
	if err != nil {
		return r, err
	}
	fill, partial := covenant.RoundOrderPath(sp.Witness)
	if !fill {
		r.Refund = true
		return r, nil
	}
	k := uint64(sp.Input)
	outs := sp.Outputs
	if 2*k >= uint64(len(outs)) {
		return r, fmt.Errorf("spender %s has no output %d to credit the maker of input %d", sp.Txid, 2*k, k)
	}
	r.Credit = fillOutput(uint32(2*k), outs[2*k])
	if 2*k+1 < uint64(len(outs)) {
		at := outs[2*k+1]
		fo := fillOutput(uint32(2*k+1), at)
		if partial && at.AssetExplicit && at.Asset == o.AssetA && bytes.Equal(at.Script, spk) {
			r.Rest, r.Remainder, r.Partial = &fo, at.Value, true
		} else {
			r.Gap = &fo
		}
	}
	if r.Remainder <= locked {
		r.Filled = locked - r.Remainder
	}
	if len(outs) > 0 {
		if p, ok := covenant.ReadClearing(outs[len(outs)-1].Script); ok {
			r.Clearing = &offerstore.RoundPrice{Base: displayOf(p.Base), Quote: displayOf(p.Quote), N: p.N, D: p.D}
		}
	}
	if _, err := o.CheckFill(covenant.Explicit(o.AssetA, locked, spk), outs, sp.Input, partial); err != nil {
		r.Disagrees = err.Error()
	}
	return r, nil
}
