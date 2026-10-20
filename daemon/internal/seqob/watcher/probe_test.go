package watcher

import (
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A covenant offer is admitted only when the chain backs every advertised fact.
func TestSubmitProbeChecksTheOutpoint(t *testing.T) {
	terms := sampleTerms("aa", 0)
	exp, err := expectFromTerms(terms)
	if err != nil {
		t.Fatal(err)
	}
	offer := &seqobv1.Offer{OfferAmount: 90_000_000, Settlement: &seqobv1.Offer_Covenant{Covenant: terms}}
	good := CovState{Unspent: true, Value: 90_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex}
	cases := []struct {
		name string
		st   CovState
		want string
	}{
		{"funded as advertised", good, ""},
		{"spent or absent", CovState{}, "not an unspent output"},
		{"wrong program", CovState{Unspent: true, Value: 90_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: "5120" + strings.Repeat("77", 32)}, "does not pay the program"},
		{"wrong asset", CovState{Unspent: true, Value: 90_000_000, AssetDisplay: "cafe", SPKHex: exp.OrderSPKHex}, "holds asset"},
		{"over-advertised", CovState{Unspent: true, Value: 89_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex}, "offer advertises"},
	}
	for _, c := range cases {
		chain := newFakeChain()
		chain.states["aa:0"] = c.st
		err := SubmitProbe{Chain: chain}.CheckOffer(context.Background(), offer)
		if c.want == "" && err != nil {
			t.Fatalf("%s: unexpected %v", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
	// Non-covenant offers and a nil chain pass untouched.
	if err := (SubmitProbe{}).CheckOffer(context.Background(), &seqobv1.Offer{}); err != nil {
		t.Fatal(err)
	}
}
