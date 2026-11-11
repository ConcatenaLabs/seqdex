package validator

import (
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func restrictedOffer(quote string, rt *seqobv1.RestrictedTerms) *seqobv1.Offer {
	base := strings.Repeat("ab", 32)
	return &seqobv1.Offer{Pair: &seqobv1.AssetPair{BaseAsset: base, QuoteAsset: quote}, TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount: 300, OfferAmount: 300, OfferAsset: base, WantAmount: 100000, WantAsset: quote,
		Settlement: &seqobv1.Offer_Restricted{Restricted: rt}}
}

func TestRestrictedQuote(t *testing.T) {
	ok := func() *seqobv1.RestrictedTerms {
		return &seqobv1.RestrictedTerms{PolicyServer: "https://example.org/openamp", PolicyX: make([]byte, 32),
			MakerAid: strings.Repeat("01", 20), PotAsset: strings.Repeat("cd", 32), PotAmount: 20000}
	}
	if err := checkRestricted(restrictedOffer("BTC", ok())); err != nil {
		t.Fatalf("a well-formed quote for bitcoin: %v", err)
	}
	for name, mut := range map[string]func(*seqobv1.RestrictedTerms){
		"http":             func(r *seqobv1.RestrictedTerms) { r.PolicyServer = "http://example.org" },
		"credentials":      func(r *seqobv1.RestrictedTerms) { r.PolicyServer = "https://u:p@example.org" },
		"short key":        func(r *seqobv1.RestrictedTerms) { r.PolicyX = make([]byte, 31) },
		"account":          func(r *seqobv1.RestrictedTerms) { r.MakerAid = "zz" },
		"no pot":           func(r *seqobv1.RestrictedTerms) { r.PotAmount = 0 },
		"pot is the asset": func(r *seqobv1.RestrictedTerms) { r.PotAsset = strings.Repeat("ab", 32) },
	} {
		r := ok()
		mut(r)
		if checkRestricted(restrictedOffer("BTC", r)) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	conf := restrictedOffer("BTC", ok())
	conf.Confidential = true
	if checkRestricted(conf) == nil {
		t.Error("a confidential restricted quote was accepted")
	}
	if checkRestricted(restrictedOffer(strings.Repeat("ef", 32), ok())) == nil {
		t.Error("a quote for an asset with a fee pot and no receive address was accepted")
	}
}
