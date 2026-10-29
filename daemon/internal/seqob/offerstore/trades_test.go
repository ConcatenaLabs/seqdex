package offerstore

import (
	"os"
	"path/filepath"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// A market is the unordered pair: an order selling gold for usdx and one selling
// usdx for gold trade in one log, served the same way from either direction.
func TestTradesOneMarketBothDirections(t *testing.T) {
	s := New(nil)
	k := key(t)
	ask := mkOffer(t, k, "a1", 1799999999) // sells 100 gold for 45 usdx: 0.45 usdx/gold
	rev := mkOffer(t, k, "r1", 1799999999)
	rev.Pair = &seqobv1.AssetPair{BaseAsset: "usdx", QuoteAsset: "gold"}
	rev.OfferAsset, rev.WantAsset = "usdx", "gold"
	rev.BaseAmount, rev.OfferAmount, rev.WantAmount = 50, 50, 100 // sells usdx at 2 gold/usdx = 0.5 usdx/gold
	rev.MakerSig = nil
	if err := offer.SignOffer(rev, k); err != nil {
		t.Fatal(err)
	}
	s.RecordTradeVia(ask, 100, "tx1", "f1", RailOnchain)
	s.RecordTradeVia(rev, 50, "tx2", "f2", RailLeafResting)
	gu := s.TradesFor(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}, 0)
	ug := s.TradesFor(&seqobv1.AssetPair{BaseAsset: "usdx", QuoteAsset: "gold"}, 0)
	if len(gu) != 2 || len(ug) != 2 {
		t.Fatalf("want both trades from both directions, got %d and %d", len(gu), len(ug))
	}
	if gu[0].Price != 0.45 || gu[0].Size != 100 || gu[0].Quote != 45 || !gu[0].TakerBuys || gu[0].Rail != RailOnchain {
		t.Fatalf("gold/usdx first trade %+v", gu[0])
	}
	if gu[1].Price != 0.5 || gu[1].Size != 100 || gu[1].Quote != 50 || gu[1].TakerBuys || gu[1].Rail != RailLeafResting {
		t.Fatalf("gold/usdx second trade (the usdx seller's: its taker sold gold) %+v", gu[1])
	}
	if ug[1].Price != 2 || ug[1].Size != 50 || ug[1].Quote != 100 || !ug[1].TakerBuys {
		t.Fatalf("usdx/gold second trade %+v", ug[1])
	}
}

// A fill attested twice (the session's settle ack and the chain watcher, a
// maker reporting again) is one trade.
func TestTradesSameIDOnce(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "a1", 1799999999)
	s.RecordTradeTx(o, 10, "txA")
	s.RecordTradeTx(o, 10, "txA")
	if !s.RecordReported("gold", 7, "usdx", 3, RailLeafMaker, "m:leaf-maker:sw1") {
		t.Fatal("first report not recorded")
	}
	if s.RecordReported("gold", 7, "usdx", 3, RailLeafMaker, "m:leaf-maker:sw1") {
		t.Fatal("second report of the same swap recorded")
	}
	tr := s.TradesFor(&seqobv1.AssetPair{BaseAsset: "usdx", QuoteAsset: "gold"}, 0)
	if len(tr) != 2 {
		t.Fatalf("want 2 trades, got %+v", tr)
	}
	// The reported trade: the maker gave 7 gold for 3 usdx, so seen as usdx/gold
	// its taker sold usdx (gave usdx, got gold).
	if r := tr[1]; r.Size != 3 || r.Quote != 7 || r.TakerBuys || r.Rail != RailLeafMaker {
		t.Fatalf("reported trade %+v", r)
	}
	if s.RecordReported("gold", 0, "usdx", 3, RailLeafMaker, "x") || s.RecordReported("gold", 1, "gold", 3, RailLeafMaker, "y") {
		t.Fatal("a trade of zero, or of one asset for itself, recorded")
	}
}

// A log written before markets were canonical (no quote, the resting order's
// pair) replays into the one market; a restart keeps rails and ids.
func TestTradeLogReplaysLegacyAndCanonical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	legacy := `{"pk":"usdx/gold","ts":"2026-10-01T00:00:00Z","price":2,"size":50,"txid":"old"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s1 := New(nil)
	if err := s1.EnableTradeLog(path); err != nil {
		t.Fatal(err)
	}
	s1.RecordReported("gold", 10, "usdx", 5, RailBTCGateway, "id1")
	s2 := New(nil)
	if err := s2.EnableTradeLog(path); err != nil {
		t.Fatal(err)
	}
	s2.RecordReported("gold", 10, "usdx", 5, RailBTCGateway, "id1") // already held: ignored
	tr := s2.TradesFor(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}, 0)
	if len(tr) != 2 {
		t.Fatalf("want legacy + reported, got %+v", tr)
	}
	if tr[0].Size != 100 || tr[0].Quote != 50 || tr[0].Price != 0.5 || tr[0].Txid != "old" {
		t.Fatalf("legacy trade %+v", tr[0])
	}
	if tr[1].Rail != RailBTCGateway || tr[1].ID != "id1" || tr[1].Size != 10 {
		t.Fatalf("reported trade after restart %+v", tr[1])
	}
	if n := s2.RetractTrade(&seqobv1.AssetPair{BaseAsset: "usdx", QuoteAsset: "gold"}, "old"); n != 1 {
		t.Fatalf("retract from the other direction removed %d", n)
	}
}
