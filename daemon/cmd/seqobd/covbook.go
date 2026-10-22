package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
)

// restoreCovenantBook lists again the covenant orders the relay kept in its
// covenant book file (-covenant-book) before it stopped. Each is checked as a
// posted offer is (validator.Readmit: signature, terms, expiry, its outpoint
// on the chain) and listed at what its outpoint holds. One whose outpoint was
// spent while the relay was down is put back held at 0, for the chain watcher
// to follow the spend: to the remainder a part fill made, or out of the book;
// a pool's side to the coins the pool rests at now. A pool's side whose coin
// holds nothing above its reserve is put back held at 0 too, as the follower
// kept it. Anything else the chain or the clock now refuses is dropped and
// named.
func restoreCovenantBook(path string, store *offerstore.Store, v *validator.Validator, logger *log.Logger) (listed, followed, dropped int) {
	kept, bad, err := offerstore.ReadCovenantBook(path)
	if err != nil {
		logger.Printf("covenant book %s: %v (starting without it)", path, err)
		return 0, 0, 0
	}
	for _, e := range bad {
		logger.Printf("covenant book %s: %v (skipped)", path, e)
		dropped++
	}
	for _, kc := range kept {
		o := kc.Offer
		txid, vout, _ := offer.CovenantOutpoint(o)
		if c, _, ok := offer.RestingLocator(o); ok {
			txid, vout = "coin "+c, 0
		}
		if t, n, ok := offer.PoolOwnCoin(o); ok {
			txid, vout = t, n
		}
		size, err := v.Readmit(context.Background(), o)
		// A pool's side sold down to its reserve stays in the book as it did
		// before the restart, held at 0, for the follower to list again.
		spent := errors.Is(err, watcher.ErrNotUnspent) || (offer.IsPool(o) && errors.Is(err, watcher.ErrPoolDrained))
		if err != nil && !spent {
			logger.Printf("covenant book: %s/%s at %s:%d not listed again: %v", o.GetMakerPubkey(), o.GetOfferId(), txid, vout, err)
			dropped++
			continue
		}
		if spent && offer.IsResting(o) {
			spent = false
		}
		if err == nil && size == 0 {
			// The node could not be asked: the size it was listed at (held, if it
			// was held), until the watcher reads the chain.
			size, spent = kc.Active, kc.Active == 0
		}
		if _, err := store.RestoreCovenant(o, size, kc.ReRested, spent); err != nil {
			// One offer per outpoint (offerstore.ErrOutpointListed) holds here too.
			logger.Printf("covenant book: %s/%s at %s:%d not listed again: %v", o.GetMakerPubkey(), o.GetOfferId(), txid, vout, err)
			dropped++
			continue
		}
		if spent {
			logger.Printf("covenant book: %s/%s at %s:%d held at 0 for the watcher to follow (%v)", o.GetMakerPubkey(), o.GetOfferId(), txid, vout, heldWhy(err))
			followed++
		} else {
			listed++
		}
	}
	return listed, followed, dropped
}

// keepCovenantBook writes the covenant book file every interval while the
// covenant orders change, until stop closes, and once more then.
func keepCovenantBook(w *offerstore.CovenantBookWriter, store *offerstore.Store, every time.Duration, stop <-chan struct{}, done chan<- struct{}, logger *log.Logger) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	flush := func() {
		if _, err := w.Flush(store); err != nil {
			logger.Printf("covenant book %s: %v", w.Path, err)
		}
	}
	for {
		select {
		case <-stop:
			flush()
			return
		case <-t.C:
			flush()
		}
	}
}

func heldWhy(err error) string {
	if errors.Is(err, watcher.ErrPoolDrained) {
		return "its coin holds nothing above its reserve: " + err.Error()
	}
	if err != nil {
		return "spent while the relay was down: " + err.Error()
	}
	return "it was held when the relay stopped, and the node cannot be asked now"
}
