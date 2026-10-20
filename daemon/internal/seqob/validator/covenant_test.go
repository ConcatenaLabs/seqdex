package validator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func covTerms(num, den, minLot uint64) *seqobv1.Offer_Covenant {
	return &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: strings.Repeat("ab", 32), AssetA: "aa", AssetB: "bb",
		RateNum: num, RateDen: den, MinLot: minLot, MakerProgVer: 1, ExpiryLocktime: 400,
		MakerProg: bytes.Repeat([]byte{0xc3}, 32), MakerX: bytes.Repeat([]byte{0xd4}, 32),
	}}
}

// The covenant's baked-in rate is what the chain enforces; the offer's amounts are
// what takers price from. The relay refuses a covenant offer whose two prices differ,
// and one whose arithmetic the FILL leaf cannot evaluate.
func TestCovenantTermsMustMatchOffer(t *testing.T) {
	v := New(cfg(), nil)
	check := func(name string, mut func(*seqobv1.Offer), wantErr string) {
		t.Helper()
		o := signed(t, key(t), mut)
		err := v.ValidateOffer(context.Background(), o, "")
		if wantErr == "" && err != nil {
			t.Fatalf("%s: unexpected %v", name, err)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("%s: err = %v, want containing %q", name, err, wantErr)
		}
	}
	// offer 100 A for 45 B: rate 45/100 (or reduced 9/20) is the same price.
	check("consistent", func(o *seqobv1.Offer) { o.Settlement = covTerms(45, 100, 1) }, "")
	check("reduced", func(o *seqobv1.Offer) { o.Settlement = covTerms(9, 20, 1) }, "")
	check("rate above offer", func(o *seqobv1.Offer) { o.Settlement = covTerms(100, 1, 1) }, "does not equal the offer's want/offer")
	check("rate below offer", func(o *seqobv1.Offer) { o.Settlement = covTerms(1, 100, 1) }, "does not equal")
	check("zero den", func(o *seqobv1.Offer) { o.Settlement = covTerms(45, 0, 1) }, "must be >= 1")
	check("min_lot above lock", func(o *seqobv1.Offer) { o.Settlement = covTerms(45, 100, 101) }, "exceeds the locked amount")
	check("leaf overflow", func(o *seqobv1.Offer) {
		o.OfferAmount, o.BaseAmount = 1e10, 1e10
		o.WantAmount = 4e11 + 1
		o.Settlement = covTerms(4e11+1, 1e10, 1)
	}, "overflows the FILL leaf")

	// With real 32-byte ids, asset A must be the offer_asset (internal vs display order).
	a := strings.Repeat("01", 31) + "ff"
	aInternal := "ff" + strings.Repeat("01", 31)
	b := strings.Repeat("02", 31) + "ee"
	bInternal := "ee" + strings.Repeat("02", 31)
	real := func(o *seqobv1.Offer) {
		o.Pair = &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b}
		o.OfferAsset, o.WantAsset = a, b
		ct := covTerms(45, 100, 1)
		ct.Covenant.AssetA, ct.Covenant.AssetB = aInternal, bInternal
		o.Settlement = ct
	}
	check("real assets consistent", real, "")
	check("real assets swapped", func(o *seqobv1.Offer) {
		real(o)
		ct := o.GetCovenant()
		ct.AssetA, ct.AssetB = bInternal, aInternal
	}, "is not the offer_asset")
}

// An order's REFUND lock may be a block height or a median time (from
// 500,000,000): the same leaf takes both, and so does the relay.
func TestCovenantRefundLockIsAHeightOrAMedianTime(t *testing.T) {
	v := New(cfg(), nil)
	for _, lock := range []uint32{400, 499_999_999, 500_000_000, 1_792_611_631} {
		o := signed(t, key(t), func(o *seqobv1.Offer) {
			ct := covTerms(45, 100, 1)
			ct.Covenant.ExpiryLocktime = lock
			o.Settlement = ct
		})
		if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
			t.Fatalf("expiry_locktime %d refused: %v", lock, err)
		}
	}
	o := signed(t, key(t), func(o *seqobv1.Offer) {
		ct := covTerms(45, 100, 1)
		ct.Covenant.ExpiryLocktime = 0
		o.Settlement = ct
	})
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil || !strings.Contains(err.Error(), "expiry_locktime must be set") {
		t.Fatalf("an order with no refund lock: %v", err)
	}
}
