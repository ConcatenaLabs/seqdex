package watcher

import (
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A covenant offer is admitted only when the chain backs every advertised fact,
// and is listed at what its outpoint holds: its signed size, or less when the
// outpoint is the order resting at its remainder after a part fill.
func TestSubmitProbeChecksTheOutpoint(t *testing.T) {
	terms := sampleTerms("aa", 0) // min_lot 5,000,000
	exp, err := expectFromTerms(terms)
	if err != nil {
		t.Fatal(err)
	}
	offer := &seqobv1.Offer{OfferAmount: 90_000_000, Settlement: &seqobv1.Offer_Covenant{Covenant: terms}}
	at := func(v uint64) CovState {
		return CovState{Unspent: true, Value: v, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex}
	}
	cases := []struct {
		name   string
		st     CovState
		listed uint64
		want   string
	}{
		{"funded as signed", at(90_000_000), 90_000_000, ""},
		{"its remainder after a part fill", at(37_000_000), 37_000_000, ""},
		{"a remainder of exactly min_lot (a whole fill takes it)", at(5_000_000), 5_000_000, ""},
		{"spent or absent", CovState{}, 0, "not an unspent output"},
		{"another program", CovState{Unspent: true, Value: 37_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: "5120" + strings.Repeat("77", 32)}, 0, "does not pay the program"},
		{"wrong asset", CovState{Unspent: true, Value: 90_000_000, AssetDisplay: "cafe", SPKHex: exp.OrderSPKHex}, 0, "holds asset"},
		{"more than the maker signed", at(90_000_001), 0, "holds 90000001 atoms, more than the offer_amount 90000000 its maker signed"},
		{"below min_lot", at(4_999_999), 0, "holds 4999999 atoms, below the order's min_lot 5000000"},
		{"confidential", CovState{Unspent: true, Confidential: true, SPKHex: exp.OrderSPKHex}, 0, "is confidential"},
	}
	for _, c := range cases {
		chain := newFakeChain()
		chain.states["aa:0"] = c.st
		listed, err := SubmitProbe{Chain: chain}.CheckOffer(context.Background(), offer)
		if c.want == "" && (err != nil || listed != c.listed) {
			t.Fatalf("%s: listed %d, err %v; want %d", c.name, listed, err, c.listed)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
	// Non-covenant offers and a nil chain pass untouched, at their signed size.
	if listed, err := (SubmitProbe{}).CheckOffer(context.Background(), &seqobv1.Offer{}); err != nil || listed != 0 {
		t.Fatal(listed, err)
	}
}
