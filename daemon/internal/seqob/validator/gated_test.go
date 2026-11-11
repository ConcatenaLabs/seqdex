package validator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// The default signed offer sells 100 of base for 45 of quote; the gated
// order's terms must say the same, with real 32-byte ids.
func gatedOffer(o *seqobv1.Offer) {
	a := strings.Repeat("01", 31) + "ff"
	b := strings.Repeat("02", 31) + "ee"
	o.Pair = &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b}
	o.OfferAsset, o.WantAsset = a, b
	o.Settlement = &seqobv1.Offer_GatedOrder{GatedOrder: &seqobv1.GatedOrderTerms{
		CovenantTxid: strings.Repeat("ab", 32), AssetA: "ff" + strings.Repeat("01", 31), AssetB: "ee" + strings.Repeat("02", 31),
		RateNum: 45, RateDen: 100, MinLot: 1, MakerProg: bytes.Repeat([]byte{0xc3}, 32), MakerProgVer: 1,
		Expiry: 1_800_000_000, PolicyX: bytes.Repeat([]byte{0x02}, 32), MakerEnclaveProg: bytes.Repeat([]byte{0xe5}, 32),
		PolicyServer: "https://policy.example/openamp", MakerAid: strings.Repeat("aa", 20),
	}}
}

// A gated order is listed only when its terms build its script, match the
// offer's price and assets, and name a policy server and an account a taker
// can reach.
func TestGatedOrderTerms(t *testing.T) {
	v := New(cfg(), nil)
	check := func(name string, mut func(*seqobv1.GatedOrderTerms, *seqobv1.Offer), wantErr string) {
		t.Helper()
		o := signed(t, key(t), func(o *seqobv1.Offer) {
			gatedOffer(o)
			mut(o.GetGatedOrder(), o)
		})
		err := v.ValidateOffer(context.Background(), o, "")
		if wantErr == "" && err != nil {
			t.Fatalf("%s: unexpected %v", name, err)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("%s: err = %v, want containing %q", name, err, wantErr)
		}
	}
	check("consistent", func(*seqobv1.GatedOrderTerms, *seqobv1.Offer) {}, "")
	check("with clawback", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.IssuerX = bytes.Repeat([]byte{0x03}, 32) }, "")
	check("local server over http", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.PolicyServer = "http://127.0.0.1:9999" }, "")
	check("rate off the offer", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.RateNum = 46 }, "does not equal")
	check("assets swapped", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.AssetA, g.AssetB = g.AssetB, g.AssetA }, "is not the offer_asset")
	check("a height expiry", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.Expiry = 400 }, "is a height")
	check("short policy key", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.PolicyX = []byte{1} }, "policy_x")
	check("short enclave", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.MakerEnclaveProg = []byte{1} }, "maker_enclave_prog")
	check("short issuer key", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.IssuerX = []byte{1} }, "issuer_x")
	check("plain http server", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.PolicyServer = "http://policy.example" }, "https")
	check("no server", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.PolicyServer = "" }, "https")
	check("bad account", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.MakerAid = "zz" }, "maker_aid")
	check("a buy", func(_ *seqobv1.GatedOrderTerms, o *seqobv1.Offer) { o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY }, "BUY")
	check("min_lot above lock", func(g *seqobv1.GatedOrderTerms, _ *seqobv1.Offer) { g.MinLot = 101 }, "exceeds the locked amount")
}
