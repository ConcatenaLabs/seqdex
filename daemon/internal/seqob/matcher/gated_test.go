package matcher

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A gated order is filled only by its policy server, with a taker's
// transaction: no cross with it has a settlement owner, on either side.
func TestGatedOrderHasNoSettlementOwner(t *testing.T) {
	gated := &seqobv1.Offer{Settlement: &seqobv1.Offer_GatedOrder{GatedOrder: &seqobv1.GatedOrderTerms{}}}
	plain := &seqobv1.Offer{Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{}}}
	if hasSettlementOwner(plain, gated) || hasSettlementOwner(gated, plain) {
		t.Fatal("a cross with a gated order has a settlement owner")
	}
}
