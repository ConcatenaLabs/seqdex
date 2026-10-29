package main

// scripted_test.go — the scripted-book test: a full detect->plan pass over a
// multi-relay, multi-family book of REAL signed offers, driving everything up to
// (but never into) execution — the exact seam main.go runs on each poll.

import (
	"strings"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func TestScriptedBookDetectToPlan(t *testing.T) {
	base, quote := tBase, offer.BTCSentinel
	now := time.Now()
	soon := uint64(now.Add(30 * time.Second).Unix())

	// The union view a poll would fetch: three relays, mixed families/orientations.
	crossAsk := signOffer(mkOffer(FamCross, SideAsk, base, quote, 100, 4800))  // ask 48.00
	plnAsk := signOffer(mkOffer(FamPureLN, SideAsk, base, quote, 80, 3760))    // ask 47.00
	covAsk := signOffer(mkOffer(FamCovenant, SideAsk, base, quote, 100, 4000)) // ask 40.00 <- best ask (covfill-executable)
	subBid := signOffer(mkOffer(FamSubAsset, SideBid, base, quote, 50, 2450))  // bid 49.00  <- best bid
	crossBid := signOffer(mkOffer(FamCross, SideBid, base, quote, 100, 4600))  // bid 46.00 (does not cross)
	expiring := signOffer(mkOffer(FamPureLN, SideBid, base, quote, 100, 5200, withExpiry(soon)))
	forged := mkOffer(FamPureLN, SideBid, base, quote, 100, 6000) // UNSIGNED: a malicious relay row
	forged.MakerSig = []byte{0xde, 0xad}

	offersByRelay := map[string][]*seqobv1.Offer{
		"http://relay-a": {crossAsk, crossBid, forged},
		"http://relay-b": {plnAsk, expiring, crossAsk /* duplicate of relay-a's */},
		"http://relay-c": {subBid, covAsk},
	}

	union := BuildUnion(offersByRelay, base, quote, now, 5*time.Minute, noLog)
	if len(union) != 5 { // forged dropped, expiring dropped, crossAsk deduped
		ids := map[string]bool{}
		for _, n := range union {
			ids[n.ID()] = true
		}
		t.Fatalf("union has %d orders (%v), want 5", len(union), ids)
	}
	for _, n := range union {
		if n.Offer == forged {
			t.Fatal("forged offer survived signature verification")
		}
		if n.Offer == expiring {
			t.Fatal("near-expiry offer survived the min-TTL filter")
		}
	}

	// Executability as configured for LN + on-chain but NOT samechain — the real
	// Caps check with a realistic box config. The seq node also unlocks covfill.
	caps := Caps{
		SeqRPC: "http://u:p@h:1", SeqWallet: "w",
		BtcRPC: "http://u:p@h:2", BtcWallet: "b", BtcChain: "testnet4",
		AssetLNSocket: "/tmp/a.rpc", BtcLNSocket: "/tmp/b.rpc",
	}
	d := Detect(union, caps.Executable, GateConfig{MinProfitBps: 10, SpendFeeAtoms: 10})
	if d.Plan == nil {
		t.Fatalf("no plan: %s", d.Skip)
	}
	p := d.Plan

	// The covenant ask (40.00) is the best-priced ask AND executable now that
	// the single-fill driver exists: -seq-rpc/-seq-wallet unlock covfill. The
	// pure-LN 47.00 must lose on price.
	if p.Ask.Offer != covAsk {
		t.Fatalf("chose ask %s (%s), want the covenant ask", p.Ask.ID(), p.Ask.Family)
	}
	if p.Bid.Offer != subBid {
		t.Fatalf("chose bid %s (%s), want the sub-asset bid", p.Bid.ID(), p.Bid.Family)
	}

	// Overlap: min(100, 50) = 50; both legs sliceable (covenant min_lot 1: the
	// remainder 50 re-rests legally).
	if p.Take != 50 {
		t.Fatalf("take=%d want 50", p.Take)
	}
	// Economics: cost = ceil(50*4000/100) = 2000; revenue = floor(50*2450/50) = 2450.
	if p.Cost != 2000 || p.Revenue != 2450 || p.Gross != 450 {
		t.Fatalf("economics: cost=%d revenue=%d gross=%d", p.Cost, p.Revenue, p.Gross)
	}
	// Gate held: est costs = (subasset 2 + covenant 1) * 10 = 30 < 450.
	if p.EstCost != 30 {
		t.Fatalf("est cost=%d want 30", p.EstCost)
	}

	// Leg ordering: sub-asset (score 2) is slower than covenant (5): bid first.
	if p.First != p.Bid || p.Second != p.Ask {
		t.Fatalf("ordering wrong: first=%s", p.First.Family)
	}
	if !strings.Contains(p.OrderReason, "slower") {
		t.Fatalf("reason: %s", p.OrderReason)
	}

	// And the plan is admissible under an exposure cap sized for its worst case
	// (sell-first: short 50 base / long 2450 quote if leg 2 fails).
	l := NewLedger(3000, nil)
	if err := l.Reserve(p); err != nil {
		t.Fatalf("ledger refused the planned crossing: %v", err)
	}
}

// TestScriptedBookCovenantGating pins the covenant executability gate: without
// the Sequentia node wallet a covenant leg is skipped with the missing-flags
// reason (never a wrong execution); WITH -seq-rpc/-seq-wallet the same book
// produces a plan through covfill.
func TestScriptedBookCovenantGating(t *testing.T) {
	base, quote := tBase, tQuote
	union := []*NormOrder{
		norm(mkOffer(FamCovenant, SideAsk, base, quote, 100, 40), base, quote),
		norm(mkOffer(FamPureLN, SideBid, base, quote, 100, 50), base, quote),
	}
	// No seq node: the covenant ask is inexecutable, so no plan.
	caps := Caps{AssetLNSocket: "/tmp/a.rpc", BtcLNSocket: "/tmp/b.rpc"}
	d := Detect(union, caps.Executable, gateOff())
	if d.Plan != nil {
		t.Fatalf("covenant leg executed without a seq node: %+v", d.Plan)
	}
	if !strings.Contains(d.Skip, "no executable ask") {
		t.Fatalf("skip=%q", d.Skip)
	}
	if ok, why := caps.Executable(union[0]); ok || !strings.Contains(why, "-seq-rpc/-seq-wallet") {
		t.Fatalf("covenant executability: ok=%v why=%q", ok, why)
	}
	// With the seq node the covenant leg executes and the cross plans.
	caps.SeqRPC, caps.SeqWallet = "http://u:p@h:1", "w"
	if ok, why := caps.Executable(union[0]); !ok {
		t.Fatalf("covenant still inexecutable with a seq node: %s", why)
	}
	d = Detect(union, caps.Executable, gateOff())
	if d.Plan == nil {
		t.Fatalf("no plan with the seq node configured: %s", d.Skip)
	}
	if d.Plan.Ask.Family != FamCovenant {
		t.Fatalf("ask family = %s, want covenant", d.Plan.Ask.Family)
	}
}

// TestCapsExecutabilityMatrix pins which flag sets unlock which families.
func TestCapsExecutabilityMatrix(t *testing.T) {
	base, quote := tBase, offer.BTCSentinel
	full := Caps{
		SeqRPC: "r", SeqWallet: "w", BtcRPC: "r", BtcWallet: "w", BtcChain: "testnet4",
		AssetLNSocket: "a", BtcLNSocket: "b",
		Esplora: "e", TakerPriv: strings.Repeat("11", 32), TakerBlinding: strings.Repeat("22", 32),
	}
	for _, c := range []struct {
		fam  Family
		caps Caps
		ok   bool
	}{
		{FamPureLN, full, true},
		{FamPureLN, Caps{AssetLNSocket: "a"}, false}, // missing btc-ln
		{FamCross, full, true},
		{FamCross, Caps{BtcRPC: "r", BtcWallet: "w"}, false}, // missing seq side
		{FamSubmarine, full, true},
		{FamSubAsset, full, true},
		{FamSubAsset, Caps{AssetLNSocket: "a"}, false}, // missing btc side
		{FamSameChain, full, true},
		{FamSameChain, Caps{Esplora: "e"}, false},
		{FamCovenant, full, true},                  // covfill: needs only the seq node wallet
		{FamCovenant, Caps{SeqRPC: "r"}, false},    // missing -seq-wallet
		{FamCovenant, Caps{SeqWallet: "w"}, false}, // missing -seq-rpc
	} {
		n := norm(mkOffer(c.fam, SideAsk, base, quote, 100, 45), base, quote)
		if ok, why := c.caps.Executable(n); ok != c.ok {
			t.Errorf("family %v caps-ok=%v want %v (%s)", c.fam, ok, c.ok, why)
		}
	}
	// Mixed same-chain sub-asset (non-BTC quote) needs the seq node, not bitcoind.
	mixed := norm(mkOffer(FamSubAsset, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
	if ok, _ := (&Caps{AssetLNSocket: "a", SeqRPC: "r", SeqWallet: "w"}).Executable(mixed); !ok {
		t.Error("mixed same-chain sub-asset should run on the seq node")
	}
	if ok, _ := (&Caps{AssetLNSocket: "a", BtcRPC: "r", BtcWallet: "w"}).Executable(mixed); ok {
		t.Error("mixed same-chain sub-asset must not claim bitcoind suffices")
	}
	// Flipped offers: covenant and same-chain legs execute (their -amount is
	// converted to the offer's own base); the other families do not.
	for _, fam := range []Family{FamSameChain, FamCovenant} {
		flipped := norm(mkOffer(fam, SideAsk, tQuote, tBase, 50, 100), tBase, tQuote)
		if ok, why := full.Executable(flipped); !ok {
			t.Errorf("flipped %v executability: ok=%v why=%q", fam, ok, why)
		}
	}
	flippedLN := norm(mkOffer(FamPureLN, SideAsk, tQuote, tBase, 50, 100), tBase, tQuote)
	if ok, why := full.Executable(flippedLN); ok || !strings.Contains(why, "flipped") {
		t.Errorf("flipped pure-LN executability: ok=%v why=%q", ok, why)
	}
}
