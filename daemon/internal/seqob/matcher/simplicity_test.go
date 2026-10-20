package matcher

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A Simplicity covenant order never auto-crosses, resting or incoming: no
// settlement owner (the settler, the bridge, Matched) carries its terms.
func TestSimplicityCovenantHasNoAutoCrossOwner(t *testing.T) {
	sim := &seqobv1.Offer{Settlement: &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: &seqobv1.SimplicityCovenantTerms{}}}
	tap := &seqobv1.Offer{Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{}}}
	plain := &seqobv1.Offer{Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{}}}
	for name, c := range map[string][2]*seqobv1.Offer{
		"interactive vs resting simplicity": {plain, sim},
		"tapscript vs resting simplicity":   {tap, sim},
		"simplicity vs resting tapscript":   {sim, tap},
		"simplicity vs resting simplicity":  {sim, sim},
	} {
		if hasSettlementOwner(c[0], c[1]) {
			t.Errorf("%s: auto-crosses", name)
		}
	}
	if !hasSettlementOwner(plain, tap) {
		t.Fatal("a resting tapscript covenant lost its owner")
	}
}
