package watcher

import (
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// InteractiveBook is the part of the book that holds the interactive fills the
// relay recorded when their makers acknowledged them (offerstore's
// WatchInteractiveFill). The watcher follows each one's transaction under the
// same finality rule as a covenant fill. A book without it has none to follow.
type InteractiveBook interface {
	InteractiveWatches() []offerstore.InteractiveWatch
	InteractiveRecord(k offerstore.Key, txid string, w offerstore.InteractiveWatch) (offerstore.CovenantFill, bool)
	SetInteractiveTraded(k offerstore.Key, txid string, traded bool)
}

// followInteractive measures every followed interactive fill against the live
// tip: pending until its transaction is in a block, then confirmed, final once
// certified and its anchor buried the required depth, undone (its trade
// retracted) when its block leaves the active chain, and recorded again with
// its trade when it is mined again.
func (w *Watcher) followInteractive(a AnchorTip, anchorKnown bool, st *Stats) {
	ib, ok := w.book.(InteractiveBook)
	if !ok {
		return
	}
	for _, iw := range ib.InteractiveWatches() {
		k := iw.Order
		ts, err := w.chain.TxStatus(iw.Txid)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s interactive fill %s: status: %v", k.MakerPubkey, k.OfferID, short(iw.Txid), err)
			continue
		}
		rec, live := ib.InteractiveRecord(k, iw.Txid, iw)
		if !ts.InChain {
			if live && rec.State != offerstore.FillPending {
				why := "its transaction left the active chain"
				if ts.InMempool {
					why = "its block left the active chain (its transaction is in the mempool again)"
				}
				rec.State, rec.Reason = offerstore.FillUndone, why
				w.book.UpdateFill(rec)
				ib.SetInteractiveTraded(k, iw.Txid, false)
				st.Undone++
				w.log.Printf("watcher: %s/%s interactive fill %s UNDONE: %s", k.MakerPubkey, k.OfferID, short(iw.Txid), why)
			}
			continue
		}
		required := w.finalDepth
		if iw.Required > required {
			required = iw.Required
		}
		depth, certified, final := finality(ts, a, anchorKnown, required)
		next := rec
		if !live {
			next = offerstore.CovenantFill{Order: k, Pair: iw.Offer.GetPair(), Txid: iw.Txid, Filled: iw.Filled,
				Taker: offerstore.FillTaker{Via: offerstore.TakerInteractive}}
		}
		next.Block, next.Height, next.Certified, next.AnchorDepth, next.RequiredDepth = ts.Block, ts.Height, certified, depth, required
		next.Reason = ""
		next.State = offerstore.FillConfirmed
		if final || (live && rec.State == offerstore.FillFinal && !anchorKnown) {
			next.State = offerstore.FillFinal
		}
		switch {
		case !live:
			w.book.RecordFill(next, nil)
			ib.SetInteractiveTraded(k, iw.Txid, true)
			w.log.Printf("watcher: %s/%s interactive fill %s mined again (%s)", k.MakerPubkey, k.OfferID, short(iw.Txid), next.State)
		case next.Block != rec.Block || next.Certified != rec.Certified || next.AnchorDepth != rec.AnchorDepth || next.State != rec.State:
			w.book.UpdateFill(next)
			if next.State == offerstore.FillFinal && rec.State != offerstore.FillFinal {
				st.Final++
				w.log.Printf("watcher: %s/%s interactive fill %s FINAL (certified, anchor depth %d >= %d)", k.MakerPubkey, k.OfferID, short(iw.Txid), depth, required)
			}
		}
	}
}

func (b *storeBook) InteractiveWatches() []offerstore.InteractiveWatch {
	return b.s.InteractiveWatches()
}

func (b *storeBook) InteractiveRecord(k offerstore.Key, txid string, w offerstore.InteractiveWatch) (offerstore.CovenantFill, bool) {
	return b.s.InteractiveRecord(k, w.Offer.GetPair(), txid)
}

func (b *storeBook) SetInteractiveTraded(k offerstore.Key, txid string, traded bool) {
	b.s.SetInteractiveTraded(k, txid, traded)
}
