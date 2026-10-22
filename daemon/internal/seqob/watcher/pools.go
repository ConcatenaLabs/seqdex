package watcher

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A pool (seqdex/pool) rests at two coins, its base coin and its quote coin,
// at one address. Each of its two offers lists what its own coin holds above
// that coin's reserve: a pool lists only what it holds. A fill spends both
// coins, the base coin at input j, and re-creates the pool at the spender's
// outputs 2j and 2j+1; the maker's withdrawal spends them anywhere.
//
// The follower reads the pool's base coin every pass. Unspent: both coins are
// checked and each side listed at its depth. Spent by an unconfirmed
// transaction: both sides are held at 0 (nothing can fill them until it
// settles). Spent in a block: a fill moves both offers to the new coins; a
// withdrawal holds them at 0 until it is final, then delists them. Every move
// is kept, and when its transaction leaves the active chain (a Bitcoin-driven
// reorg) the pool returns to the coins it rested at before it.

// PoolBook is the part of the order book the pool follower moves.
type PoolBook interface {
	SnapshotPools() []offerstore.PoolEntry
	RepointPool(k offerstore.Key, baseTxid string, baseVout uint32, quoteTxid string, quoteVout uint32, active uint64)
	ReconcilePool(k offerstore.Key, active uint64)
	RemovePool(k offerstore.Key, status seqobv1.OfferStatus) error
	RecordPoolMove(m offerstore.PoolMove)
}

type poolCoins struct {
	baseTxid  string
	baseVout  uint32
	quoteTxid string
	quoteVout uint32
}

type poolStep struct {
	prev     poolCoins
	spender  string
	withdraw bool
	required int64
	final    bool
	move     offerstore.PoolMove
}

// poolSide is what the follower needs of one listed side.
type poolSide struct {
	pool  covenant.SimPool
	spk   []byte
	key   string
	ask   bool
	coins poolCoins
}

func readPoolSide(o *seqobv1.Offer) (poolSide, error) {
	var s poolSide
	p, err := offer.PoolOf(o.GetPool())
	if err != nil {
		return s, err
	}
	spk, err := p.Script()
	if err != nil {
		return s, err
	}
	bt, bv, qt, qv, _ := offer.PoolCoins(o)
	k, _ := offer.PoolKey(o)
	return poolSide{pool: p, spk: spk, key: k, ask: offer.PoolIsAsk(o), coins: poolCoins{bt, bv, qt, qv}}, nil
}

// depth is what this side lists from what the coins hold, never above what its
// maker signed: the ask what the base coin holds above its reserve, the bid
// what the quote coin holds above its.
func (s poolSide) depth(x, y uint64, o *seqobv1.Offer) uint64 {
	a, b := s.pool.Depth(x, y)
	d := b
	if s.ask {
		d = a
	}
	if d > o.GetOfferAmount() {
		d = o.GetOfferAmount()
	}
	return d
}

func readCoin(chain ChainView, txid string, vout uint32, spk []byte, asset [32]byte) (CovState, error) {
	st, err := chain.Inspect(txid, vout, hex.EncodeToString(spk), displayOf(asset))
	if err != nil {
		return st, err
	}
	if st.Unspent {
		switch {
		case !strings.EqualFold(st.SPKHex, hex.EncodeToString(spk)):
			return st, fmt.Errorf("pool coin %s:%d does not pay the pool's script, the one its terms make", txid, vout)
		case st.Confidential:
			return st, fmt.Errorf("pool coin %s:%d is confidential: the pool's program reads only explicit amounts", txid, vout)
		case !strings.EqualFold(st.AssetDisplay, displayOf(asset)):
			return st, fmt.Errorf("pool coin %s:%d holds asset %s, not %s", txid, vout, st.AssetDisplay, displayOf(asset))
		}
	}
	return st, nil
}

// ErrPoolDrained is ProbePool's refusal of a side whose coin holds nothing
// above its reserve. A post is refused for it; a side the relay already listed
// (read back from its covenant book) is kept, held at 0, and the follower lists
// it again once a fill refills its coin.
var ErrPoolDrained = errors.New("holds nothing above its reserve")

// ProbePool checks a side of a pool when it is posted: both coins unspent at
// the pool's script, holding the pool's two assets explicitly, and something
// above the side's reserve. It returns what the side lists (offer units,
// never above offer_amount); 0 when the chain could not be asked.
func ProbePool(chain ChainView, o *seqobv1.Offer) (uint64, error) {
	if !offer.IsPool(o) || chain == nil {
		return 0, nil
	}
	s, err := readPoolSide(o)
	if err != nil {
		return 0, fmt.Errorf("pool terms: %w", err)
	}
	xs, err := readCoin(chain, s.coins.baseTxid, s.coins.baseVout, s.spk, s.pool.Base)
	if err != nil {
		if xs.Unspent {
			return 0, err
		}
		return 0, nil // the node could not be asked: no evidence
	}
	ys, err := readCoin(chain, s.coins.quoteTxid, s.coins.quoteVout, s.spk, s.pool.Quote)
	if err != nil {
		if ys.Unspent {
			return 0, err
		}
		return 0, nil
	}
	if !xs.Unspent {
		return 0, fmt.Errorf("the pool's base coin %s:%d %w", s.coins.baseTxid, s.coins.baseVout, ErrNotUnspent)
	}
	if !ys.Unspent {
		return 0, fmt.Errorf("the pool's quote coin %s:%d %w", s.coins.quoteTxid, s.coins.quoteVout, ErrNotUnspent)
	}
	d := s.depth(xs.Value, ys.Value, o)
	if d == 0 {
		side, reserve := "quote coin", s.pool.ReserveQuote
		if s.ask {
			side, reserve = "base coin", s.pool.ReserveBase
		}
		return 0, fmt.Errorf("the pool's %s %w %d: this side has nothing to fill", side, ErrPoolDrained, reserve)
	}
	return d, nil
}

// reconcilePools runs the pool follower over every listed side.
func (w *Watcher) reconcilePools(pb PoolBook, a AnchorTip, anchorKnown bool, st *Stats) {
	if w.poolTracks == nil {
		w.poolTracks = map[offerstore.Key][]*poolStep{}
	}
	for _, e := range pb.SnapshotPools() {
		s, err := readPoolSide(e.Offer)
		if err != nil {
			st.Errors++
			continue
		}
		if !w.recheckPool(pb, e, s, a, anchorKnown, st) {
			continue
		}
		if w.lastWithdrawn(e.Key) {
			continue
		}
		xs, err := readCoin(w.chain, s.coins.baseTxid, s.coins.baseVout, s.spk, s.pool.Base)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: pool %s/%s: base coin: %v", e.Key.MakerPubkey, e.Key.OfferID, err)
			pb.ReconcilePool(e.Key, 0)
			continue
		}
		switch {
		case xs.Unspent:
			ys, err := readCoin(w.chain, s.coins.quoteTxid, s.coins.quoteVout, s.spk, s.pool.Quote)
			if err != nil || !ys.Unspent {
				// The quote coin is spent without the base coin: only the maker's
				// withdrawal does that. Nothing can fill the pool now.
				pb.ReconcilePool(e.Key, 0)
				continue
			}
			st.Live++
			pb.ReconcilePool(e.Key, s.depth(xs.Value, ys.Value, e.Offer))
		case xs.SpenderTxid != "" && !xs.SpenderConfirmed:
			st.Pending++
			pb.ReconcilePool(e.Key, 0)
		case xs.Spender != nil:
			w.recordPoolSpend(pb, e, s, xs.Spender, a, anchorKnown, st)
		default:
			st.Ghost++
			pb.ReconcilePool(e.Key, 0)
		}
	}
}

func (w *Watcher) lastWithdrawn(k offerstore.Key) bool {
	tr := w.poolTracks[k]
	return len(tr) > 0 && tr[len(tr)-1].withdraw
}

// recheckPool measures every recorded move of a side, oldest first; the first
// whose transaction left the active chain undoes it and every move after it,
// and the pool rests again where it rested before. It returns false when the
// side left the book.
func (w *Watcher) recheckPool(pb PoolBook, e offerstore.PoolEntry, s poolSide, a AnchorTip, anchorKnown bool, st *Stats) bool {
	tr := w.poolTracks[e.Key]
	for i, step := range tr {
		ts, err := w.chain.TxStatus(step.spender)
		if err != nil {
			st.Errors++
			return true
		}
		if !ts.InChain {
			why := "its transaction left the active chain"
			if ts.InMempool {
				why = "its block left the active chain (its transaction is in the mempool again)"
			}
			for j := len(tr) - 1; j >= i; j-- {
				m := tr[j].move
				m.State, m.Reason = offerstore.FillUndone, why
				pb.RecordPoolMove(m)
				st.Undone++
				w.log.Printf("watcher: pool %s/%s move %s UNDONE: %s", e.Key.MakerPubkey, e.Key.OfferID, short(tr[j].spender), why)
			}
			p := tr[i].prev
			pb.RepointPool(e.Key, p.baseTxid, p.baseVout, p.quoteTxid, p.quoteVout, 0)
			w.poolTracks[e.Key] = tr[:i]
			return true
		}
		depth, _, final := finality(ts, a, anchorKnown, step.required)
		m := step.move
		m.Block, m.Height, m.AnchorDepth, m.State = ts.Block, ts.Height, depth, offerstore.FillConfirmed
		if final {
			m.State = offerstore.FillFinal
		}
		if m != step.move {
			pb.RecordPoolMove(m)
			step.move = m
		}
		if final && !step.final {
			step.final = true
			st.Final++
			if step.withdraw && i == len(tr)-1 {
				w.log.Printf("watcher: pool %s/%s withdrawn by its maker in %s, FINAL: delisted", e.Key.MakerPubkey, e.Key.OfferID, short(step.spender))
				if err := pb.RemovePool(e.Key, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED); err != nil {
					st.Errors++
				}
				return false
			}
		}
	}
	return true
}

// recordPoolSpend records a confirmed spend of a side's base coin: a fill that
// re-creates the pool at 2j and 2j+1 (both offers move there), or anything
// else, the maker's withdrawal (held at 0, delisted once final).
func (w *Watcher) recordPoolSpend(pb PoolBook, e offerstore.PoolEntry, s poolSide, sp *SpenderTx, a AnchorTip, anchorKnown bool, st *Stats) {
	ts, err := w.chain.TxStatus(sp.Txid)
	if err != nil || !ts.InChain {
		pb.ReconcilePool(e.Key, 0)
		return
	}
	j := uint64(sp.Input)
	outs := sp.Outputs
	at := func(i uint64, asset [32]byte) (uint64, bool) {
		if i >= uint64(len(outs)) {
			return 0, false
		}
		o := outs[i]
		ok := o.AssetExplicit && o.ValueExplicit && o.Asset == asset && !o.HasNonce && string(o.Script) == string(s.spk)
		return o.Value, ok
	}
	path := covenant.PoolPath(sp.Witness)
	x1, okx := at(2*j, s.pool.Base)
	y1, oky := at(2*j+1, s.pool.Quote)
	step := &poolStep{prev: s.coins, spender: sp.Txid, required: w.required(e.Offer)}
	m := offerstore.PoolMove{Pool: s.key, Txid: sp.Txid, Input: sp.Input, Block: ts.Block, Height: ts.Height,
		State: offerstore.FillConfirmed}
	depth, _, final := finality(ts, a, anchorKnown, step.required)
	m.AnchorDepth = depth
	if final {
		m.State = offerstore.FillFinal
	}
	if (path == "sell" || path == "buy") && okx && oky {
		x0 := sp.PrevValue
		m.Sell, m.X1, m.Y1 = path == "sell", x1, y1
		if m.Sell && x0 >= x1 {
			m.Base = x0 - x1
		} else if !m.Sell && x1 >= x0 {
			m.Base = x1 - x0
		}
		if len(outs) > 0 {
			if p, ok := covenant.ReadClearing(outs[len(outs)-1].Script); ok {
				m.Price = &offerstore.RoundPrice{Base: displayOf(p.Base), Quote: displayOf(p.Quote), N: p.N, D: p.D}
			}
		}
		step.move = m
		step.final = final
		w.poolTracks[e.Key] = append(w.poolTracks[e.Key], step)
		pb.RecordPoolMove(m)
		st.PartialFill++
		w.log.Printf("watcher: pool %s/%s FILL %s (%s %d base): rests again at %s:%d and %s:%d, %d base and %d quote",
			e.Key.MakerPubkey, e.Key.OfferID, short(sp.Txid), path, m.Base, short(sp.Txid), 2*j, short(sp.Txid), 2*j+1, x1, y1)
		pb.RepointPool(e.Key, sp.Txid, uint32(2*j), sp.Txid, uint32(2*j+1), s.depth(x1, y1, e.Offer))
		return
	}
	m.Withdrawn = true
	step.move, step.withdraw, step.final = m, true, final
	w.poolTracks[e.Key] = append(w.poolTracks[e.Key], step)
	pb.RecordPoolMove(m)
	st.Filled++
	pb.ReconcilePool(e.Key, 0)
	w.log.Printf("watcher: pool %s/%s withdrawn by its maker in %s; held until final, then delisted", e.Key.MakerPubkey, e.Key.OfferID, short(sp.Txid))
	if final {
		if err := pb.RemovePool(e.Key, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED); err != nil {
			st.Errors++
		}
	}
}
