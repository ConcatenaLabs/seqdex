package offerstore

import (
	"errors"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/btcsuite/btcd/btcec/v2"
)

// covAt is mkCovOffer created at a given time.
func covAt(t *testing.T, k *btcec.PrivateKey, id, txid string, vout uint32, created uint64) *seqobv1.Offer {
	t.Helper()
	o := mkCovOffer(t, k, id, txid, vout, 90)
	o.CreatedAtUnix = created
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

func listedIDs(s *Store, txid string, vout uint32) []string {
	var ids []string
	for _, e := range s.SnapshotCovenants() {
		if tx, v, _ := offer.CovenantOutpoint(e.Offer); tx == txid && v == vout {
			ids = append(ids, e.Key.OfferID)
		}
	}
	return ids
}

// One covenant order per outpoint: the same maker's later offer replaces the
// listed one, an earlier or equal one is refused, and another maker's is refused,
// in whichever order they arrive.
func TestOneOfferPerCovenantOutpoint(t *testing.T) {
	s := New(nil)
	m, other := key(t), key(t)
	old, relist := covAt(t, m, "old", "aabb", 1, 1750000000), covAt(t, m, "relist", "aabb", 1, 1750000100)

	if _, err := s.Submit(old); err != nil {
		t.Fatal(err)
	}
	_, _, ch := s.Subscribe(old.GetPair(), false)
	if _, err := s.Submit(relist); err != nil {
		t.Fatalf("the maker's later offer: %v", err)
	}
	if ids := listedIDs(s, "aabb", 1); len(ids) != 1 || ids[0] != "relist" {
		t.Fatalf("listed at aabb:1: %v", ids)
	}
	if ev := recv(t, ch); ev.Type != EventStatus || ev.Status.GetOfferId() != "old" || ev.Status.GetStatus() != seqobv1.OfferStatus_OFFER_STATUS_CANCELLED {
		t.Fatalf("first delta %+v", ev)
	}
	if ev := recv(t, ch); ev.Type != EventRemoved || ev.Ref.GetOfferId() != "old" {
		t.Fatalf("second delta %+v", ev)
	}
	// The older one posted again (a re-post of an earlier snapshot): refused.
	if _, err := s.Submit(old); !errors.Is(err, ErrOutpointListed) {
		t.Fatalf("the maker's earlier offer: %v", err)
	}
	if _, err := s.Submit(covAt(t, m, "same-time", "aabb", 1, 1750000100)); !errors.Is(err, ErrOutpointListed) {
		t.Fatalf("an offer created at the same time: %v", err)
	}
	// Another maker, however late: refused.
	if _, err := s.Submit(covAt(t, other, "theirs", "AABB", 1, 1760000000)); !errors.Is(err, ErrOutpointListed) {
		t.Fatalf("another maker's offer: %v", err)
	}
	if ids := listedIDs(s, "aabb", 1); len(ids) != 1 || ids[0] != "relist" {
		t.Fatalf("listed at aabb:1: %v", ids)
	}

	// The reverse arrival order ends the same way.
	s2 := New(nil)
	if _, err := s2.Submit(relist); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Submit(old); !errors.Is(err, ErrOutpointListed) {
		t.Fatalf("reverse order: %v", err)
	}
	if ids := listedIDs(s2, "aabb", 1); len(ids) != 1 || ids[0] != "relist" {
		t.Fatalf("reverse order listed: %v", ids)
	}

	// A re-rest onto a remainder another offer lists: the same rule. The maker's
	// earlier offer at the funding outpoint is re-rested where its later one rests.
	s3 := New(nil)
	if _, err := s3.Submit(covAt(t, m, "funding", "ccdd", 0, 1750000000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.SubmitAt(covAt(t, m, "adopted", "eeff", 1, 1750000200), 40); err != nil {
		t.Fatal(err)
	}
	if err := s3.RepointCovenant(Key{MakerPubkey: old.GetMakerPubkey(), OfferID: "funding"}, "eeff", 1, 40, true); err != nil {
		t.Fatal(err)
	}
	if ids := listedIDs(s3, "eeff", 1); len(ids) != 1 || ids[0] != "adopted" {
		t.Fatalf("after the re-rest: %v", ids)
	}
	if _, ok := s3.Get(Key{MakerPubkey: old.GetMakerPubkey(), OfferID: "funding"}); ok {
		t.Fatal("the earlier offer still rests")
	}
	// A plain re-rest is still one order at its remainder; the index follows it.
	if err := s3.RepointCovenant(Key{MakerPubkey: old.GetMakerPubkey(), OfferID: "adopted"}, "1122", 1, 20, true); err != nil {
		t.Fatal(err)
	}
	if k, ok := s3.ListedAt("1122", 1); !ok || k.OfferID != "adopted" {
		t.Fatalf("index after the re-rest: %v %v", k, ok)
	}
	if _, ok := s3.ListedAt("eeff", 1); ok {
		t.Fatal("the old outpoint is still indexed")
	}
	// Once the order leaves, its outpoint is free.
	if err := s3.RemoveCovenantFilled(Key{MakerPubkey: old.GetMakerPubkey(), OfferID: "adopted"}, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Submit(covAt(t, other, "next", "1122", 1, 1)); err != nil {
		t.Fatalf("a freed outpoint: %v", err)
	}
}

// The rule holds when a covenant book file is read back: of two kept lines for
// one outpoint the maker's later offer stands, read in either order, and
// another maker's is not listed again.
func TestRestoredCovenantBookListsOneOfferPerOutpoint(t *testing.T) {
	m, other := key(t), key(t)
	old, relist := covAt(t, m, "old", "aabb", 1, 1750000000), covAt(t, m, "relist", "aabb", 1, 1750000100)
	theirs := covAt(t, other, "theirs", "aabb", 1, 1760000000)
	for _, order := range [][]*seqobv1.Offer{{old, relist, theirs}, {theirs, relist, old}} {
		s := New(nil)
		var refused int
		for _, o := range order {
			if _, err := s.RestoreCovenant(o, 0, false, false); errors.Is(err, ErrOutpointListed) {
				refused++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		ids := listedIDs(s, "aabb", 1)
		if len(ids) != 1 {
			t.Fatalf("order %s: listed %v", order[0].GetOfferId(), ids)
		}
		// Whoever is first at the outpoint stands against another maker; the same
		// maker's later offer replaces its earlier one rather than being refused.
		want, wantRefused := "relist", 1
		if order[0] == theirs {
			want, wantRefused = "theirs", 2
		}
		if ids[0] != want || refused != wantRefused {
			t.Fatalf("order %s: listed %v (%d refused), want %s (%d)", order[0].GetOfferId(), ids, refused, want, wantRefused)
		}
	}
}
