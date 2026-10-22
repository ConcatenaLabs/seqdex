package covenant

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"

	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// A clearing round fills crossing orders of one market at one price. The
// round's last output publishes the price, and every seqdex/round-order in the
// transaction reads it there (see RoundOrder). This file holds the price
// output, the rule that chooses the price (ClearPrice), the allocation at that
// price (Allocate) and the transaction's layout (BuildRound).

// Price is N atoms of Quote for every D atoms of Base. Asset ids are in
// internal byte order.
type Price struct {
	Base, Quote [32]byte
	N, D        uint64
}

// Rat is the price as a rational.
func (p Price) Rat() *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).SetUint64(p.N), new(big.Int).SetUint64(p.D))
}

// ClearingScript is the clearing output's scriptPubKey: OP_RETURN OP_PUSHDATA1
// 80 <base 32> <quote 32> <N 8> <D 8>, 83 bytes, the standard data-carrier size.
func ClearingScript(p Price) []byte {
	s := make([]byte, 0, 83)
	s = append(s, 0x6a, 0x4c, 0x50)
	s = append(s, p.Base[:]...)
	s = append(s, p.Quote[:]...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], p.N)
	s = append(s, n[:]...)
	binary.BigEndian.PutUint64(n[:], p.D)
	return append(s, n[:]...)
}

// ReadClearing reads a clearing output's scriptPubKey; ok is false for any
// other script.
func ReadClearing(s []byte) (p Price, ok bool) {
	if len(s) != 83 || s[0] != 0x6a || s[1] != 0x4c || s[2] != 0x50 {
		return p, false
	}
	copy(p.Base[:], s[3:35])
	copy(p.Quote[:], s[35:67])
	p.N = binary.BigEndian.Uint64(s[67:75])
	p.D = binary.BigEndian.Uint64(s[75:83])
	return p, true
}

// Side is one participant of a round, in the market (Base, Quote): a round
// order, or one side of a pool.
type Side struct {
	ID string
	// Ask: it sells Base (a round order selling Base, or a pool's ask); else it
	// sells Quote (a round order selling Quote, or a pool's bid).
	Ask bool
	// Size: what it can sell, in the asset it sells (an ask's Base, a bid's
	// Quote); a pool side lists what its coin holds above its reserve.
	Size uint64
	// Limit: the price in Quote per Base it accepts, at least for an ask, at
	// most for a bid.
	Limit *big.Rat
	// MinLot floors a fill and any remainder, in the asset it sells; for a pool
	// it floors the Base a fill moves, and a pool keeps no remainder floor.
	MinLot uint64
	Pool   bool
	// Seq orders sides of one limit: the earlier first.
	Seq int
}

// ClearPrice chooses a round's price among sides that cross. It maximises the
// Base exchanged, min(supply, demand), over every limit and every price
// between two neighbouring limits at which supply equals demand; then it
// minimises the imbalance |supply - demand|; then it takes the lowest price.
// ok is false when nothing crosses.
func ClearPrice(sides []Side, base, quote [32]byte) (p Price, ok bool, err error) {
	var cands []*big.Rat
	for _, s := range sides {
		if s.Limit == nil || s.Limit.Sign() <= 0 {
			return p, false, fmt.Errorf("side %s has no positive limit", s.ID)
		}
		cands = append(cands, s.Limit)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Cmp(cands[j]) < 0 })
	uniq := cands[:0]
	for _, c := range cands {
		if len(uniq) == 0 || uniq[len(uniq)-1].Cmp(c) != 0 {
			uniq = append(uniq, c)
		}
	}
	type eval struct {
		price, volume, imbalance *big.Rat
	}
	measure := func(price *big.Rat, asksAt, bidsAt func(Side) bool) eval {
		supply, q := new(big.Rat), new(big.Rat)
		for _, s := range sides {
			sz := new(big.Rat).SetInt(new(big.Int).SetUint64(s.Size))
			if s.Ask && asksAt(s) {
				supply.Add(supply, sz)
			}
			if !s.Ask && bidsAt(s) {
				q.Add(q, sz)
			}
		}
		demand := new(big.Rat).Quo(q, price)
		v := supply
		if demand.Cmp(v) < 0 {
			v = demand
		}
		imb := new(big.Rat).Sub(supply, demand)
		return eval{price, v, imb.Abs(imb)}
	}
	var evals []eval
	for _, c := range uniq {
		c := c
		evals = append(evals, measure(c, func(s Side) bool { return s.Limit.Cmp(c) <= 0 }, func(s Side) bool { return s.Limit.Cmp(c) >= 0 }))
	}
	for i := 0; i+1 < len(uniq); i++ {
		lo, hi := uniq[i], uniq[i+1]
		supply, q := new(big.Rat), new(big.Rat)
		for _, s := range sides {
			sz := new(big.Rat).SetInt(new(big.Int).SetUint64(s.Size))
			if s.Ask && s.Limit.Cmp(lo) <= 0 {
				supply.Add(supply, sz)
			}
			if !s.Ask && s.Limit.Cmp(hi) >= 0 {
				q.Add(q, sz)
			}
		}
		if supply.Sign() == 0 || q.Sign() == 0 {
			continue
		}
		bal := new(big.Rat).Quo(q, supply)
		if bal.Cmp(lo) <= 0 || bal.Cmp(hi) >= 0 {
			continue
		}
		fit, okFit := fit64(bal, lo, hi)
		if !okFit {
			continue
		}
		evals = append(evals, measure(fit, func(s Side) bool { return s.Limit.Cmp(lo) <= 0 }, func(s Side) bool { return s.Limit.Cmp(hi) >= 0 }))
	}
	var best *eval
	for i := range evals {
		e := &evals[i]
		if e.volume.Sign() <= 0 {
			continue
		}
		if best == nil {
			best = e
			continue
		}
		switch c := e.volume.Cmp(best.volume); {
		case c > 0:
			best = e
		case c == 0:
			switch ci := e.imbalance.Cmp(best.imbalance); {
			case ci < 0:
				best = e
			case ci == 0 && e.price.Cmp(best.price) < 0:
				best = e
			}
		}
	}
	if best == nil {
		return p, false, nil
	}
	n, d := best.price.Num(), best.price.Denom()
	if !n.IsUint64() || !d.IsUint64() {
		return p, false, fmt.Errorf("the price %s does not fit 64 bits", best.price.RatString())
	}
	return Price{Base: base, Quote: quote, N: n.Uint64(), D: d.Uint64()}, true, nil
}

// fit64 is x when its terms fit 64 bits, else the last continued-fraction
// convergent of x whose terms fit and which stays strictly between lo and hi.
func fit64(x, lo, hi *big.Rat) (*big.Rat, bool) {
	if x.Num().IsUint64() && x.Denom().IsUint64() {
		return x, true
	}
	var best *big.Rat
	h0, h1 := big.NewInt(0), big.NewInt(1)
	k0, k1 := big.NewInt(1), big.NewInt(0)
	r := new(big.Rat).Set(x)
	for i := 0; i < 128; i++ {
		a := new(big.Int).Quo(r.Num(), r.Denom())
		h := new(big.Int).Add(new(big.Int).Mul(a, h1), h0)
		k := new(big.Int).Add(new(big.Int).Mul(a, k1), k0)
		if !h.IsUint64() || !k.IsUint64() || k.Sign() == 0 {
			break
		}
		c := new(big.Rat).SetFrac(h, k)
		if c.Cmp(lo) > 0 && c.Cmp(hi) < 0 {
			best = c
		}
		h0, h1, k0, k1 = h1, h, k1, k
		frac := new(big.Rat).Sub(r, new(big.Rat).SetInt(a))
		if frac.Sign() == 0 {
			break
		}
		r = frac.Inv(frac)
	}
	return best, best != nil
}

// Fill is a side's part in a round at its price: Sold of the asset it sells,
// and Got of the other. A round order is credited exactly ceil(Sold x its
// rate); a pool selling Base takes in exactly ceil(Sold x N/D) of Quote, and a
// pool buying db of Base pays out floor(db x N/D) of Quote (Sold is then the
// Quote it pays and Got the Base).
type Fill struct {
	ID   string
	Ask  bool
	Pool bool
	Sold uint64
	Got  uint64
}

// Allocate fills the sides that accept p. The short side fills whole; the long
// side fills in price, then Seq, priority up to the short side's Base, the last
// in part where its minimum lots allow, a side that cannot take a legal part
// skipped. It returns the fills and the round's shortfall in each asset: what
// the participants are credited beyond what they sell, under one atom per
// participant, which the clearer adds.
func Allocate(sides []Side, p Price) (fills []Fill, shortBase, shortQuote uint64, err error) {
	if p.N == 0 || p.D == 0 {
		return nil, 0, 0, fmt.Errorf("price %d/%d", p.N, p.D)
	}
	pr := p.Rat()
	var asks, bids []Side
	for _, s := range sides {
		if s.Ask && s.Limit.Cmp(pr) <= 0 {
			asks = append(asks, s)
		}
		if !s.Ask && s.Limit.Cmp(pr) >= 0 {
			bids = append(bids, s)
		}
	}
	sort.SliceStable(asks, func(i, j int) bool {
		if c := asks[i].Limit.Cmp(asks[j].Limit); c != 0 {
			return c < 0
		}
		return asks[i].Seq < asks[j].Seq
	})
	sort.SliceStable(bids, func(i, j int) bool {
		if c := bids[i].Limit.Cmp(bids[j].Limit); c != 0 {
			return c > 0
		}
		return bids[i].Seq < bids[j].Seq
	})
	// Base a bid of g quote is credited (or a pool's bid takes for g).
	bidBase := func(s Side, g uint64) (uint64, error) {
		if s.Pool {
			// The most Base whose floor(db*N/D) is at most g.
			return floorMulDiv(g, p.D, p.N)
		}
		return ceilMulDiv(g, p.D, p.N)
	}
	var supply, demand uint64
	for _, s := range asks {
		supply += s.Size
	}
	for _, s := range bids {
		b, err := bidBase(s, s.Size)
		if err != nil {
			return nil, 0, 0, err
		}
		demand += b
	}
	volume := supply
	if demand < volume {
		volume = demand
	}
	if volume == 0 {
		return nil, 0, 0, fmt.Errorf("nothing crosses at %d/%d", p.N, p.D)
	}
	legal := func(s Side, take uint64) uint64 {
		if take >= s.Size {
			return s.Size
		}
		if s.Pool {
			if take < s.MinLot {
				return 0
			}
			return take
		}
		if s.Size-take < s.MinLot {
			if s.Size < 2*s.MinLot {
				return 0
			}
			take = s.Size - s.MinLot
		}
		if take < s.MinLot {
			return 0
		}
		return take
	}
	for round := 0; round < 4; round++ {
		fills = fills[:0]
		var askBase, bidGot uint64
		left := volume
		for _, s := range asks {
			t := legal(s, left)
			if t == 0 {
				continue
			}
			got, err := ceilMulDiv(t, p.N, p.D)
			if err != nil {
				return nil, 0, 0, err
			}
			fills = append(fills, Fill{ID: s.ID, Ask: true, Pool: s.Pool, Sold: t, Got: got})
			askBase += t
			left -= t
			if left == 0 {
				break
			}
		}
		left = volume
		for _, s := range bids {
			full, err := bidBase(s, s.Size)
			if err != nil {
				return nil, 0, 0, err
			}
			var g, b uint64
			if full <= left {
				g, b = s.Size, full
			} else {
				if s.Pool {
					b = left
					if g, err = floorMulDiv(b, p.N, p.D); err != nil {
						return nil, 0, 0, err
					}
				} else {
					want, err := floorMulDiv(left, p.N, p.D)
					if err != nil {
						return nil, 0, 0, err
					}
					g = legal(s, want)
					for g > 0 {
						if b, err = ceilMulDiv(g, p.D, p.N); err != nil {
							return nil, 0, 0, err
						}
						if b <= left {
							break
						}
						g = legal(s, g-1)
					}
				}
				if s.Pool && b < s.MinLot {
					g, b = 0, 0
				}
			}
			if g == 0 && b == 0 {
				continue
			}
			if s.Pool {
				// The pool sells g of Quote and takes b of Base.
				fills = append(fills, Fill{ID: s.ID, Pool: true, Sold: g, Got: b})
			} else {
				fills = append(fills, Fill{ID: s.ID, Sold: g, Got: b})
			}
			bidGot += b
			left -= b
			if left == 0 {
				break
			}
		}
		if askBase == bidGot || askBase == 0 || bidGot == 0 {
			if askBase == 0 || bidGot == 0 {
				return nil, 0, 0, fmt.Errorf("no legal fill on one side at %d/%d", p.N, p.D)
			}
			break
		}
		next := askBase
		if bidGot < next {
			next = bidGot
		}
		if next == volume {
			break
		}
		volume = next
	}
	var baseIn, baseOut, quoteIn, quoteOut uint64
	for _, f := range fills {
		if f.Ask {
			baseIn += f.Sold
			quoteOut += f.Got
		} else {
			quoteIn += f.Sold
			baseOut += f.Got
		}
	}
	if baseOut > baseIn {
		shortBase = baseOut - baseIn
	}
	if quoteOut > quoteIn {
		shortQuote = quoteOut - quoteIn
	}
	return fills, shortBase, shortQuote, nil
}

// RoundCoin is a coin a round spends: its outpoint (display txid) and what the
// chain shows of it.
type RoundCoin struct {
	Txid   string
	Vout   uint32
	Asset  [32]byte
	Value  uint64
	Script []byte
}

func (c RoundCoin) spent() SpentCoin { return ExplicitCoin(c.Asset, c.Value, c.Script) }

func (c RoundCoin) output() Output { return Explicit(c.Asset, c.Value, c.Script) }

// RoundTake is one round order in a round: its coin and what it sells.
type RoundTake struct {
	Order  RoundOrder
	Coin   RoundCoin
	Filled uint64
}

// RoundPool is a pool in a round: its two coins and the move. Sell is true when
// the pool pays Base out; Base and Quote are what move.
type RoundPool struct {
	Pool        SimPool
	BaseCoin    RoundCoin
	QuoteCoin   RoundCoin
	Sell        bool
	Base, Quote uint64
}

// Round is a clearing round to lay out.
type Round struct {
	Price  Price
	Orders []RoundTake // at inputs 0..n-1
	Pools  []RoundPool // their Base coins at n, n+2, ...; Quote coins after each
	// Funding are the clearer's coins after the covenant inputs: its gap coins
	// and its fee coin. Change returns what the round does not spend of them
	// to ChangeScript.
	Funding      []RoundCoin
	ChangeScript []byte
	FeeAsset     [32]byte
	Fee          uint64
}

// Laid is a round laid out: the transaction with every covenant input's
// witness but the clearer's signature, which SetClearerSig fills in once the
// clearer has signed Digest.
type Laid struct {
	Tx     *transaction.Transaction
	Coins  []SpentCoin
	Digest [32]byte
	Round  Round
	Fills  []*RoundFill
	Moves  []*PoolMove
}

// BuildRound lays out a round: round orders at inputs 0..n-1, each credit at
// 2k and remainder at 2k+1; each pool's Base coin at an input j after them
// with its Quote coin at j+1, the pool again at 2j and 2j+1; the clearer's
// coins last. Its change fills the free slots (a whole fill's 2k+1, a pool's
// 2j+2 and 2j+3), then the fee; a free slot left over holds a bare OP_RETURN of
// nothing. The clearing output is last. Every order and pool is judged over the
// result (CheckFill), so a layout the chain would refuse is an error here.
func BuildRound(r Round, genesis [32]byte) (*Laid, error) {
	if len(r.Orders) == 0 {
		return nil, fmt.Errorf("a round without a round order needs no clearer")
	}
	type slot struct {
		o    *Output
		used bool
	}
	nIn := len(r.Orders) + 2*len(r.Pools)
	slots := make([]slot, 0, 2*nIn)
	put := func(i int, o Output) error {
		for len(slots) <= i {
			slots = append(slots, slot{})
		}
		if slots[i].used {
			return fmt.Errorf("output %d is claimed twice", i)
		}
		oo := o
		slots[i] = slot{o: &oo, used: true}
		return nil
	}
	balance := map[[32]byte]int64{}
	add := func(a [32]byte, v uint64, sign int64) { balance[a] += sign * int64(v) }
	var coins []SpentCoin
	var ins []RoundCoin
	for k, t := range r.Orders {
		o := t.Order
		credit, err := o.Credit(t.Filled, r.Price)
		if err != nil {
			return nil, fmt.Errorf("order %d: %w", k, err)
		}
		if !o.Accepts(r.Price) {
			return nil, fmt.Errorf("order %d does not accept the price %d/%d", k, r.Price.N, r.Price.D)
		}
		if err := put(2*k, Explicit(o.AssetB, credit, o.MakerScript)); err != nil {
			return nil, err
		}
		add(o.AssetA, t.Coin.Value, 1)
		add(o.AssetB, credit, -1)
		if t.Filled < t.Coin.Value {
			spk, err := o.Script()
			if err != nil {
				return nil, err
			}
			if err := put(2*k+1, Explicit(o.AssetA, t.Coin.Value-t.Filled, spk)); err != nil {
				return nil, err
			}
			add(o.AssetA, t.Coin.Value-t.Filled, -1)
		}
		ins = append(ins, t.Coin)
	}
	for i, pl := range r.Pools {
		j := len(r.Orders) + 2*i
		spk, err := pl.Pool.Script()
		if err != nil {
			return nil, err
		}
		x1, y1 := pl.BaseCoin.Value, pl.QuoteCoin.Value
		if pl.Sell {
			x1, y1 = x1-pl.Base, y1+pl.Quote
		} else {
			x1, y1 = x1+pl.Base, y1-pl.Quote
		}
		if err := put(2*j, Explicit(pl.Pool.Base, x1, spk)); err != nil {
			return nil, err
		}
		if err := put(2*j+1, Explicit(pl.Pool.Quote, y1, spk)); err != nil {
			return nil, err
		}
		add(pl.Pool.Base, pl.BaseCoin.Value, 1)
		add(pl.Pool.Quote, pl.QuoteCoin.Value, 1)
		add(pl.Pool.Base, x1, -1)
		add(pl.Pool.Quote, y1, -1)
		ins = append(ins, pl.BaseCoin, pl.QuoteCoin)
	}
	for _, c := range r.Funding {
		add(c.Asset, c.Value, 1)
		ins = append(ins, c)
	}
	add(r.FeeAsset, r.Fee, -1)
	var change []Output
	var assets [][32]byte
	for a := range balance {
		assets = append(assets, a)
	}
	sort.Slice(assets, func(i, j int) bool { return bytes.Compare(assets[i][:], assets[j][:]) < 0 })
	for _, a := range assets {
		switch v := balance[a]; {
		case v < 0:
			return nil, fmt.Errorf("the round is short %d of %x: the clearer's coins do not cover it", -v, a)
		case v > 0:
			change = append(change, Explicit(a, uint64(v), r.ChangeScript))
		}
	}
	feeOut := Output{Asset: r.FeeAsset, AssetExplicit: true, Value: r.Fee, ValueExplicit: true, Script: []byte{}}
	tail := append(change, feeOut)
	var outs []Output
	for i := range slots {
		switch {
		case slots[i].used:
			outs = append(outs, *slots[i].o)
		case len(tail) > 0:
			outs = append(outs, tail[0])
			tail = tail[1:]
		default:
			outs = append(outs, Output{Asset: r.FeeAsset, AssetExplicit: true, Value: 0, ValueExplicit: true, Script: []byte{0x6a}})
		}
	}
	outs = append(outs, tail...)
	outs = append(outs, Output{Asset: r.FeeAsset, AssetExplicit: true, Value: 0, ValueExplicit: true, Script: ClearingScript(r.Price)})

	tx := transaction.NewTx(2)
	for _, c := range ins {
		h, err := elementsutil.TxIDToBytes(c.Txid)
		if err != nil || len(h) != 32 {
			return nil, fmt.Errorf("coin txid %q", c.Txid)
		}
		in := transaction.NewTxInput(h, c.Vout)
		tx.AddInput(in)
		coins = append(coins, c.spent())
	}
	for _, o := range outs {
		v, err := elementsutil.ValueToBytes(o.Value)
		if err != nil {
			return nil, err
		}
		tx.AddOutput(transaction.NewTxOutput(append([]byte{0x01}, o.Asset[:]...), v, o.Script))
	}
	l := &Laid{Tx: tx, Coins: coins, Round: r}
	for k, t := range r.Orders {
		f, err := t.Order.CheckFill(t.Coin.output(), outs, uint32(k), t.Filled < t.Coin.Value)
		if err != nil {
			return nil, fmt.Errorf("order %d: its program would refuse this round: %w", k, err)
		}
		l.Fills = append(l.Fills, f)
	}
	for i, pl := range r.Pools {
		j := uint32(len(r.Orders) + 2*i)
		m, err := pl.Pool.CheckFill(pl.BaseCoin.output(), pl.QuoteCoin.output(), outs, j, pl.Sell)
		if err != nil {
			return nil, fmt.Errorf("pool %d: its program would refuse this round: %w", i, err)
		}
		l.Moves = append(l.Moves, m)
		w, err := pl.Pool.FillWitness(pl.Sell)
		if err != nil {
			return nil, err
		}
		tx.Inputs[j].Witness = w
		if w, err = pl.Pool.PairWitness(); err != nil {
			return nil, err
		}
		tx.Inputs[j+1].Witness = w
	}
	d, err := ClearingDigest(genesis, tx, coins)
	if err != nil {
		return nil, err
	}
	l.Digest = d
	return l, nil
}

// SetClearerSig puts the clearer's signature over Digest into every round
// order's witness. Witnesses are outside the digest, so the clearer's coins may
// be signed before or after.
func (l *Laid) SetClearerSig(sig [64]byte) error {
	for k, t := range l.Round.Orders {
		if err := t.Order.CheckClearerSig(l.Digest, sig); err != nil {
			return fmt.Errorf("order %d: %w", k, err)
		}
		w, err := t.Order.FillWitness(t.Filled < t.Coin.Value, l.Round.Price, sig)
		if err != nil {
			return err
		}
		l.Tx.Inputs[k].Witness = w
	}
	return nil
}
