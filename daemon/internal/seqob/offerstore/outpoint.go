package offerstore

import (
	"errors"
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// One covenant order per outpoint. A covenant order is its coin: two offers
// naming one outpoint are one order counted twice, and a book that lists both
// shows depth that is not there. The outpoint is outside the signed bytes, so a
// maker that posts an order again under a new offer id (a seeder refreshing it
// before its expiry), or anyone re-posting an older signed offer of it, would
// otherwise list the same coin under a second key.
//
// The rule, wherever an order enters the book or moves to another outpoint:
//   - an offer from the same maker created later than the one listed at the
//     outpoint replaces it (the listed one leaves the book, CANCELLED); one
//     created at the same time or earlier is refused;
//   - an offer from another maker for a listed outpoint is refused.

// ErrOutpointListed refuses a covenant offer for an outpoint another offer
// already lists.
var ErrOutpointListed = errors.New("covenant outpoint already listed")

// outpointOf is a covenant offer's outpoint as the book indexes it.
func outpointOf(o *seqobv1.Offer) (string, bool) {
	txid, vout, ok := offer.CovenantOutpoint(o)
	if !ok || txid == "" {
		return "", false
	}
	return fmt.Sprintf("%s:%d", strings.ToLower(txid), vout), true
}

// claimLocked decides whether o, under key k, may rest at its outpoint: nil
// when no other key lists it; the key of this maker's earlier offer there,
// which leaves the book for it; or ErrOutpointListed. The caller holds s.mu.
func (s *Store) claimLocked(k Key, o *seqobv1.Offer) (*Key, error) {
	op, ok := outpointOf(o)
	if !ok {
		return nil, nil
	}
	k2, ok := s.covAt[op]
	if !ok || k2 == k {
		return nil, nil
	}
	e2, ok := s.entries[k2]
	if !ok {
		delete(s.covAt, op)
		return nil, nil
	}
	if k2.MakerPubkey != k.MakerPubkey {
		return nil, fmt.Errorf("%w: %s rests under another maker's offer %s/%s", ErrOutpointListed, op, k2.MakerPubkey, k2.OfferID)
	}
	if o.GetCreatedAtUnix() <= e2.Offer.GetCreatedAtUnix() {
		return nil, fmt.Errorf("%w: %s rests under this maker's offer %s, created at %d, not before this one's %d",
			ErrOutpointListed, op, k2.OfferID, e2.Offer.GetCreatedAtUnix(), o.GetCreatedAtUnix())
	}
	return &k2, nil
}

// evicted is an offer that left the book for a later one at its outpoint, to
// be announced once the lock is released.
type evicted struct {
	pair *seqobv1.AssetPair
	conf bool
	st   *seqobv1.OrderStatus
	key  Key
}

// evictLocked removes the offer at k, replaced by its maker's later offer for
// the same outpoint. The caller holds s.mu and announces the result.
func (s *Store) evictLocked(k Key) *evicted {
	e, ok := s.entries[k]
	if !ok {
		return nil
	}
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_CANCELLED
	ev := &evicted{pair: e.Offer.GetPair(), conf: e.Offer.GetConfidential(), st: e.orderStatus(k), key: k}
	s.remove(k)
	return ev
}

func (s *Store) announceEvicted(ev *evicted) {
	if ev == nil {
		return
	}
	s.broadcast(Event{Type: EventStatus, Pair: ev.pair, Confidential: ev.conf, Status: ev.st})
	s.broadcast(Event{Type: EventRemoved, Pair: ev.pair, Confidential: ev.conf, Ref: &seqobv1.OfferRef{OfferId: ev.key.OfferID, MakerPubkey: ev.key.MakerPubkey}})
}

// indexLocked and unindexLocked keep covAt, outpoint -> key, in step with the
// entries. The caller holds s.mu.
func (s *Store) indexLocked(k Key, o *seqobv1.Offer) {
	if op, ok := outpointOf(o); ok {
		s.covAt[op] = k
	}
}

func (s *Store) unindexLocked(k Key, o *seqobv1.Offer) {
	if op, ok := outpointOf(o); ok && s.covAt[op] == k {
		delete(s.covAt, op)
	}
}

// ListedAt is the key of the offer listed at a covenant outpoint, if any.
func (s *Store) ListedAt(txid string, vout uint32) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.covAt[fmt.Sprintf("%s:%d", strings.ToLower(txid), vout)]
	return k, ok
}
