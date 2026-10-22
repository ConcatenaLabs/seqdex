package offerstore

import (
	"sort"
	"strings"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"google.golang.org/protobuf/proto"
)

// --- pools ------------------------------------------------------------------------
//
// A pool (PoolTerms) lists two offers, its ask and its bid, each at what its own
// coin holds above that coin's reserve. The relay's pool follower (watcher.Pools)
// moves both to the coins a fill re-creates the pool at, holds them at 0 while
// a spend of the pool is unconfirmed or the coins cannot be read, and delists
// them once the maker's withdrawal is final. It records each move it sees in a
// block (PoolMove), and undoes the record when its block leaves the chain.

// PoolEntry is a listed side of a pool, for the follower.
type PoolEntry struct {
	Key    Key
	Offer  *seqobv1.Offer // the live pointer; read only
	Active uint64
}

// PoolMove is one move of a pool seen in a block: a fill at its quotes (Sell:
// the pool paid base out), or the maker's withdrawal.
type PoolMove struct {
	Pool      string      `json:"pool"` // the pool's key (its terms)
	Txid      string      `json:"txid"`
	Input     uint32      `json:"input"` // j, the base coin's input
	Withdrawn bool        `json:"withdrawn,omitempty"`
	Sell      bool        `json:"sell"`
	Base      uint64      `json:"base"`  // base that moved
	Quote     uint64      `json:"quote"` // quote that moved
	X1        uint64      `json:"base_after"`
	Y1        uint64      `json:"quote_after"`
	Price     *RoundPrice `json:"round_price,omitempty"` // the price of the clearing round the fill was part of, if any

	Block       string    `json:"block"`
	Height      int64     `json:"height"`
	AnchorDepth int64     `json:"anchor_depth"`
	State       string    `json:"state"` // confirmed, final, undone
	Reason      string    `json:"reason,omitempty"`
	Recorded    time.Time `json:"recorded"`
}

func poolMoveID(m PoolMove) string { return m.Pool + "/" + m.Txid }

// SnapshotPools returns every listed side of a pool.
func (s *Store) SnapshotPools() []PoolEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PoolEntry, 0)
	for k, e := range s.entries {
		if offer.IsPool(e.Offer) {
			out = append(out, PoolEntry{Key: k, Offer: e.Offer, Active: e.ActiveAmount})
		}
	}
	return out
}

func (s *Store) poolStatusLocked(e *Entry) {
	if e.ActiveAmount >= e.Offer.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
}

// RepointPool points a listed side of a pool at the coins the pool rests at
// now and lists it at active (never above what its maker signed). The signed
// terms are unchanged. No-op for a missing or unchanged entry.
func (s *Store) RepointPool(k Key, baseTxid string, baseVout uint32, quoteTxid string, quoteVout uint32, active uint64) {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsPool(e.Offer) {
		s.mu.Unlock()
		return
	}
	if active > e.Offer.GetBaseAmount() {
		active = e.Offer.GetBaseAmount()
	}
	bt, bv, qt, qv, _ := offer.PoolCoins(e.Offer)
	if strings.EqualFold(bt, baseTxid) && bv == baseVout && strings.EqualFold(qt, quoteTxid) && qv == quoteVout && e.ActiveAmount == active {
		s.mu.Unlock()
		return
	}
	no := proto.Clone(e.Offer).(*seqobv1.Offer)
	offer.SetPoolCoins(no, baseTxid, baseVout, quoteTxid, quoteVout)
	s.unindexLocked(k, e.Offer)
	e.Offer = no
	s.indexLocked(k, no)
	e.ActiveAmount = active
	e.reRested = true
	s.poolStatusLocked(e)
	pair, conf, st := no.GetPair(), no.GetConfidential(), e.orderStatus(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventCreated, Pair: pair, Confidential: conf, Offer: no})
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
}

// ReconcilePool lists a side of a pool at active, its coins unchanged: what its
// coin holds above its reserve, or 0 while it cannot be filled (a spend of the
// pool unconfirmed, or the coins unreadable). No-op when unchanged.
func (s *Store) ReconcilePool(k Key, active uint64) {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsPool(e.Offer) {
		s.mu.Unlock()
		return
	}
	if active > e.Offer.GetBaseAmount() {
		active = e.Offer.GetBaseAmount()
	}
	if e.ActiveAmount == active {
		s.mu.Unlock()
		return
	}
	e.ActiveAmount = active
	s.poolStatusLocked(e)
	pair, conf, st := e.Offer.GetPair(), e.Offer.GetConfidential(), e.orderStatus(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
}

// RemovePool delists a side of a pool (its maker withdrew it). The move
// records stay.
func (s *Store) RemovePool(k Key, status seqobv1.OfferStatus) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsPool(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	e.ActiveAmount = 0
	s.mu.Unlock()
	return s.dropWithStatus(k, status)
}

// RecordPoolMove records, or updates, a pool's move.
func (s *Store) RecordPoolMove(m PoolMove) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poolMoves == nil {
		s.poolMoves = map[string]PoolMove{}
	}
	if old, ok := s.poolMoves[poolMoveID(m)]; ok {
		m.Recorded = old.Recorded
	} else if m.Recorded.IsZero() {
		m.Recorded = time.Now()
	}
	s.poolMoves[poolMoveID(m)] = m
}

// PoolMoves lists the recorded moves, newest first; pool limits them to one
// pool's (its key) when not empty.
func (s *Store) PoolMoves(pool string) []PoolMove {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PoolMove, 0, len(s.poolMoves))
	for _, m := range s.poolMoves {
		if pool == "" || m.Pool == pool {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Recorded.After(out[j].Recorded) })
	return out
}

// ClearingRound is one clearing round as the relay saw it: the round orders it
// filled (each recorded with the price the round's last output publishes) and
// the pools it moved. State is the least settled of its records: undone when
// any is, then confirmed, then final.
type ClearingRound struct {
	Txid        string         `json:"txid"`
	Price       *RoundPrice    `json:"price"`
	Clearer     string         `json:"clearer,omitempty"`
	Block       string         `json:"block"`
	Height      int64          `json:"height"`
	AnchorDepth int64          `json:"anchor_depth"`
	State       string         `json:"state"`
	Fills       []CovenantFill `json:"fills"`
	PoolMoves   []PoolMove     `json:"pool_moves"`
	Recorded    time.Time      `json:"recorded"`
}

func worse(a, b string) string {
	rank := map[string]int{FillUndone: 0, FillConfirmed: 1, FillFinal: 2}
	if ra, ok := rank[a]; ok {
		if rb, ok := rank[b]; ok && rb < ra {
			return b
		}
		return a
	}
	return b
}

// ClearingRounds returns the clearing rounds the relay has records of, newest
// first, at most limit (0: all).
func (s *Store) ClearingRounds(limit int) []ClearingRound {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// A round undone by a reorg and mined again leaves an undone record beside
	// the new one for each of its fills: a round shows each fill once, at its
	// newest record.
	newest := map[string]*CovenantFill{}
	for _, ring := range s.fills {
		for _, f := range ring {
			if f.Taker.Via != TakerClearingRound {
				continue
			}
			id := fillID(f.Order, f.Txid, f.Input)
			if have := newest[id]; have == nil || f.Updated.After(have.Updated) {
				newest[id] = f
			}
		}
	}
	by := map[string]*ClearingRound{}
	for _, f := range newest {
		{
			r := by[f.Txid]
			if r == nil {
				r = &ClearingRound{Txid: f.Txid, Price: f.Taker.Price, Clearer: f.Taker.Clearer, State: f.State, Recorded: f.Recorded}
				by[f.Txid] = r
			}
			r.Fills = append(r.Fills, *f)
			r.Block, r.Height, r.AnchorDepth = f.Block, f.Height, f.AnchorDepth
			r.State = worse(r.State, f.State)
			if f.Recorded.Before(r.Recorded) {
				r.Recorded = f.Recorded
			}
		}
	}
	for _, m := range s.poolMoves {
		if r := by[m.Txid]; r != nil {
			r.PoolMoves = append(r.PoolMoves, m)
		}
	}
	out := make([]ClearingRound, 0, len(by))
	for _, r := range by {
		sort.Slice(r.Fills, func(i, j int) bool { return r.Fills[i].Input < r.Fills[j].Input })
		if r.PoolMoves == nil {
			r.PoolMoves = []PoolMove{}
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Recorded.After(out[j].Recorded) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
