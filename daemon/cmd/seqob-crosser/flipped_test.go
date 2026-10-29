package main

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func liftExecutor() *Executor {
	e := covExecutor()
	e.Caps.Esplora, e.Caps.TakerPriv, e.Caps.TakerBlinding = "http://esplora", "11", "22"
	return e
}

// A covenant ask posted on the REVERSE orientation (it sells 4000 of our quote
// for 100 of our base) is a canonical BID. Selling 40 base into it claims
// floor(40*4000/100) = 1600 of asset A (our quote) and pays at most 40 base.
func TestFlippedCovenantBidLeg(t *testing.T) {
	n := norm(mkOffer(FamCovenant, SideAsk, tQuote, tBase, 4000, 100), tBase, tQuote)
	if !n.Flipped || n.Side != SideBid || n.BaseSize != 100 || n.QuoteNum != 4000 {
		t.Fatalf("want flipped bid 100 base for 4000 quote, got %+v", n)
	}
	args, _, _, err := covExecutor().legCommand(n, 40)
	if err != nil {
		t.Fatal(err)
	}
	cmd, f := argsMap(t, args)
	if cmd != "covfill" || f["-amount"] != "1600" || f["-max-pay"] != "40" || f["-base"] != tQuote || f["-quote"] != tBase {
		t.Fatalf("covfill args %v", f)
	}
	args, _, _, _ = covExecutor().legCommand(n, 100)
	if _, f = argsMap(t, args); f["-amount"] != "0" {
		t.Fatalf("whole flipped take -amount = %s, want 0", f["-amount"])
	}
}

// min_lot of a flipped covenant is in its asset A (our quote): converted to
// canonical base atoms, rounded up.
func TestFlippedCovenantMinLot(t *testing.T) {
	n := norm(mkOffer(FamCovenant, SideAsk, tQuote, tBase, 4000, 100, withMinLot(90)), tBase, tQuote)
	if n.MinLot != 3 { // ceil(90*100/4000)
		t.Fatalf("flipped min_lot %d, want 3", n.MinLot)
	}
}

// A same-chain lift sizes -amount in the offer's own base. A flipped ask (the
// maker gives our base, wants our quote, its own base) is paid ceil(take*Q/B) of
// our quote; a flipped bid gives floor(take*Q/B) of our quote.
func TestFlippedSameChainLiftAmount(t *testing.T) {
	ask := norm(mkOffer(FamSameChain, SideBid, tQuote, tBase, 333, 100), tBase, tQuote) // maker buys 333 quote with 100 base
	if !ask.Flipped || ask.Side != SideAsk {
		t.Fatalf("want flipped ask, got %+v", ask)
	}
	args, _, _, _ := liftExecutor().legCommand(ask, 10)
	if _, f := argsMap(t, args); f["-amount"] != "34" || f["-base"] != tQuote {
		t.Fatalf("flipped ask lift args %v, want -amount 34 (ceil(10*333/100))", f)
	}
	bid := norm(mkOffer(FamSameChain, SideAsk, tQuote, tBase, 333, 100), tBase, tQuote) // maker sells 333 quote for 100 base
	args, _, _, _ = liftExecutor().legCommand(bid, 10)
	if _, f := argsMap(t, args); f["-amount"] != "33" {
		t.Fatalf("flipped bid lift args %v, want -amount 33 (floor(10*333/100))", f)
	}
	plain := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 100, 333), tBase, tQuote)
	args, _, _, _ = liftExecutor().legCommand(plain, 10)
	if _, f := argsMap(t, args); f["-amount"] != "10" {
		t.Fatalf("unflipped lift -amount %v, want 10", f)
	}
}

// The relays' statuses: a filled or cancelled offer leaves the union, a partly
// filled one is capped to what is left (a flipped one converted to our base),
// and the cap stops a capped whole take from being sent as -amount 0.
func TestApplyStatuses(t *testing.T) {
	open := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 400), tBase, tQuote)
	gone := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 100, 400), tBase, tQuote)
	part := norm(mkOffer(FamCovenant, SideAsk, tQuote, tBase, 4000, 100), tBase, tQuote) // flipped bid
	nost := norm(mkOffer(FamSameChain, SideBid, tBase, tQuote, 100, 300), tBase, tQuote)
	st := map[string]*seqobv1.OrderStatus{
		open.ID(): {Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: 100},
		gone.ID(): {Status: seqobv1.OfferStatus_OFFER_STATUS_FILLED, ActiveAmount: 0},
		part.ID(): {Status: seqobv1.OfferStatus_OFFER_STATUS_PARTIAL, ActiveAmount: 1000},
	}
	got := ApplyStatuses([]*NormOrder{open, gone, part, nost}, st)
	if len(got) != 3 {
		t.Fatalf("kept %d offers, want 3", len(got))
	}
	if part.BaseSize != 25 || part.BaseDen != 100 { // floor(1000*100/4000)
		t.Fatalf("partial flipped capped to %d (den %d), want 25 (100)", part.BaseSize, part.BaseDen)
	}
	if part.Whole(25) || !open.Whole(100) {
		t.Fatalf("Whole: capped %v open %v", part.Whole(25), open.Whole(100))
	}
	args, _, _, _ := covExecutor().legCommand(part, 25)
	if _, f := argsMap(t, args); f["-amount"] != "1000" {
		t.Fatalf("capped flipped covfill -amount %s, want 1000", f["-amount"])
	}
}

// A cross between a flipped covenant bid and an unflipped same-chain ask is now
// a plan, sized and gated.
func TestDetectFlippedCross(t *testing.T) {
	bid := norm(mkOffer(FamCovenant, SideAsk, tQuote, tBase, 4200, 100), tBase, tQuote)  // pays 42/base
	ask := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 100, 4000), tBase, tQuote) // sells at 40/base
	e := liftExecutor()
	d := Detect([]*NormOrder{bid, ask}, e.Caps.Executable, GateConfig{MinProfitBps: 10, SpendFeeAtoms: 1})
	if d.Plan == nil {
		t.Fatalf("no plan: %s", d.Skip)
	}
	if d.Plan.Take != 100 || d.Plan.Cost != 4000 || d.Plan.Revenue != 4200 {
		t.Fatalf("plan %+v", d.Plan)
	}
}
