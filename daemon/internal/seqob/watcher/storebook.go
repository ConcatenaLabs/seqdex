package watcher

import (
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// storeBook adapts an *offerstore.Store to the BookController interface. It is
// the in-process wiring the relay uses (the watcher runs as a goroutine holding
// the same store). A remote deployment would implement BookController over an
// admin RPC to the relay instead; the reconciliation core is agnostic.
type storeBook struct{ s *offerstore.Store }

// NewStoreBook returns a BookController backed by store.
func NewStoreBook(store *offerstore.Store) BookController { return &storeBook{s: store} }

func (b *storeBook) SnapshotCovenants() []CovEntry {
	src := b.s.SnapshotCovenants()
	out := make([]CovEntry, 0, len(src))
	for _, c := range src {
		out = append(out, CovEntry{Key: c.Key, Offer: c.Offer, Terms: c.Terms, Sim: c.Sim, Active: c.Active, ReRested: c.ReRested})
	}
	return out
}

func (b *storeBook) ReconcileLive(k offerstore.Key, active uint64) error {
	return b.s.ReconcileCovenantActive(k, active)
}

func (b *storeBook) Repoint(k offerstore.Key, txid string, vout uint32, size uint64, reRested bool) error {
	return b.s.RepointCovenant(k, txid, vout, size, reRested)
}

func (b *storeBook) RemoveRefunded(k offerstore.Key, spenderTxid string) error {
	return b.s.RemoveCovenantRefunded(k, spenderTxid)
}

func (b *storeBook) Reopen(o *seqobv1.Offer, active uint64) error { return b.s.Reopen(o, active) }

func (b *storeBook) RecordFill(f offerstore.CovenantFill, resting *seqobv1.Offer) {
	b.s.RecordCovenantFill(f, resting)
}

func (b *storeBook) UpdateFill(f offerstore.CovenantFill) { b.s.UpdateCovenantFill(f) }

func (b *storeBook) SetAnchorConfs(k offerstore.Key, depth uint32) {
	b.s.SetCovenantAnchorConfs(k, depth)
}

func (b *storeBook) RemoveFilled(k offerstore.Key, spenderTxid string) error {
	return b.s.RemoveCovenantFilled(k, spenderTxid)
}

func (b *storeBook) HoldForSpend(k offerstore.Key, spenderTxid string) error {
	return b.s.HoldCovenantForSpend(k, spenderTxid)
}

func (b *storeBook) RemoveGhost(k offerstore.Key, reason string) error {
	// A ghost's funding UTXO is gone at the tip: map to UTXO_INVALIDATED, the
	// existing "the maker's coins moved / are unusable" removal.
	return b.s.MarkInvalidated(k)
}

func (b *storeBook) HoldGhost(k offerstore.Key, reason string) error {
	return b.s.HoldCovenantGhost(k, reason)
}
