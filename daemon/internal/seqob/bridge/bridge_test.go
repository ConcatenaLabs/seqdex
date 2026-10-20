package bridge

import (
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// a covenant selling X wanting Y at 3 Y per X (num/den = 3/1), min_lot 5.
func testOrder(t *testing.T) covenant.Order {
	t.Helper()
	var x, y, mk [32]byte
	x[0], y[0], mk[0] = 0xaa, 0xbb, 0xcc
	return covenant.Order{
		AssetA: x, AssetB: y,
		RateNum: 3, RateDen: 1,
		MakerProg: mk[:], MakerVer: 1, MinLot: 5,
		ExpiryLocktime: 400, MakerX: mk, InternalKey: covenant.NUMS,
	}
}

// TestBridgePaysMakerCeilPriceAndKeepsFee: an under-cap cross fronts at 0-conf,
// pays the maker its exact ceil price, and keeps the LN/on-chain Y spread.
func TestBridgePaysMakerCeilPriceAndKeepsFee(t *testing.T) {
	o := testOrder(t)
	// Fill 30 X. Covenant ceil price = 30*3 = 90 Y owed to the maker.
	// LN order offers 100 Y for 30 X (a richer bid) -> bridge collects 100 Y.
	s, err := Plan(o, 30, 30, 0, LNSide{OfferY: 100, WantX: 30, Max0Conf: 50}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.PayY != 90 {
		t.Fatalf("PayY = %d, want 90 (maker ceil price)", s.PayY)
	}
	if s.RecvX != 30 || s.LNDeliverX != 30 {
		t.Fatalf("RecvX/LNDeliverX = %d/%d, want 30/30", s.RecvX, s.LNDeliverX)
	}
	if s.LNRecvY != 100 {
		t.Fatalf("LNRecvY = %d, want 100", s.LNRecvY)
	}
	if s.FeeY != 10 {
		t.Fatalf("FeeY = %d, want 10 (100 collected - 90 paid)", s.FeeY)
	}
	if !s.ZeroConfFront || s.RequireConfs {
		t.Fatalf("under-cap cross should 0-conf front: %+v", s)
	}
	if s.Fill.CreditIndex != 0 || s.Fill.RemainderIndex != 1 {
		t.Fatalf("single-covenant FILL index map wrong: %+v", s.Fill)
	}
}

// TestBridgeOverCapRequiresConfs: a fill above the per-offer 0-conf cap must not
// be fronted; the bridge waits for the FILL to be anchor-buried.
func TestBridgeOverCapRequiresConfs(t *testing.T) {
	o := testOrder(t)
	// Fill 30 X but the cap is only 20 -> over cap.
	s, err := Plan(o, 30, 30, 0, LNSide{OfferY: 100, WantX: 30, Max0Conf: 20}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ZeroConfFront {
		t.Fatalf("over-cap cross must NOT 0-conf front: %+v", s)
	}
	if !s.RequireConfs {
		t.Fatalf("over-cap cross must require confirmations: %+v", s)
	}
}

// TestBridgeZeroCapRequiresConfs: an offer with no 0-conf cap never fronts.
func TestBridgeZeroCapRequiresConfs(t *testing.T) {
	o := testOrder(t)
	s, err := Plan(o, 30, 30, 0, LNSide{OfferY: 100, WantX: 30, Max0Conf: 0}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ZeroConfFront || !s.RequireConfs {
		t.Fatalf("no-cap offer must require confirmations: %+v", s)
	}
}

// TestBridgeFeeMustCoverFronting: within the cap but with a fee that does not
// clear the sized policy fee, the bridge refuses to front (principle #4 — never
// front more than fees cover).
func TestBridgeFeeMustCoverFronting(t *testing.T) {
	o := testOrder(t)
	// Bid exactly the ceil price (90 Y for 30 X): fee = 0, cannot cover any fee.
	// policyFeeSats=1000, rateY=0 -> minFeeY = 1000 > 0 fee.
	s, err := Plan(o, 30, 30, 0, LNSide{OfferY: 90, WantX: 30, Max0Conf: 50}, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.FeeY != 0 {
		t.Fatalf("FeeY = %d, want 0 (bid == ceil price)", s.FeeY)
	}
	if s.MinFeeY != 1000 {
		t.Fatalf("MinFeeY = %d, want 1000", s.MinFeeY)
	}
	if s.ZeroConfFront {
		t.Fatalf("fee below sized policy fee must NOT front: %+v", s)
	}
	if !s.RequireConfs {
		t.Fatalf("fee below sized policy fee must require confs: %+v", s)
	}
}

// TestBridgeRejectsUnderpayingCross: a cross where the LN side pays LESS than the
// covenant's ceil price is refused outright (the maker would be underpaid).
func TestBridgeRejectsUnderpayingCross(t *testing.T) {
	o := testOrder(t)
	// LN offers only 80 Y for 30 X; ceil price is 90 -> underpays the maker.
	if _, err := Plan(o, 30, 30, 0, LNSide{OfferY: 80, WantX: 30, Max0Conf: 50}, 0, 0); err == nil {
		t.Fatal("expected Plan to reject an underpaying cross")
	}
}

// TestBridgePartialFillRemainder: a partial fill re-rests the covenant remainder
// and honours the min_lot floors, and the FILL leg flags partial.
func TestBridgePartialFillRemainder(t *testing.T) {
	o := testOrder(t)
	// Locked 30 X, fill 20 X -> remainder 10 X (>= min_lot 5). Ceil price 60 Y.
	s, err := Plan(o, 30, 20, 0, LNSide{OfferY: 100, WantX: 20, Max0Conf: 50}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Fill.Partial || s.Fill.Remainder != 10 {
		t.Fatalf("partial fill remainder wrong: partial=%v remainder=%d", s.Fill.Partial, s.Fill.Remainder)
	}
	if s.PayY != 60 {
		t.Fatalf("PayY = %d, want 60 (ceil 20*3)", s.PayY)
	}
}

// TestSizePolicyFeeNotInverted checks the principle-#4 fee sizing: a valuable
// asset (high rate) correctly pays FEWER atoms; the arithmetic is never inverted.
func TestSizePolicyFeeNotInverted(t *testing.T) {
	// rateY == 1e8 (a unit-priced asset): atoms == sats.
	if got := SizePolicyFee(500, FeeScale); got != 500 {
		t.Fatalf("unit-rate fee = %d, want 500", got)
	}
	// A more valuable asset (rate 5e12): ceil(500*1e8/5e12) = ceil(0.01) = 1 atom.
	if got := SizePolicyFee(500, 5_000_000_000_000); got != 1 {
		t.Fatalf("valuable-asset fee = %d, want 1 (fewer atoms, not inverted)", got)
	}
	// rate 0 -> flat sats fallback.
	if got := SizePolicyFee(500, 0); got != 500 {
		t.Fatalf("zero-rate fallback = %d, want 500", got)
	}
}

// TestBridgePlansAtAnyInput: the bridge's covenant spent beside other covenant
// inputs, at k = 2, is credited at 4 and rests again (or holds its gap) at 5.
func TestBridgePlansAtAnyInput(t *testing.T) {
	o := testOrder(t)
	s, err := Plan(o, 30, 20, 2, LNSide{OfferY: 100, WantX: 20, Max0Conf: 50}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Fill.InputIndex != 2 || s.Fill.CreditIndex != 4 || s.Fill.RemainderIndex != 5 {
		t.Fatalf("k=2 index map: input %d credit %d remainder %d", s.Fill.InputIndex, s.Fill.CreditIndex, s.Fill.RemainderIndex)
	}
}
