package offerstore

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// mkSimOffer builds a signed Simplicity covenant SELL offer resting at txid:vout.
func mkSimOffer(t *testing.T, k *btcec.PrivateKey, id, txid string, vout uint32, base uint64) *seqobv1.Offer {
	t.Helper()
	o := mkCovOffer(t, k, id, txid, vout, base)
	o.Settlement = &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: &seqobv1.SimplicityCovenantTerms{
		CovenantTxid: txid, CovenantVout: vout, RateNum: 1, RateDen: 3, MinLot: 5, ExpiryLocktime: 500,
		MakerScript: []byte{0x51}, RefundKey: make([]byte, 32), TemplateHash: "t",
	}}
	o.MakerSig = nil
	o.MakerPubkey = ""
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

// The book treats a Simplicity covenant order as a covenant order: the watcher
// sees it in SnapshotCovenants with its own terms, a partial fill re-points it
// (its signature still verifies), a maker's disconnect spares it, and it is not
// counted as interactive capital.
func TestSimplicityCovenantRestsAsACovenant(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkSimOffer(t, k, "sim1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	covs := s.SnapshotCovenants()
	if len(covs) != 1 || covs[0].Sim == nil || covs[0].Terms != nil || covs[0].Sim.GetCovenantTxid() != "aabb" {
		t.Fatalf("snapshot covenants: %+v", covs)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "sim1"}
	if err := s.RepointCovenant(kk, "ccdd", 1, 60, true); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 60 || e.Offer.GetSimplicityCovenant().GetCovenantTxid() != "ccdd" || e.Offer.GetSimplicityCovenant().GetCovenantVout() != 1 {
		t.Fatalf("re-point: %+v", e)
	}
	if err := offer.VerifyOffer(e.Offer); err != nil {
		t.Fatalf("the re-pointed offer no longer verifies: %v", err)
	}
	if o.GetSimplicityCovenant().GetCovenantTxid() != "aabb" {
		t.Fatal("re-point mutated the submitted offer")
	}
	if n := s.RemoveByMaker(o.GetMakerPubkey()); n != 0 {
		t.Fatalf("RemoveByMaker evicted %d covenant offers", n)
	}
	if n := s.RemoveInteractiveByMaker(o.GetMakerPubkey()); n != 0 {
		t.Fatalf("RemoveInteractiveByMaker evicted %d covenant offers", n)
	}
	if err := s.RemoveCovenantFilled(kk, "filltx"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatal("a filled Simplicity order should be removed")
	}
}
