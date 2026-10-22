package watcher

import (
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// An interactive lift's settling transaction, followed by the watcher over the
// relay's own store: pending, confirmed, final once certified and buried, undone
// (its trade retracted) when an anchor reorg takes its block, and recorded again
// with its trade when it is mined again.
func TestInteractiveFillFollowsTheChain(t *testing.T) {
	store := offerstore.New(time.Now)
	pair := &seqobv1.AssetPair{BaseAsset: "aa", QuoteAsset: "bb"}
	o := &seqobv1.Offer{OfferId: "o1", MakerPubkey: "m1", Pair: pair, TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount: 1000, OfferAsset: "aa", OfferAmount: 1000, WantAsset: "bb", WantAmount: 370,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "tb1q"}}}
	k := offerstore.Key{MakerPubkey: "m1", OfferID: "o1"}
	// What the relay does on the maker's SettleAck.
	store.RecordTradeTx(o, 100, "tx1")
	store.WatchInteractiveFill(k, o, 100, "tx1")

	chain := newFakeChain()
	w := New(chain, NewStoreBook(store), nil)
	w.SetFinalAnchorDepth(2)
	state := func(want string) offerstore.CovenantFill {
		t.Helper()
		if _, err := w.ReconcileOnce(); err != nil {
			t.Fatal(err)
		}
		fs := store.CovenantFills(pair, 0)
		last := fs[len(fs)-1]
		if last.Txid != "tx1" || last.Taker.Via != offerstore.TakerInteractive || last.State != want {
			t.Fatalf("want the last record tx1 interactive %s, got %+v (all %d)", want, last, len(fs))
		}
		return last
	}
	trades := func() int { return len(store.TradesFor(pair, 0)) }

	state(offerstore.FillPending) // not yet in a block
	chain.txs["tx1"] = TxStatus{InChain: true, Block: "b1", Height: 101, Confirmations: 1, BlockAnchor: 99, CertPresent: true, Certified: true}
	if f := state(offerstore.FillConfirmed); f.AnchorDepth != 1 || f.RequiredDepth != 2 {
		t.Fatalf("depth %d of %d", f.AnchorDepth, f.RequiredDepth)
	}
	chain.anchor.Height = 101
	state(offerstore.FillFinal)
	if trades() != 1 {
		t.Fatalf("one trade while it stands, got %d", trades())
	}

	// An anchor reorg takes its block; the transaction waits in the mempool.
	chain.txs["tx1"] = TxStatus{InMempool: true}
	if f := state(offerstore.FillUndone); f.Reason == "" {
		t.Fatal("an undone fill says why")
	}
	if trades() != 0 {
		t.Fatalf("its trade is retracted, got %d", trades())
	}
	state(offerstore.FillUndone) // and stays so while it is not in a block

	// Mined again: a new record, and its trade again.
	chain.txs["tx1"] = TxStatus{InChain: true, Block: "b2", Height: 103, Confirmations: 1, BlockAnchor: 101, CertPresent: true, Certified: true}
	if f := state(offerstore.FillConfirmed); f.Block != "b2" {
		t.Fatalf("recorded in its new block, got %s", f.Block)
	}
	if n := len(store.CovenantFills(pair, 0)); n != 2 {
		t.Fatalf("the undone record is kept beside the new one: %d records", n)
	}
	if trades() != 1 {
		t.Fatalf("its trade is recorded again, got %d", trades())
	}
	chain.anchor.Height = 103
	state(offerstore.FillFinal)

	// Uncertified, it is not final however deep.
	store.WatchInteractiveFill(k, o, 50, "tx2")
	chain.txs["tx2"] = TxStatus{InChain: true, Block: "b3", Height: 104, Confirmations: 1, BlockAnchor: 90, CertPresent: true, Certified: false}
	if _, err := w.ReconcileOnce(); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.CovenantFills(pair, 0) {
		if f.Txid == "tx2" && f.State != offerstore.FillConfirmed {
			t.Fatalf("an uncertified block is not final: %s", f.State)
		}
	}
}
