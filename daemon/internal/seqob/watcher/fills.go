package watcher

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// FillReading is what a transaction that spent a covenant order did to it,
// read from its outputs as the order's leaf reads them: the order at input k,
// its maker's credit at 2k, and at 2k+1 the order resting again or, after a
// full fill, whatever stands there.
type FillReading struct {
	// Refund: the order's input was not spent through its FILL leaf (its maker
	// took the coins back through REFUND, or a key path): no fill, no trade.
	Refund    bool
	Input     uint32
	Filled    uint64
	Remainder uint64
	Partial   bool
	Credit    offerstore.FillOutput
	Rest      *offerstore.FillOutput // 2k+1 resting the order again
	Gap       *offerstore.FillOutput // 2k+1 after a full fill, when present
	// Disagrees is set when the order's leaf, as covenant.CheckFill reads it,
	// would refuse what the chain took. The chain is right; the reading then
	// comes from the outputs alone.
	Disagrees string
	// Clearing is the price a clearing round published in its last output, for
	// a round order filled in one.
	Clearing *offerstore.RoundPrice
}

// ReadFill reads what sp did to the order it spent, which held locked atoms of
// asset A. It is pure: the chain view gathers sp.
func ReadFill(o covenant.Order, sp *SpenderTx, locked uint64) (FillReading, error) {
	r := FillReading{Input: sp.Input}
	tap, err := o.Derive()
	if err != nil {
		return r, err
	}
	// A script-path spend carries the leaf it runs second from the end of the
	// witness, before its control block. FILL's witness is [leaf, control].
	w := sp.Witness
	if len(w) < 2 || !bytes.Equal(w[len(w)-2], tap.FillLeaf) {
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
		if at.AssetExplicit && at.Asset == o.AssetA && bytes.Equal(at.Script, tap.ScriptPubKey) {
			r.Rest, r.Remainder, r.Partial = &fo, at.Value, true
		} else {
			r.Gap = &fo
		}
	}
	if r.Remainder <= locked {
		r.Filled = locked - r.Remainder
	}
	if _, err := o.CheckFill(outs, sp.Input, locked); err != nil {
		r.Disagrees = err.Error()
	}
	return r, nil
}

func fillOutput(vout uint32, o covenant.Output) offerstore.FillOutput {
	fo := offerstore.FillOutput{Vout: vout, Script: hex.EncodeToString(o.Script)}
	if o.AssetExplicit {
		fo.Asset = displayOf(o.Asset)
	}
	if o.ValueExplicit {
		fo.Value = o.Value
	}
	return fo
}

func displayOf(internal [32]byte) string {
	b := make([]byte, 32)
	for i := 0; i < 32; i++ {
		b[i] = internal[31-i]
	}
	return hex.EncodeToString(b)
}
