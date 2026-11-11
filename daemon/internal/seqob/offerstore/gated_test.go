package offerstore

import (
	"strings"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// PV5 Q1: the sweep delists a gated order at its expiry less the margin by the
// clock, whatever the offer's own expires_at_unix says.
func TestSweepDelistsAGatedOrderAtItsExpiry(t *testing.T) {
	now := time.Unix(1750000000, 0)
	s := New(func() time.Time { return now })
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750090000) // the offer itself would rest a day
	o.Settlement = &seqobv1.Offer_GatedOrder{GatedOrder: &seqobv1.GatedOrderTerms{
		CovenantTxid: strings.Repeat("ab", 32), Expiry: 1750000300}}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(1750000300-offer.GatedDelistMargin-1, 0)
	if n := s.SweepExpired(); n != 0 {
		t.Fatalf("swept %d a second before the margin", n)
	}
	now = time.Unix(1750000300-offer.GatedDelistMargin, 0)
	if n := s.SweepExpired(); n != 1 {
		t.Fatalf("swept %d at the margin, want 1", n)
	}
}
