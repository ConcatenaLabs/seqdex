package covenant

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// SimPool is a maker's two-sided pool under the Simplicity template
// seqdex/pool (sequentia-contracts, templates/seqdex_pool): two coins at one
// address, one of Base and one of Quote, which anyone fills from either side at
// the pool's quotes and which only MakerKey withdraws.
//
//	P2TR(NUMS, TapBranch(TapLeaf_0xbe(program), H_TapData(terms)))
//
// The Base coin at input j judges every move: input j+1 is the pool's Quote
// coin, and outputs 2j and 2j+1 are the pool again. A sell pays out db of
// Base for at least ceil(db * AskNum / AskDen) of Quote, keeping ReserveBase;
// a buy takes in db of Base for at most floor(db * BidNum / BidDen) of Quote,
// keeping ReserveQuote; db is at least MinLot and one atom. Products are
// compared at 128 bits. Every path's pruned program is a constant of the
// vendored seqdex_pool/fills.json (see seqdex_pool/PIN.json).
type SimPool struct {
	Base, Quote               [32]byte // internal byte order
	BidNum, BidDen            uint64   // the pool pays at most BidNum of Quote per BidDen of Base
	AskNum, AskDen            uint64   // the pool asks at least AskNum of Quote per AskDen of Base
	MinLot                    uint64   // the least Base a fill moves
	ReserveBase, ReserveQuote uint64   // what a fill paying that asset out must leave
	MakerKey                  [32]byte // x-only: the only key that withdraws
}

//go:embed seqdex_pool/fills.json
var poolFillsJSON []byte

var poolTemplate = loadFills("seqdex_pool", poolFillsJSON, "sell", "buy", "pair", "withdraw")

// PoolTemplateHash and PoolCMR are the vendored template's.
var (
	PoolTemplateHash = poolTemplate.TemplateHash
	PoolCMR          = poolTemplate.CMR
)

// PoolTermsLen is the data leaf's size.
const PoolTermsLen = 152

// Terms is the data leaf: the terms in the template's order, big-endian.
func (p SimPool) Terms() []byte {
	b := make([]byte, 0, PoolTermsLen)
	b = append(b, p.Base[:]...)
	b = append(b, p.Quote[:]...)
	var n [8]byte
	for _, v := range []uint64{p.BidNum, p.BidDen, p.AskNum, p.AskDen, p.MinLot, p.ReserveBase, p.ReserveQuote} {
		binary.BigEndian.PutUint64(n[:], v)
		b = append(b, n[:]...)
	}
	return append(b, p.MakerKey[:]...)
}

// PoolFromTerms reads a pool's data leaf.
func PoolFromTerms(t []byte) (SimPool, error) {
	var p SimPool
	if len(t) != PoolTermsLen {
		return p, fmt.Errorf("pool terms of %d bytes, not %d", len(t), PoolTermsLen)
	}
	copy(p.Base[:], t[0:32])
	copy(p.Quote[:], t[32:64])
	u := func(i int) uint64 { return binary.BigEndian.Uint64(t[64+8*i : 72+8*i]) }
	p.BidNum, p.BidDen, p.AskNum, p.AskDen, p.MinLot, p.ReserveBase, p.ReserveQuote = u(0), u(1), u(2), u(3), u(4), u(5), u(6)
	copy(p.MakerKey[:], t[120:152])
	return p, nil
}

// Validate refuses terms no pool should carry. The program takes any terms;
// these protect the maker: a quote that cannot be met or costs nothing, a bid
// above the ask (anyone could buy from the pool and sell back to it at a
// profit until both sides reach their reserves), one asset on both sides, a
// reserve of zero (a side could be emptied to a coin of no value), and a key no
// signature satisfies (the pool could never be withdrawn).
func (p SimPool) Validate() error {
	if p.BidNum < 1 || p.BidDen < 1 || p.AskNum < 1 || p.AskDen < 1 {
		return fmt.Errorf("quotes %d/%d and %d/%d: every term must be at least 1", p.BidNum, p.BidDen, p.AskNum, p.AskDen)
	}
	if !mulLE(p.BidNum, p.AskDen, p.AskNum, p.BidDen) {
		return fmt.Errorf("the bid %d/%d is above the ask %d/%d", p.BidNum, p.BidDen, p.AskNum, p.AskDen)
	}
	if p.MinLot < 1 {
		return fmt.Errorf("min_lot must be at least 1")
	}
	if p.ReserveBase < 1 || p.ReserveQuote < 1 {
		return fmt.Errorf("each reserve must be at least 1")
	}
	if p.Base == p.Quote {
		return fmt.Errorf("the pool holds one asset on both sides")
	}
	if _, err := schnorr.ParsePubKey(p.MakerKey[:]); err != nil {
		return fmt.Errorf("maker key is not an x-only point: %w", err)
	}
	return nil
}

// Derive computes the pool's output from its terms and the pinned root.
func (p SimPool) Derive() (*SimTree, error) { return deriveDataTree(PoolCMR, p.Terms()) }

// Script is the pool's scriptPubKey.
func (p SimPool) Script() ([]byte, error) {
	t, err := p.Derive()
	if err != nil {
		return nil, err
	}
	return t.ScriptPubKey, nil
}

func (p SimPool) stack(path string, w []byte) ([][]byte, error) {
	t, err := p.Derive()
	if err != nil {
		return nil, err
	}
	return [][]byte{w, poolTemplate.Programs[path], PoolCMR[:], t.ControlBlock}, nil
}

// FillWitness is the Base coin's witness stack for a fill: sell is true when the
// pool pays Base out.
func (p SimPool) FillWitness(sell bool) ([][]byte, error) {
	w := &bitWriter{}
	w.bytes(p.Terms())
	w.bit(false)
	w.bit(sell)
	if sell {
		return p.stack("sell", w.b)
	}
	return p.stack("buy", w.b)
}

// PairWitness is the Quote coin's witness stack beside a fill or a withdrawal.
func (p SimPool) PairWitness() ([][]byte, error) {
	w := &bitWriter{}
	w.bytes(p.Terms())
	w.bit(true)
	w.bit(false)
	return p.stack("pair", w.b)
}

// WithdrawWitness is a coin's witness stack for the maker's withdrawal, given
// MakerKey's BIP340 signature over the input's SimSigAllHash.
func (p SimPool) WithdrawWitness(sig [64]byte) ([][]byte, error) {
	w := &bitWriter{}
	w.bytes(p.Terms())
	w.bit(true)
	w.bit(true)
	w.bytes(sig[:])
	return p.stack("withdraw", w.b)
}

// PoolPath reads which path a pool coin's witness stack takes: "sell", "buy",
// "pair", "withdraw", or "" for a stack that is not this template's.
func PoolPath(stack [][]byte) string {
	if len(stack) < 4 || !bytes.Equal(stack[2], PoolCMR[:]) || len(stack[0]) < PoolTermsLen+1 {
		return ""
	}
	for _, path := range []string{"sell", "buy", "pair", "withdraw"} {
		if bytes.Equal(stack[1], poolTemplate.Programs[path]) {
			return path
		}
	}
	return ""
}

// AskQuote is the least Quote the pool takes in for paying out db of Base.
func (p SimPool) AskQuote(db uint64) (uint64, error) { return ceilMulDiv(db, p.AskNum, p.AskDen) }

// BidQuote is the most Quote the pool pays out for taking in db of Base.
func (p SimPool) BidQuote(db uint64) (uint64, error) { return floorMulDiv(db, p.BidNum, p.BidDen) }

// Depth is what the pool lists, given what its coins hold: the Base it sells
// at its ask (x above ReserveBase), and the Quote it pays at its bid (y above
// ReserveQuote). A pool lists only what it holds.
func (p SimPool) Depth(x, y uint64) (askBase, bidQuote uint64) {
	if x > p.ReserveBase {
		askBase = x - p.ReserveBase
	}
	if y > p.ReserveQuote {
		bidQuote = y - p.ReserveQuote
	}
	return askBase, bidQuote
}

// PoolMove is a fill of a pool as its program reads it.
type PoolMove struct {
	J           uint32 // the Base coin's input; the Quote coin is j+1
	Sell        bool   // the pool paid Base out
	X0, Y0      uint64 // what the coins held
	X1, Y1      uint64 // what the pool holds again at 2j and 2j+1
	Base, Quote uint64 // db and dq: the Base and Quote that moved
}

// CheckFill judges outs as the pool's program does for a fill whose Base coin
// xc is at input j and Quote coin yc at input j+1, both read from the chain;
// sell is the PATH the spend chose. It returns nil error exactly when the
// program accepts, and otherwise names the rule that fails.
func (p SimPool) CheckFill(xc, yc Output, outs []Output, j uint32, sell bool) (*PoolMove, error) {
	spk, err := p.Script()
	if err != nil {
		return nil, err
	}
	if !xc.AssetExplicit || !xc.ValueExplicit || xc.Asset != p.Base || !bytes.Equal(xc.Script, spk) {
		return nil, fmt.Errorf("input %d is not the pool's base coin: explicit %x at the pool's script", j, p.Base)
	}
	if !yc.AssetExplicit || !yc.ValueExplicit || yc.Asset != p.Quote || !bytes.Equal(yc.Script, spk) {
		return nil, fmt.Errorf("input %d is not the pool's quote coin: explicit %x at the pool's script", j+1, p.Quote)
	}
	m := &PoolMove{J: j, Sell: sell, X0: xc.Value, Y0: yc.Value}
	at := func(i uint64, asset [32]byte, what string) (uint64, error) {
		if i >= uint64(len(outs)) {
			return 0, fmt.Errorf("no output %d for the pool's %s", i, what)
		}
		o := outs[i]
		switch {
		case !o.AssetExplicit || !o.ValueExplicit:
			return 0, fmt.Errorf("output %d (the pool's %s) is confidential; the program reads it explicit", i, what)
		case o.Asset != asset:
			return 0, fmt.Errorf("output %d (the pool's %s) holds another asset", i, what)
		case !bytes.Equal(o.Script, spk):
			return 0, fmt.Errorf("output %d (the pool's %s) does not pay the pool's own script", i, what)
		case o.HasNonce:
			return 0, fmt.Errorf("output %d (the pool's %s) carries a nonce", i, what)
		}
		return o.Value, nil
	}
	if m.X1, err = at(2*uint64(j), p.Base, "base coin"); err != nil {
		return nil, err
	}
	if m.Y1, err = at(2*uint64(j)+1, p.Quote, "quote coin"); err != nil {
		return nil, err
	}
	if sell {
		if m.X1 > m.X0 || m.Y1 < m.Y0 {
			return nil, fmt.Errorf("a sell pays base out and takes quote in: base %d -> %d, quote %d -> %d", m.X0, m.X1, m.Y0, m.Y1)
		}
		m.Base, m.Quote = m.X0-m.X1, m.Y1-m.Y0
		if m.X1 < p.ReserveBase {
			return nil, fmt.Errorf("the sell leaves %d of base, below the reserve %d", m.X1, p.ReserveBase)
		}
	} else {
		if m.X1 < m.X0 || m.Y1 > m.Y0 {
			return nil, fmt.Errorf("a buy takes base in and pays quote out: base %d -> %d, quote %d -> %d", m.X0, m.X1, m.Y0, m.Y1)
		}
		m.Base, m.Quote = m.X1-m.X0, m.Y0-m.Y1
		if m.Y1 < p.ReserveQuote {
			return nil, fmt.Errorf("the buy leaves %d of quote, below the reserve %d", m.Y1, p.ReserveQuote)
		}
	}
	if m.Base < p.MinLot || m.Base == 0 {
		return nil, fmt.Errorf("the fill moves %d of base, below min_lot %d", m.Base, p.MinLot)
	}
	if sell && !mulLE(m.Base, p.AskNum, m.Quote, p.AskDen) {
		return nil, fmt.Errorf("the pool takes in %d of quote for %d of base, below its ask %d/%d", m.Quote, m.Base, p.AskNum, p.AskDen)
	}
	if !sell && !mulLE(m.Quote, p.BidDen, m.Base, p.BidNum) {
		return nil, fmt.Errorf("the pool pays out %d of quote for %d of base, above its bid %d/%d", m.Quote, m.Base, p.BidNum, p.BidDen)
	}
	return m, nil
}
