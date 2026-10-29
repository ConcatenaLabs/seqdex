package main

import (
	"strings"
	"testing"
)

func TestSizeOneWholeAndSlices(t *testing.T) {
	small := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 900, 1800), tBase, tQuote)
	if got, _ := SizeOne(small, 1000); got != 900 {
		t.Fatalf("an offer smaller than the take is taken whole: got %d", got)
	}
	big := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 10000, 20000), tBase, tQuote)
	if got, _ := SizeOne(big, 1000); got != 1000 {
		t.Fatalf("a sliceable offer gives the take asked: got %d", got)
	}
	whole := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 1500, 3000, withNoPartial()), tBase, tQuote)
	if got, _ := SizeOne(whole, 1000); got != 1500 {
		t.Fatalf("a whole-only offer within twice the take is taken whole: got %d", got)
	}
	tooBig := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 5000, 10000, withNoPartial()), tBase, tQuote)
	if got, why := SizeOne(tooBig, 1000); got != 0 || !strings.Contains(why, "whole-take only") {
		t.Fatalf("a whole-only offer over twice the take is refused: got %d (%s)", got, why)
	}
	minFill := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 10000, 20000, withMinFill(1500)), tBase, tQuote)
	if got, _ := SizeOne(minFill, 1000); got != 1500 {
		t.Fatalf("min_fill raises the take: got %d", got)
	}
	minFillFar := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 10000, 20000, withMinFill(5000)), tBase, tQuote)
	if got, _ := SizeOne(minFillFar, 1000); got != 0 {
		t.Fatalf("min_fill over twice the take is refused: got %d", got)
	}
}

func TestSizeOneCovenantRemainder(t *testing.T) {
	cov := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 10000, 20000, withMinLot(400)), tBase, tQuote)
	if got, _ := SizeOne(cov, 9800); got != 10000 {
		// 9800 would leave 200 < min_lot 400, and the whole is within twice the take
		t.Fatalf("a covenant take leaves a remainder of 0 or at least min_lot: got %d", got)
	}
	bigLot := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 10000, 20000, withMinLot(6000)), tBase, tQuote)
	if got, why := SizeOne(bigLot, 4000); got != 0 || !strings.Contains(why, "valid remainder") {
		// min_lot raises the take to 6000, which would leave 4000 < 6000, and the whole is over twice
		t.Fatalf("no legal size near the take is refused: got %d (%s)", got, why)
	}
	if got, _ := SizeOne(cov, 300); got != 400 {
		t.Fatalf("a covenant take is at least min_lot: got %d", got)
	}
	if got, _ := SizeOne(cov, 5100); got != 5100 {
		t.Fatalf("a covenant slice leaving at least min_lot is taken as asked: got %d", got)
	}
}

func TestPickTakeBestPriceAllowedRailOwnMakerAvoided(t *testing.T) {
	cheapCov := mkOffer(FamCovenant, SideAsk, tBase, tQuote, 10000, 19000)
	lnAsk := mkOffer(FamPureLN, SideAsk, tBase, tQuote, 10000, 20000)
	worseLN := mkOffer(FamPureLN, SideAsk, tBase, tQuote, 10000, 21000)
	bid := mkOffer(FamPureLN, SideBid, tBase, tQuote, 10000, 18000)
	orders := []*NormOrder{norm(worseLN, tBase, tQuote), norm(cheapCov, tBase, tQuote), norm(lnAsk, tBase, tQuote), norm(bid, tBase, tQuote)}

	n, take, why := PickTake(orders, "buy", 1000, nil, execAll, nil)
	if n == nil || n.Offer.GetOfferId() != cheapCov.GetOfferId() || take != 1000 {
		t.Fatalf("any rail: the best ask (the covenant) is taken, got %v %d (%s)", n, take, why)
	}
	n, _, _ = PickTake(orders, "buy", 1000, map[string]bool{"pureln": true}, execAll, nil)
	if n == nil || n.Offer.GetOfferId() != lnAsk.GetOfferId() {
		t.Fatalf("Lightning only: the best Lightning ask is taken, got %v", n)
	}
	n, _, _ = PickTake(orders, "buy", 1000, map[string]bool{"pureln": true}, execAll, map[string]bool{lnAsk.GetMakerPubkey(): true})
	if n == nil || n.Offer.GetOfferId() != worseLN.GetOfferId() {
		t.Fatalf("an avoided maker is skipped, got %v", n)
	}
	n, _, _ = PickTake(orders, "sell", 1000, nil, execAll, nil)
	if n == nil || n.Offer.GetOfferId() != bid.GetOfferId() {
		t.Fatalf("a sell takes the best bid, got %v", n)
	}
	noExec := func(*NormOrder) (bool, string) { return false, "missing -asset-ln-socket" }
	if n, _, why := PickTake(orders, "buy", 1000, nil, noExec, nil); n != nil || !strings.Contains(why, "missing -asset-ln-socket") {
		t.Fatalf("nothing executable: no offer, and why: %v %s", n, why)
	}
}
