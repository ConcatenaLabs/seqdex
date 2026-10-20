package covenant

import (
	"encoding/hex"
	"fmt"
)

// Spec is a fill transaction in JSON, as seqob-covenant's filltx subcommand
// reads it and as testdata/fill_vectors.json carries it: each take's terms in
// the relay's CovenantTerms field names (bytes as hex), asset ids, programs
// and keys in internal byte order, txids in display order.
type Spec struct {
	Takes   []SpecTake    `json:"takes"`
	Credits []uint64      `json:"credits,omitempty"`
	Gaps    []*SpecOutput `json:"gaps"`
	Inputs  []Outpoint    `json:"inputs"`
	Tail    []SpecOutput  `json:"tail"`
}

// SpecTerms is an order's terms.
type SpecTerms struct {
	AssetA         string `json:"asset_a"`
	AssetB         string `json:"asset_b"`
	RateNum        uint64 `json:"rate_num"`
	RateDen        uint64 `json:"rate_den"`
	MakerProg      string `json:"maker_prog"`
	MakerProgVer   int    `json:"maker_prog_ver"`
	MinLot         uint64 `json:"min_lot"`
	ExpiryLocktime int64  `json:"expiry_locktime"`
	MakerX         string `json:"maker_x"`
	InternalKey    string `json:"internal_key,omitempty"`
}

// SpecTake is one order taken.
type SpecTake struct {
	Terms  SpecTerms `json:"terms"`
	Txid   string    `json:"txid"`
	Vout   uint32    `json:"vout"`
	Locked uint64    `json:"locked"`
	Filled uint64    `json:"filled"`
}

// SpecOutput is an explicit output.
type SpecOutput struct {
	Asset  string `json:"asset"`
	Value  uint64 `json:"value"`
	Script string `json:"script"`
}

func hex32(s, what string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("%s must be 32-byte hex (got %q)", what, s)
	}
	copy(out[:], b)
	return out, nil
}

// Order is the covenant.Order the terms describe (NUMS when internal_key is
// absent).
func (t SpecTerms) Order() (Order, error) {
	var o Order
	var err error
	if o.AssetA, err = hex32(t.AssetA, "asset_a"); err != nil {
		return o, err
	}
	if o.AssetB, err = hex32(t.AssetB, "asset_b"); err != nil {
		return o, err
	}
	if o.MakerProg, err = hex.DecodeString(t.MakerProg); err != nil || len(o.MakerProg) != 32 {
		return o, fmt.Errorf("maker_prog must be 32-byte hex")
	}
	if o.MakerX, err = hex32(t.MakerX, "maker_x"); err != nil {
		return o, err
	}
	o.InternalKey = NUMS
	if t.InternalKey != "" {
		if o.InternalKey, err = hex32(t.InternalKey, "internal_key"); err != nil {
			return o, err
		}
	}
	o.RateNum, o.RateDen, o.MinLot = t.RateNum, t.RateDen, t.MinLot
	o.MakerVer = t.MakerProgVer
	if o.MakerVer == 0 {
		o.MakerVer = 1
	}
	o.ExpiryLocktime = t.ExpiryLocktime
	return o, nil
}

// Output is the explicit output the spec describes.
func (s SpecOutput) Output() (Output, error) {
	a, err := hex32(s.Asset, "asset")
	if err != nil {
		return Output{}, err
	}
	spk, err := hex.DecodeString(s.Script)
	if err != nil {
		return Output{}, fmt.Errorf("script: %w", err)
	}
	return Explicit(a, s.Value, spk), nil
}

// FillTx is the FillTx the spec describes, for BuildFillTx.
func (s Spec) FillTx() (FillTx, error) {
	f := FillTx{Credits: s.Credits, Inputs: s.Inputs}
	for k, t := range s.Takes {
		o, err := t.Terms.Order()
		if err != nil {
			return f, fmt.Errorf("take %d: %w", k, err)
		}
		f.Takes = append(f.Takes, Take{Order: o, Txid: t.Txid, Vout: t.Vout, Locked: t.Locked, Filled: t.Filled})
	}
	for k, g := range s.Gaps {
		if g == nil {
			f.Gaps = append(f.Gaps, nil)
			continue
		}
		o, err := g.Output()
		if err != nil {
			return f, fmt.Errorf("gap %d: %w", k, err)
		}
		f.Gaps = append(f.Gaps, &o)
	}
	for i, t := range s.Tail {
		o, err := t.Output()
		if err != nil {
			return f, fmt.Errorf("tail %d: %w", i, err)
		}
		f.Tail = append(f.Tail, o)
	}
	return f, nil
}
