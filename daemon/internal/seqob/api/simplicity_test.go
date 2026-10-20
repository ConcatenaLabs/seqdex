package api

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A Simplicity covenant order needs no live maker and takes no lift slot.
func TestSimplicityCovenantNeedsNoLiveMaker(t *testing.T) {
	sim := &seqobv1.Offer{Settlement: &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: &seqobv1.SimplicityCovenantTerms{}}}
	if needsLiveMaker(sim) {
		t.Fatal("needsLiveMaker")
	}
	if !liftGuardExempts(sim) {
		t.Fatal("liftGuardExempts")
	}
}
