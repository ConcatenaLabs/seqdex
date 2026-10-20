package offerstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func mkOffer(t *testing.T, k *btcec.PrivateKey, id string, expires uint64) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: expires,
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

// TestSnapshotPairNamespaceDisjoint asserts the transparent and confidential books
// are disjoint views over the same pair: SnapshotPair(false) returns only
// transparent offers, SnapshotPair(true) only confidential ones.
func TestSnapshotPairNamespaceDisjoint(t *testing.T) {
	s := New(nil)
	k := key(t)

	transparent := mkOffer(t, k, "t1", 1799999999)
	conf := mkOffer(t, k, "c1", 1799999999)
	conf.Confidential = true
	conf.MakerSig = nil
	if err := offer.SignOffer(conf, k); err != nil {
		t.Fatalf("sign confidential: %v", err)
	}
	if _, err := s.Submit(transparent); err != nil {
		t.Fatalf("submit transparent: %v", err)
	}
	if _, err := s.Submit(conf); err != nil {
		t.Fatalf("submit confidential: %v", err)
	}

	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}
	pub := s.SnapshotPair(pair, false)
	if len(pub) != 1 || pub[0].GetOfferId() != "t1" {
		t.Fatalf("transparent book must contain only t1, got %+v", pub)
	}
	blinded := s.SnapshotPair(pair, true)
	if len(blinded) != 1 || blinded[0].GetOfferId() != "c1" {
		t.Fatalf("confidential book must contain only c1, got %+v", blinded)
	}
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestInteractiveOversellDemotion covers P3.10 soft accounting: an interactive
// offer that pushes a maker's cumulative committed capital in an asset past the soft
// cap is DEMOTED (advisory), earlier offers stay non-demoted, and covenant offers are
// exempt (funded on-chain, never counted or demoted).
func TestInteractiveOversellDemotion(t *testing.T) {
	s := New(nil)
	s.SetInteractiveCap(150) // soft cap: 150 atoms of committed 'gold' per maker
	k := key(t)

	o1 := mkOffer(t, k, "o1", 1799999999) // SELL, offer_asset gold, offer_amount 100
	maker := o1.GetMakerPubkey()
	if _, err := s.Submit(o1); err != nil {
		t.Fatalf("submit o1: %v", err)
	}
	if c := s.CommittedInteractiveCapital(maker, "gold"); c != 100 {
		t.Fatalf("committed after o1 = %d, want 100", c)
	}
	if st, _ := s.OrderStatusOf(Key{MakerPubkey: maker, OfferID: "o1"}); st.GetDemoted() {
		t.Fatal("o1 (100 <= 150 cap) must not be demoted")
	}

	o2 := mkOffer(t, k, "o2", 1799999999) // another 100 -> cumulative 200 > 150
	if _, err := s.Submit(o2); err != nil {
		t.Fatalf("submit o2: %v", err)
	}
	if c := s.CommittedInteractiveCapital(maker, "gold"); c != 200 {
		t.Fatalf("committed after o2 = %d, want 200", c)
	}
	if st, _ := s.OrderStatusOf(Key{MakerPubkey: maker, OfferID: "o2"}); !st.GetDemoted() {
		t.Fatal("o2 (pushes committed to 200 > 150) must be demoted")
	}
	if st, _ := s.OrderStatusOf(Key{MakerPubkey: maker, OfferID: "o1"}); st.GetDemoted() {
		t.Fatal("o1 must remain non-demoted (only the marginal over-cap offer is flagged)")
	}

	// A covenant offer for the same maker+asset is EXEMPT: never counted, never demoted.
	cov := mkCovOffer(t, k, "cov", "txcov", 0, 1000) // offer_asset gold, offer_amount 1000
	if _, err := s.Submit(cov); err != nil {
		t.Fatalf("submit cov: %v", err)
	}
	if c := s.CommittedInteractiveCapital(maker, "gold"); c != 200 {
		t.Fatalf("covenant must not count toward committed capital, got %d", c)
	}
	if st, _ := s.OrderStatusOf(Key{MakerPubkey: maker, OfferID: "cov"}); st.GetDemoted() {
		t.Fatal("a covenant offer must never be demoted")
	}
}

// TestRetractTradeRemovesTradeAndFixesLastPrice covers the trade-feed half of P3.9
// reorg-undo: retracting a settle txid removes its trade(s) from the ring, so
// last_price (the newest ring entry) falls back to the surviving trade.
func TestRetractTradeRemovesTradeAndFixesLastPrice(t *testing.T) {
	s := New(nil)
	k := key(t)
	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}
	// ask 0.45 (want/offer = 45/100); a second offer at 0.60 for a distinct last price.
	sellA := mkOffer(t, k, "s1", 1799999999) // 0.45
	sellB := mkOffer(t, k, "s2", 1799999999)
	sellB.WantAmount = 60
	sellB.MakerSig = nil
	if err := offer.SignOffer(sellB, k); err != nil {
		t.Fatal(err)
	}
	s.RecordTradeTx(sellA, 10, "txA") // price 0.45
	s.RecordTradeTx(sellB, 20, "txB") // price 0.60 (newest -> last_price)
	if last := lastPrice(s, pair); last != 0.60 {
		t.Fatalf("last_price before retract = %v, want 0.60", last)
	}
	if n := s.RetractTrade(pair, "txB"); n != 1 {
		t.Fatalf("retract removed %d, want 1", n)
	}
	tr := s.TradesFor(pair, 0)
	if len(tr) != 1 || tr[0].Txid != "txA" {
		t.Fatalf("after retract want only txA, got %+v", tr)
	}
	if last := lastPrice(s, pair); last != 0.45 {
		t.Fatalf("last_price after retract = %v, want 0.45 (fell back to txA)", last)
	}
	if n := s.RetractTrade(pair, "nope"); n != 0 {
		t.Fatalf("retracting a non-existent txid removed %d, want 0", n)
	}
}

// TestRetractTradeSurvivesRestart covers the durability half of P3.9: a retraction is
// appended to the JSONL, so a relay restart's replay does not resurrect the reorged
// trade.
func TestRetractTradeSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}
	k := key(t)
	sell := mkOffer(t, k, "s1", 1799999999)

	s1 := New(nil)
	if err := s1.EnableTradeLog(path); err != nil {
		t.Fatal(err)
	}
	s1.RecordTradeTx(sell, 10, "txA")
	s1.RecordTradeTx(sell, 20, "txB")
	if n := s1.RetractTrade(pair, "txB"); n != 1 {
		t.Fatalf("retract removed %d, want 1", n)
	}

	// Restart: a fresh store replaying the same log must contain only txA.
	s2 := New(nil)
	if err := s2.EnableTradeLog(path); err != nil {
		t.Fatal(err)
	}
	tr := s2.TradesFor(pair, 0)
	if len(tr) != 1 || tr[0].Txid != "txA" {
		t.Fatalf("after restart want only txA, got %+v", tr)
	}
}

// lastPrice reads a pair's last_price — the newest trade in the ring, exactly the
// source Markets().LastPrice reads.
func lastPrice(s *Store, pair *seqobv1.AssetPair) float64 {
	tr := s.TradesFor(pair, 0)
	if len(tr) == 0 {
		return 0
	}
	return tr[len(tr)-1].Price
}

// TestSubscriberDeltaSeqGap covers P3.8 delta sequencing: each delivered delta carries
// a monotonic per-subscription Seq, and a delta DROPPED on a buffer overflow leaves a
// hole in the delivered sequence — the gap the WS forwarder turns into a Resync.
func TestSubscriberDeltaSeqGap(t *testing.T) {
	s := New(nil)
	s.SetSubBuf(2) // tiny buffer so a 3rd un-drained delta overflows deterministically
	k := key(t)
	_, id, ch := s.Subscribe(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}, false)
	defer s.Unsubscribe(id)

	sub := func(idStr string) {
		if _, err := s.Submit(mkOffer(t, k, idStr, 1799999999)); err != nil {
			t.Fatalf("submit %s: %v", idStr, err)
		}
	}
	// Two creates fill the buffer (seq 1, 2); a third overflows and is DROPPED (seq 3).
	sub("o1")
	sub("o2")
	sub("o3") // dropped: buffer full
	if e := recv(t, ch); e.Seq != 1 {
		t.Fatalf("first delta seq = %d, want 1", e.Seq)
	}
	if e := recv(t, ch); e.Seq != 2 {
		t.Fatalf("second delta seq = %d, want 2", e.Seq)
	}
	// The buffer has drained; a fourth create fits (seq 4). Its delivery shows a GAP
	// (expected 3, got 4) — seq 3 was dropped.
	sub("o4")
	if e := recv(t, ch); e.Seq != 4 {
		t.Fatalf("post-overflow delta seq = %d, want 4 (seq 3 dropped -> gap)", e.Seq)
	}
}

func TestSubmitVerifiesAndSnapshot(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := s.Submit(o); err == nil {
		t.Fatalf("expected duplicate submit to fail")
	}
	snap := s.SnapshotPair(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}, false)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// Tampered offer must be rejected on submit.
	bad := mkOffer(t, k, "bbbb", 1750003600)
	bad.WantAmount = 1 // invalidates the signature
	if _, err := s.Submit(bad); err == nil {
		t.Fatalf("expected tampered offer to be rejected")
	}
}

func TestSnapshotPlusDeltas(t *testing.T) {
	s := New(nil)
	k := key(t)
	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}

	// Pre-existing offer appears in the snapshot.
	o1 := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o1); err != nil {
		t.Fatal(err)
	}
	snap, id, ch := s.Subscribe(pair, false)
	defer s.Unsubscribe(id)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// A subsequent submit arrives as a created delta.
	o2 := mkOffer(t, k, "bbbb", 1750003600)
	if _, err := s.Submit(o2); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, ch)
	if ev.Type != EventCreated || ev.Offer.GetOfferId() != "bbbb" {
		t.Fatalf("expected created delta for bbbb, got %+v", ev)
	}

	// Cancel arrives as a removed delta.
	c := &seqobv1.OfferCancel{OfferId: "bbbb", Nonce: 1}
	if err := offer.SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	ev = recv(t, ch)
	if ev.Type != EventRemoved || ev.Ref.GetOfferId() != "bbbb" {
		t.Fatalf("expected removed delta for bbbb, got %+v", ev)
	}
}

func TestCancelNonceReplay(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	c := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: 5}
	if err := offer.SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Re-post the same offer_id, then replay the old cancel: must be rejected.
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("re-submit: %v", err)
	}
	if err := s.Cancel(c); err == nil {
		t.Fatalf("expected replayed cancel (stale nonce) to be rejected")
	}
	// A fresh higher-nonce cancel works.
	c2 := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: 6}
	if err := offer.SignCancel(c2, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c2); err != nil {
		t.Fatalf("fresh cancel: %v", err)
	}
}

func TestCancelWrongSigner(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	// Cancel signed by a different key references the maker's offer_id+pubkey:
	// the signature will not verify against that maker_pubkey.
	other := key(t)
	c := &seqobv1.OfferCancel{OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), Nonce: 1}
	if err := offer.SignCancel(c, other); err == nil {
		t.Fatalf("expected SignCancel to reject mismatched maker_pubkey")
	}
}

func TestExpirySweeper(t *testing.T) {
	now := time.Unix(1750000000, 0)
	s := New(func() time.Time { return now })
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750000100) // expires 100s after now
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	if n := s.SweepExpired(); n != 0 {
		t.Fatalf("nothing should expire yet, swept %d", n)
	}
	now = time.Unix(1750000200, 0) // advance past expiry
	if n := s.SweepExpired(); n != 1 {
		t.Fatalf("expected 1 expired, swept %d", n)
	}
	if len(s.SnapshotPair(o.GetPair(), false)) != 0 {
		t.Fatalf("expired offer still in book")
	}
}

func TestPartialFillThenFilled(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600) // base 100, min_anchor_depth 0
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPartialFill(kk, 60, "txid1", 0); err != nil {
		t.Fatalf("partial fill: %v", err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 40 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL {
		t.Fatalf("after partial: %+v ok=%v", e, ok)
	}
	// Fill the rest: 0-conf tolerant (min_anchor_depth=0) so it reaches FILLED.
	if err := s.ApplyPartialFill(kk, 40, "txid2", 0); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("filled offer should be removed from the book")
	}
}

// TestRecordSettledFillRecordsDecrementsAndGuardsReplay is the P2.8 core: a durable
// cross/LN lift reports settlement off the courier path via RecordSettledFill, and the
// relay must record the trade at the resting order's price and decrement the order —
// exactly what the interactive SettleAck does, but keyed to the offer and replay-guarded
// by a strictly-increasing nonce (the offer_id is reused across -requote).
func TestRecordSettledFillRecordsDecrementsAndGuardsReplay(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600) // SELL gold/usdx, base 100, want 45 -> price 0.45, min_anchor_depth 0
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	pair := o.GetPair()

	// A partial settled fill of 40: records a trade of 40 at 0.45 and decrements to 60.
	if err := s.RecordSettledFill(kk, 40, "txid1", 0, 1); err != nil {
		t.Fatalf("record settled (partial): %v", err)
	}
	trades := s.TradesFor(pair, 0)
	if len(trades) != 1 || trades[0].Size != 40 || trades[0].Price != 0.45 {
		t.Fatalf("want one trade {size:40, price:0.45}, got %+v", trades)
	}
	if e, ok := s.Get(kk); !ok || e.ActiveAmount != 60 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL {
		t.Fatalf("after partial settled fill: %+v ok=%v", e, ok)
	}

	// Replay of the same (or lower) nonce is rejected — no second trade, no over-decrement.
	if err := s.RecordSettledFill(kk, 40, "txid1", 0, 1); err == nil {
		t.Fatalf("expected stale-nonce rejection on replay")
	}
	if trades := s.TradesFor(pair, 0); len(trades) != 1 {
		t.Fatalf("replay must not record a second trade, got %d", len(trades))
	}

	// The remaining 60 settles (higher nonce, min_anchor_depth 0 -> FILLED + removed).
	if err := s.RecordSettledFill(kk, 60, "txid2", 0, 2); err != nil {
		t.Fatalf("record settled (rest): %v", err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("fully-settled offer should be removed from the book")
	}
	if total := s.TradesFor(pair, 0); len(total) != 2 || total[1].Size != 60 {
		t.Fatalf("want two trades totalling base 100, got %+v", total)
	}

	// A settled report for an already-gone offer is a safe no-op (still nonce-guarded).
	if err := s.RecordSettledFill(kk, 10, "txid3", 0, 3); err != nil {
		t.Fatalf("settled fill on a removed offer should be a no-op, got %v", err)
	}
	if trades := s.TradesFor(pair, 0); len(trades) != 2 {
		t.Fatalf("no-op settled fill must not record a trade, got %d", len(trades))
	}
}

// TestRecordSettledFillAnchorGatesFinalize proves anchor_confs is honored end to end: a
// whole fill reported at/above the offer's min_anchor_depth reaches FILLED and is removed,
// while the same fill reported BELOW the bar records the trade but is held (active 0, not
// removed). This is the wiring that lets a min_anchor_depth>0 offer finalize at all — the
// maker reports its finality bar in one shot (anchor_confs = min_anchor_depth); previously
// the relay hard-coded 0 confs, so such an offer could never reach FILLED.
func TestRecordSettledFillAnchorGatesFinalize(t *testing.T) {
	mkAnchored := func(t *testing.T, s *Store, k *btcec.PrivateKey, id string) (Key, *seqobv1.AssetPair) {
		t.Helper()
		o := mkOffer(t, k, id, 1750003600)
		o.MinAnchorDepth = 2
		o.MakerSig = nil
		if err := offer.SignOffer(o, k); err != nil {
			t.Fatalf("sign: %v", err)
		}
		kk, err := s.Submit(o)
		if err != nil {
			t.Fatal(err)
		}
		return kk, o.GetPair()
	}

	// At the bar (confs 2 >= min_anchor_depth 2): FILLED + removed, trade recorded.
	s := New(nil)
	kk, pair := mkAnchored(t, s, key(t), "atbar")
	if err := s.RecordSettledFill(kk, 100, "txid1", 2, 1); err != nil {
		t.Fatalf("record settled (at bar): %v", err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("fill at/above the anchor bar should finalize and be removed")
	}
	if tr := s.TradesFor(pair, 0); len(tr) != 1 || tr[0].Size != 100 {
		t.Fatalf("want one trade of 100 at/above the bar, got %+v", tr)
	}

	// Below the bar (confs 1 < min_anchor_depth 2): trade recorded, but held (active 0, not removed).
	s2 := New(nil)
	kk2, pair2 := mkAnchored(t, s2, key(t), "belowbar")
	if err := s2.RecordSettledFill(kk2, 100, "txid1", 1, 1); err != nil {
		t.Fatalf("record settled (below bar): %v", err)
	}
	e, ok := s2.Get(kk2)
	if !ok || e.ActiveAmount != 0 || e.Status == seqobv1.OfferStatus_OFFER_STATUS_FILLED {
		t.Fatalf("below the anchor bar the fill must record but not finalize: %+v ok=%v", e, ok)
	}
	if tr := s2.TradesFor(pair2, 0); len(tr) != 1 || tr[0].Size != 100 {
		t.Fatalf("want one trade of 100 recorded below the bar, got %+v", tr)
	}
}

func TestReopenRestoresOrder(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a fill, then an anchor orphan that re-opens the order.
	if err := s.ApplyPartialFill(kk, 100, "txid", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("expected offer removed after fill")
	}
	if err := s.Reopen(o, 100); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 100 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_OPEN {
		t.Fatalf("after reopen: %+v ok=%v", e, ok)
	}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delta")
		return Event{}
	}
}

// mkCovOffer builds a signed covenant SELL offer resting at outpoint txid:vout.
func mkCovOffer(t *testing.T, k *btcec.PrivateKey, id, txid string, vout uint32, base uint64) *seqobv1.Offer {
	t.Helper()
	b32 := func(f byte) []byte {
		o := make([]byte, 32)
		for i := range o {
			o[i] = f
		}
		return o
	}
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    base,
		OfferAmount:   base,
		OfferAsset:    "gold",
		WantAmount:    base / 3,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: 1750003600,
		Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
			CovenantTxid: txid, CovenantVout: vout,
			AssetA: hexOf(b32(0xa1)), AssetB: hexOf(b32(0xb2)),
			RateNum: 1, RateDen: 3, MakerProg: b32(0xc3), MakerProgVer: 1,
			MinLot: 5, ExpiryLocktime: 500, MakerX: b32(0xd4), InternalKey: b32(0xee),
		}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = h[c>>4]
		out[i*2+1] = h[c&0xf]
	}
	return string(out)
}

func TestCovenantReconcile(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// SnapshotCovenants sees it.
	covs := s.SnapshotCovenants()
	if len(covs) != 1 || covs[0].Terms.GetCovenantTxid() != "aabb" {
		t.Fatalf("snapshot covenants: %+v", covs)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}

	// Optimistic decrement then LIVE reconcile back to on-chain value re-opens it.
	if err := s.ApplyPartialFill(kk, 30, "", 0); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 60 {
		t.Fatalf("after optimistic fill active=%d want 60", e.ActiveAmount)
	}
	if err := s.ReconcileCovenantActive(kk, 90); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 90 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_OPEN {
		t.Fatalf("live reconcile: active=%d status=%v", e.ActiveAmount, e.Status)
	}

	// Hold for an unconfirmed spend: active drops to 0 (hidden from matcher).
	if err := s.HoldCovenantForSpend(kk, "spendtx"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 0 || e.SettleTxid != "spendtx" {
		t.Fatalf("hold: active=%d settle=%s", e.ActiveAmount, e.SettleTxid)
	}

	// Re-rest the remainder at a NEW outpoint with reduced size.
	if err := s.RerestCovenantRemainder(kk, "ccdd", 1, 60); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 60 || e.Offer.GetCovenant().GetCovenantTxid() != "ccdd" || e.Offer.GetCovenant().GetCovenantVout() != 1 {
		t.Fatalf("re-rest: %+v", e)
	}
	// Original submitted offer pointer must be untouched (clone-on-write).
	if o.GetCovenant().GetCovenantTxid() != "aabb" {
		t.Fatalf("re-rest mutated the original offer pointer")
	}

	// Full fill removes it.
	if err := s.RemoveCovenantFilled(kk, "filltx"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("filled covenant should be removed")
	}
}

// A covenant fill's trade is recorded with its fill record, at the size the chain shows (locked less
// the rest), with the fill's txid; re-pointing or removing the order records none; undoing the fill
// retracts its trade, so the feed and the candles follow the chain. Drives: a partial fill of 30
// (rests again with 60), a full fill of the 60, then the full fill undone.
func TestCovenantFillRecordsCarryTheTrades(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}
	pair := o.GetPair()

	f1 := CovenantFill{Order: kk, Pair: pair, Txid: "fill1", Input: 1, Spent: "aabb:0", Locked: 90, Filled: 30, Remainder: 60,
		Credit: FillOutput{Vout: 2, Value: 10}, Rest: &FillOutput{Vout: 3, Value: 60}, State: FillConfirmed}
	s.RecordCovenantFill(f1, o)
	if err := s.RerestCovenantRemainder(kk, "fill1", 3, 60); err != nil {
		t.Fatal(err)
	}
	if trades := s.TradesFor(pair, 0); len(trades) != 1 || trades[0].Size != 30 || trades[0].Txid != "fill1" {
		t.Fatalf("after partial: want exactly 1 trade of 30 by fill1, got %+v", trades)
	}
	// The same record again (a later pass) is an update, not a second trade.
	f1.State = FillFinal
	s.RecordCovenantFill(f1, o)
	if trades := s.TradesFor(pair, 0); len(trades) != 1 {
		t.Fatalf("a repeated record made another trade: %+v", trades)
	}

	f2 := CovenantFill{Order: kk, Pair: pair, Txid: "fill2", Input: 0, Spent: "fill1:3", Locked: 60, Filled: 60,
		Credit: FillOutput{Vout: 0, Value: 20}, State: FillConfirmed}
	s.RecordCovenantFill(f2, o)
	if err := s.HoldCovenantForSpend(kk, "fill2"); err != nil {
		t.Fatal(err)
	}
	trades := s.TradesFor(pair, 0)
	if len(trades) != 2 || trades[0].Size+trades[1].Size != 90 {
		t.Fatalf("recorded trades must be {30, 60} summing to base 90, got %+v", trades)
	}
	// The full fill is undone: its trade is retracted, the partial fill's stays.
	f2.State, f2.Reason = FillUndone, "its block left the active chain"
	if !s.UpdateCovenantFill(f2) {
		t.Fatal("no live record to undo")
	}
	if trades := s.TradesFor(pair, 0); len(trades) != 1 || trades[0].Txid != "fill1" {
		t.Fatalf("after the undo: %+v", trades)
	}
	recs := s.CovenantFills(pair, 0)
	if len(recs) != 2 || recs[0].State != FillFinal || recs[1].State != FillUndone || recs[1].Reason == "" || recs[0].OfferID != "cov1" {
		t.Fatalf("records: %+v", recs)
	}
	// An undone record is history: moving it on again finds nothing.
	if s.UpdateCovenantFill(f2) {
		t.Fatal("an undone record was moved on again")
	}
	// The order rests back where it was, its terms (and signature) untouched.
	if err := s.RepointCovenant(kk, "aabb", 0, 90, false); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(kk)
	if e.ActiveAmount != 90 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_OPEN || e.Offer.GetCovenant().GetCovenantTxid() != "aabb" {
		t.Fatalf("repointed: %+v", e)
	}
	if err := offer.VerifyOffer(e.Offer); err != nil {
		t.Fatalf("the repointed offer no longer verifies: %v", err)
	}
	if len(s.TradesFor(pair, 0)) != 1 {
		t.Fatal("repointing recorded a trade")
	}
}

func TestCovenantRefundIsExpiredAndNoTrade(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}
	if err := s.RemoveCovenantRefunded(kk, "refund1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatal("refunded order still rests")
	}
	if len(s.TradesFor(o.GetPair(), 0)) != 0 {
		t.Fatal("a refund made a trade")
	}
}

func TestCovenantSurvivesMakerDisconnect(t *testing.T) {
	// RemoveByMaker must NOT evict a covenant (the watcher owns covenant removal).
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	n := s.RemoveByMaker(o.GetMakerPubkey())
	if n != 0 {
		t.Fatalf("RemoveByMaker evicted %d covenant offers; must be 0", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}); !ok {
		t.Fatalf("covenant offer must survive maker disconnect")
	}
}

func mkCrossOffer(t *testing.T, k *btcec.PrivateKey, id string, expires uint64) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "BTC"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "BTC",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: expires,
		Settlement:    &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func TestCrossSurvivesMakerDisconnect(t *testing.T) {
	// A BTC<->asset cross offer is serviced by the maker's always-online settlement agent, which
	// reconnects after a WS blip. RemoveByMaker must NOT evict it (evicting it on every transient drop
	// emptied the cross book and vanished BTC from the DEX). Only interactive same-chain co-sign offers
	// — which genuinely need the maker online to lift — are evicted.
	s := New(nil)
	k := key(t)
	cross := mkCrossOffer(t, k, "x1", 1799999999)
	same := mkOffer(t, k, "s1", 1799999999) // interactive same-chain: MUST still be evicted
	if _, err := s.Submit(cross); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(same); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveByMaker(cross.GetMakerPubkey()); n != 1 {
		t.Fatalf("RemoveByMaker evicted %d offers; want 1 (the same-chain one only)", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: cross.GetMakerPubkey(), OfferID: "x1"}); !ok {
		t.Fatalf("cross offer must survive maker disconnect")
	}
	if _, ok := s.Get(Key{MakerPubkey: same.GetMakerPubkey(), OfferID: "s1"}); ok {
		t.Fatalf("interactive same-chain offer must be evicted on disconnect")
	}
}

func TestSubmitDuplicateReturnsErrExists(t *testing.T) {
	// A reconnecting durable-offer agent re-posts the same (maker, offer_id) key; Submit must return
	// ErrExists so the WS layer refreshes it via Edit + re-register instead of 409ing the maker off.
	s := New(nil)
	k := key(t)
	o := mkCrossOffer(t, k, "x1", 1799999999)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(o); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Submit err = %v; want ErrExists", err)
	}
}
