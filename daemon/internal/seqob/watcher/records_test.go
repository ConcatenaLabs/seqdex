package watcher

import (
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// fakeRounds knows one operator's rounds.
type fakeRounds map[string][]uint32

func (f fakeRounds) Round(txid string, _ []covenant.Output) (RoundInfo, bool, error) {
	b, ok := f[txid]
	return RoundInfo{Operator: "0p", BatchVouts: b}, ok, nil
}

func pass(t *testing.T, w *Watcher) Stats {
	t.Helper()
	st, err := w.ReconcileOnce()
	if err != nil {
		t.Fatal(err)
	}
	if st.Errors != 0 {
		t.Fatalf("pass with errors: %+v", st)
	}
	return st
}

// Two orders filled by one round, k = 0 (in part) and k = 1 (whole): two
// records, each read from the round's outputs (credit at 2k, rest or gap at
// 2k+1), the taker named as the round's operator, not as an output.
func roundFillsTwo(t *testing.T) (*fakeChain, *fakeBook, *Watcher) {
	t.Helper()
	chain := newFakeChain()
	book := newFakeBook(mkEntry("A", "aa", 0, 90_000_000), mkEntry("B", "bb", 0, 60_000_000))
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 30_000_000}, {"bb", 0, 60_000_000, 60_000_000}}, "round1", 50, 100)
	w := New(chain, book, nil)
	w.SetRounds(fakeRounds{"round1": {4}})
	st := pass(t, w)
	if st.PartialFill != 1 || st.Filled != 1 {
		t.Fatalf("stats %+v", st)
	}
	return chain, book, w
}

func TestARoundsFillsAreRecordedFromItsOutputs(t *testing.T) {
	_, book, _ := roundFillsTwo(t)
	a, b := book.fillsOf("A"), book.fillsOf("B")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("records: %+v", book.fills)
	}
	fa, fb := a[0], b[0]
	if fa.Input != 0 || fa.Credit.Vout != 0 || fa.Rest == nil || fa.Rest.Vout != 1 || fa.Remainder != 60_000_000 || fa.Filled != 30_000_000 {
		t.Fatalf("order A's record: %+v", fa)
	}
	if fb.Input != 1 || fb.Credit.Vout != 2 || fb.Rest != nil || fb.Gap == nil || fb.Gap.Vout != 3 || fb.Filled != 60_000_000 {
		t.Fatalf("order B's record: %+v", fb)
	}
	if fa.Credit.Value != covenant.CeilPrice(30_000_000, 1, 3) || fa.Spent != "aa:0" || fb.Spent != "bb:0" {
		t.Fatalf("credit/spent: %+v / %+v", fa, fb)
	}
	for _, f := range []offerstore.CovenantFill{fa, fb} {
		if f.Taker.Via != offerstore.TakerArcaRound || f.Taker.Operator != "0p" || f.Taker.RoundTxid != "round1" ||
			len(f.Taker.BatchVouts) != 1 || f.Taker.BatchVouts[0] != 4 || !strings.Contains(f.Taker.Note, "no output of the round pays the taker") {
			t.Fatalf("taker: %+v", f.Taker)
		}
		if f.State != offerstore.FillConfirmed || f.AnchorDepth != 0 || f.RequiredDepth != DefaultFinalAnchorDepth || !f.Certified {
			t.Fatalf("finality fields: %+v", f)
		}
	}
	// Order A rests again at (round1, 2*0+1) with its rest; order B is held.
	if e, _ := book.entry("A"); e.Terms.GetCovenantTxid() != "round1" || e.Terms.GetCovenantVout() != 1 || e.Active != 60_000_000 || !e.ReRested {
		t.Fatalf("order A rests at %s:%d with %d", e.Terms.GetCovenantTxid(), e.Terms.GetCovenantVout(), e.Active)
	}
	if e, ok := book.entry("B"); !ok || e.Active != 0 {
		t.Fatalf("order B must be held (hidden) until its fill is final: %+v", e)
	}
}

func TestAnOnChainFillNamesNoOperator(t *testing.T) {
	chain := newFakeChain()
	book := newFakeBook(mkEntry("A", "aa", 0, 90_000_000))
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 30_000_000}}, "taker1", 50, 100)
	w := New(chain, book, nil)
	w.SetRounds(fakeRounds{"round1": {4}})
	pass(t, w)
	if f := book.fillsOf("A"); len(f) != 1 || f[0].Taker.Via != offerstore.TakerChain || f[0].Taker.Operator != "" {
		t.Fatalf("records: %+v", f)
	}
}

func TestAFillIsFinalOnlyCertifiedAndBuried(t *testing.T) {
	chain, book, w := roundFillsTwo(t)
	// One Bitcoin block above the fill's anchor: not yet.
	chain.anchor.Height = 101
	pass(t, w)
	if f := book.fillsOf("B")[0]; f.State != offerstore.FillConfirmed || f.AnchorDepth != 1 {
		t.Fatalf("at depth 1: %+v", f)
	}
	if _, ok := book.entry("B"); !ok {
		t.Fatal("order B removed before its fill is final")
	}
	// Buried two, but the block is not certified: still not.
	chain.anchor.Height = 102
	ts := chain.txs["round1"]
	ts.Certified = false
	chain.txs["round1"] = ts
	pass(t, w)
	if f := book.fillsOf("B")[0]; f.State != offerstore.FillConfirmed || f.Certified {
		t.Fatalf("uncertified: %+v", f)
	}
	// Certified, but the node has not checked the tip's anchor: still not.
	ts.Certified = true
	chain.txs["round1"] = ts
	chain.anchor.Status = "not_validated"
	pass(t, w)
	if f := book.fillsOf("B")[0]; f.State != offerstore.FillConfirmed {
		t.Fatalf("anchor not validated: %+v", f)
	}
	chain.anchor.Status = "ok"
	st := pass(t, w)
	if st.Final != 2 {
		t.Fatalf("stats %+v", st)
	}
	for _, id := range []string{"A", "B"} {
		if f := book.fillsOf(id)[0]; f.State != offerstore.FillFinal || f.AnchorDepth != 2 {
			t.Fatalf("order %s at depth 2: %+v", id, f)
		}
	}
	if _, ok := book.entry("B"); ok {
		t.Fatal("order B (filled whole, final) must leave the book")
	}
	if e, ok := book.entry("A"); !ok || e.Active != 60_000_000 {
		t.Fatal("order A (filled in part) rests on")
	}
}

func TestAnOffersMinAnchorDepthRaisesFinality(t *testing.T) {
	chain := newFakeChain()
	e := mkEntry("A", "aa", 0, 90_000_000)
	e.Offer.MinAnchorDepth = 5
	book := newFakeBook(e)
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 90_000_000}}, "t1", 50, 100)
	w := New(chain, book, nil)
	chain.anchor.Height = 102
	pass(t, w)
	if f := book.fillsOf("A")[0]; f.State != offerstore.FillConfirmed || f.RequiredDepth != 5 {
		t.Fatalf("%+v", f)
	}
	chain.anchor.Height = 105
	pass(t, w)
	if f := book.fillsOf("A")[0]; f.State != offerstore.FillFinal {
		t.Fatalf("%+v", f)
	}
}

// The round's block leaves the chain (an anchor rollback): both fills are
// undone, their trades retracted, order A rests again at its own outpoint
// with its whole size, order B (removed when its fill was final) comes back.
func TestARoundThatLeavesTheChainUndoesEveryFillInIt(t *testing.T) {
	chain, book, w := roundFillsTwo(t)
	chain.anchor.Height = 102
	pass(t, w) // both final, B removed
	chain.mempool("round1")
	st := pass(t, w)
	if st.Undone != 2 {
		t.Fatalf("stats %+v", st)
	}
	for _, id := range []string{"A", "B"} {
		fs := book.fillsOf(id)
		if len(fs) != 1 || fs[0].State != offerstore.FillUndone || !strings.Contains(fs[0].Reason, "its block left the active chain") {
			t.Fatalf("order %s's record: %+v", id, fs)
		}
	}
	if e, _ := book.entry("A"); e.Terms.GetCovenantTxid() != "aa" || e.Terms.GetCovenantVout() != 0 || e.ReRested {
		t.Fatalf("order A must rest again at aa:0: %s:%d rerested %v", e.Terms.GetCovenantTxid(), e.Terms.GetCovenantVout(), e.ReRested)
	}
	if e, ok := book.entry("B"); !ok || e.Terms.GetCovenantTxid() != "bb" {
		t.Fatalf("order B must be back at bb:0: %+v", e)
	}
	ops := strings.Join(book.ops("B"), ",")
	if !strings.Contains(ops, "update:undone,reopen,repoint") {
		t.Fatalf("order B's book operations: %s", ops)
	}
	// The round is in the mempool: both orders' outpoints are spent there, so the
	// orders are held, and nothing new is recorded.
	pass(t, w)
	if len(book.fills) != 2 {
		t.Fatalf("records after the undo: %+v", book.fills)
	}
	// It confirms again (same txid): recorded again, as new fills.
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 30_000_000}, {"bb", 0, 60_000_000, 60_000_000}}, "round1", 51, 102)
	pass(t, w)
	if a := book.fillsOf("A"); len(a) != 2 || a[1].State != offerstore.FillConfirmed || a[1].Txid != "round1" {
		t.Fatalf("order A's records: %+v", a)
	}
}

// A replaced round fills a subset of the orders, at other k: order B alone, at
// input 0. Order A, unspent again, is live at its whole size.
func TestAReplacementRoundIsRecordedAsNewFills(t *testing.T) {
	chain, book, w := roundFillsTwo(t)
	delete(chain.txs, "round1") // gone, not even in the mempool
	chain.unspent(t, "aa", 0, 90_000_000)
	chain.spend(t, []spent{{"bb", 0, 60_000_000, 60_000_000}}, "round2", 52, 101)
	w.SetRounds(fakeRounds{"round1": {4}, "round2": {2}})
	pass(t, w) // undo round1
	pass(t, w) // classify again
	b := book.fillsOf("B")
	if len(b) != 2 || b[0].State != offerstore.FillUndone || b[1].Txid != "round2" || b[1].Input != 0 || b[1].Credit.Vout != 0 ||
		b[1].Gap == nil || b[1].Gap.Vout != 1 || b[1].Taker.RoundTxid != "round2" {
		t.Fatalf("order B's records: %+v", b)
	}
	if !strings.Contains(b[0].Reason, "its transaction left the active chain") {
		t.Fatalf("reason: %q", b[0].Reason)
	}
	if e, _ := book.entry("A"); e.Active != 90_000_000 || e.Terms.GetCovenantTxid() != "aa" {
		t.Fatalf("order A: %+v", e)
	}
	if a := book.fillsOf("A"); len(a) != 1 || a[0].State != offerstore.FillUndone {
		t.Fatalf("order A's records: %+v", a)
	}
}

// A later fill of the rest is undone with the earlier one when the earlier
// one's block leaves the chain.
func TestUndoingAFillUndoesTheFillsOfItsRest(t *testing.T) {
	chain := newFakeChain()
	book := newFakeBook(mkEntry("A", "aa", 0, 90_000_000))
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 30_000_000}}, "f1", 50, 100)
	w := New(chain, book, nil)
	pass(t, w)
	chain.spend(t, []spent{{"f1", 1, 60_000_000, 20_000_000}}, "f2", 51, 100)
	pass(t, w)
	if e, _ := book.entry("A"); e.Terms.GetCovenantTxid() != "f2" || e.Active != 40_000_000 {
		t.Fatalf("order A after two fills: %+v", e)
	}
	chain.mempool("f1")
	delete(chain.txs, "f2")
	st := pass(t, w)
	if st.Undone != 2 {
		t.Fatalf("stats %+v", st)
	}
	// f1 is in the mempool again: order A is back at aa:0, held for that spend.
	if e, _ := book.entry("A"); e.Terms.GetCovenantTxid() != "aa" || e.Active != 0 {
		t.Fatalf("order A after the undo: %s:%d active %d", e.Terms.GetCovenantTxid(), e.Terms.GetCovenantVout(), e.Active)
	}
	if fs := book.fillsOf("A"); len(fs) != 2 || fs[0].State != offerstore.FillUndone || fs[1].State != offerstore.FillUndone {
		t.Fatalf("records: %+v", fs)
	}
	// f1 drops from the mempool too: the order is live at its whole size.
	delete(chain.txs, "f1")
	chain.unspent(t, "aa", 0, 90_000_000)
	pass(t, w)
	if e, _ := book.entry("A"); e.Terms.GetCovenantTxid() != "aa" || e.Active != 90_000_000 {
		t.Fatalf("order A once f1 is gone: %+v", e)
	}
}

func TestARefundRecordsNoFill(t *testing.T) {
	chain := newFakeChain()
	book := newFakeBook(mkEntry("A", "aa", 0, 90_000_000))
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 90_000_000}}, "refund1", 50, 100)
	o, _ := orderFromTerms(sampleTerms("x", 0))
	tap, _ := o.Derive()
	st := chain.states["aa:0"]
	st.Spender.Witness = [][]byte{make([]byte, 64), tap.RefundLeaf, tap.ControlBlock(o.InternalKey)}
	chain.states["aa:0"] = st
	w := New(chain, book, nil)
	pass(t, w)
	if len(book.fills) != 0 {
		t.Fatalf("a refund recorded a fill: %+v", book.fills)
	}
	chain.anchor.Height = 102
	pass(t, w)
	if ops := strings.Join(book.ops("A"), ","); ops != "hold,refunded" {
		t.Fatalf("ops %s", ops)
	}
}

// A chain without Bitcoin anchoring (a test chain) counts its own blocks.
func TestAChainWithoutAnchorsCountsItsBlocks(t *testing.T) {
	chain := newFakeChain()
	chain.anchor = AnchorTip{}
	book := newFakeBook(mkEntry("A", "aa", 0, 90_000_000))
	chain.spend(t, []spent{{"aa", 0, 90_000_000, 90_000_000}}, "t1", 50, 0)
	w := New(chain, book, nil)
	pass(t, w)
	ts := chain.txs["t1"]
	ts.Confirmations = 3
	chain.txs["t1"] = ts
	pass(t, w)
	if f := book.fillsOf("A")[0]; f.State != offerstore.FillFinal || f.AnchorDepth != 2 {
		t.Fatalf("%+v", f)
	}
}
