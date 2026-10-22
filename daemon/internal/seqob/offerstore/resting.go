package offerstore

import (
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"google.golang.org/protobuf/proto"
)

// --- orders resting in an operator's tree ---------------------------------------
//
// An order resting as a leaf in an Arca operator's tree (RestingLeafTerms) is
// listed at what the coin it rests at holds, as the operator's state view
// reports it, and the relay's follower (watcher.Resting) moves it as the view
// changes: to the coin a part fill rested the remainder at, out of the book
// when it is filled, cancelled, landed or past its delist time. Its fills are
// recorded as CovenantFill records keyed by the operator's transfer id
// (TakerOperatorTree), since an in-tree fill has no txid until it reaches the
// chain.

// TakerOperatorTree: the order was filled in an Arca operator's tree, out of round,
// by a taker giving up its own leaves; the record names the operator, the
// transfer, and the round the order's lineage rests on, whose finality the
// fill's is.
const TakerOperatorTree = "operator_tree"

// RestingEntry is a listed order resting in an operator's tree, for the
// follower to check against its operator.
type RestingEntry struct {
	Key    Key
	Offer  *seqobv1.Offer // the live pointer; read only
	Active uint64
}

// SnapshotResting returns every listed order resting in an operator's tree.
func (s *Store) SnapshotResting() []RestingEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RestingEntry, 0)
	for k, e := range s.entries {
		if offer.IsResting(e.Offer) {
			out = append(out, RestingEntry{Key: k, Offer: e.Offer, Active: e.ActiveAmount})
		}
	}
	return out
}

// RepointResting points a listed resting order at the coin (leaf id) its
// operator reports it resting at now, holding value atoms of the asset it
// sells, and lists it at that size (never above what its maker signed). The
// signed terms are unchanged: the locator is outside them. Records no trade.
// No-op for a missing entry or an unchanged one.
func (s *Store) RepointResting(k Key, coin string, value uint64) {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsResting(e.Offer) {
		s.mu.Unlock()
		return
	}
	if value > e.Offer.GetBaseAmount() {
		value = e.Offer.GetBaseAmount()
	}
	c, v, _ := offer.RestingLocator(e.Offer)
	if strings.EqualFold(c, coin) && v == value && e.ActiveAmount == value {
		s.mu.Unlock()
		return
	}
	no := proto.Clone(e.Offer).(*seqobv1.Offer)
	offer.SetRestingLocator(no, coin, value)
	s.unindexLocked(k, e.Offer)
	e.Offer = no
	s.indexLocked(k, no)
	e.ActiveAmount = value
	e.reRested = !strings.EqualFold(coin, no.GetRestingLeaf().GetOrderId())
	if value >= no.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	pair, conf, st := no.GetPair(), no.GetConfidential(), e.orderStatus(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventCreated, Pair: pair, Confidential: conf, Offer: no})
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
}

// HoldResting lists a resting order at 0 while its operator cannot be asked:
// nothing can fill it then, so the book shows nothing of it, but it keeps its
// place and returns at the next answer. No-op when already held.
func (s *Store) HoldResting(k Key) {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsResting(e.Offer) || e.ActiveAmount == 0 {
		s.mu.Unlock()
		return
	}
	e.ActiveAmount = 0
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	pair, conf, st := e.Offer.GetPair(), e.Offer.GetConfidential(), e.orderStatus(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
}

// RemoveResting delists a resting order with status: FILLED (filled whole),
// CANCELLED (its maker took it back, or its operator no longer reports it) or
// EXPIRED (landed, used up, or at its delist time). The fill records stay.
func (s *Store) RemoveResting(k Key, status seqobv1.OfferStatus) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsResting(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	e.ActiveAmount = 0
	s.mu.Unlock()
	return s.dropWithStatus(k, status)
}
