package validator

import (
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// An order resting in an operator's tree: 600,000 A at 2 B each.
func restingOffer(o *seqobv1.Offer) {
	a := strings.Repeat("1f", 32)
	b := strings.Repeat("2e", 32)
	o.Pair = &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b}
	o.OfferAsset, o.WantAsset = a, b
	o.BaseAmount, o.OfferAmount, o.WantAmount = 600_000, 600_000, 1_200_000
	o.Settlement = &seqobv1.Offer_RestingLeaf{RestingLeaf: &seqobv1.RestingLeafTerms{
		OperatorUrl: "http://127.0.0.1:3535", OperatorKey: strings.Repeat("5c", 32), OrderId: strings.Repeat("d1", 32),
		AssetA: a, AssetB: b, RateNum: 2, RateDen: 1, MinLot: 10_000,
		LandedMaker: make([]byte, 32), LandedExpiry: 1_892_611_631, RefundKey: make([]byte, 32),
		Coin: strings.Repeat("d1", 32), Value: 600_000,
	}}
}

func TestRestingOfferNeedsARelayThatFollowsItsOperator(t *testing.T) {
	o := signed(t, key(t), restingOffer)
	err := New(cfg(), nil).ValidateOffer(context.Background(), o, "")
	if err == nil || !strings.Contains(err.Error(), "listed only by a relay that follows its operator") {
		t.Fatalf("a relay following no operator: %v", err)
	}
	c := cfg()
	c.ListResting = true
	if err := New(c, nil).ValidateOffer(context.Background(), o, ""); err != nil {
		t.Fatalf("a relay that follows operators: %v", err)
	}
	bad := signed(t, key(t), func(o *seqobv1.Offer) { restingOffer(o); o.GetRestingLeaf().RateNum = 3 })
	if err := New(c, nil).ValidateOffer(context.Background(), bad, ""); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("another rate: %v", err)
	}
	conf := signed(t, key(t), func(o *seqobv1.Offer) { restingOffer(o); o.Confidential = true })
	if err := New(c, nil).ValidateOffer(context.Background(), conf, ""); err == nil || !strings.Contains(err.Error(), "same-chain co-sign rail") {
		t.Fatalf("confidential: %v", err)
	}
}
