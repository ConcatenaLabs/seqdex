package offer

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"google.golang.org/protobuf/encoding/protojson"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

var updateRestingVectors = flag.Bool("update-resting-vectors", false, "rewrite testdata/resting_leaf_offer_vectors.json")

// The vector pins, across Go and the web wallet's seqob.js, the signed bytes of
// an order resting as a leaf in an operator's tree: its terms, operator and
// order id are signed; its locator (the coin it rests at, what that coin holds)
// is not, so the offer its maker signed where the order first rested verifies
// where a fill rested it again. Both sides read the relay's wire form
// (protojson, proto names, bytes in base64), compute the same canonical bytes
// and sign them to the same signature (RFC 6979).
const restingVectorFile = "testdata/resting_leaf_offer_vectors.json"

type restingVectors struct {
	About         string          `json:"about"`
	MakerPriv     string          `json:"maker_priv"` // a test key (1), not a secret
	Rested        json.RawMessage `json:"rested"`     // as posted where the order first rests
	Moved         json.RawMessage `json:"moved"`      // as served after a part fill rested it again at another coin
	TamperedRate  json.RawMessage `json:"tampered_rate"`
	TamperedOrder json.RawMessage `json:"tampered_order"` // another order id: signed, so it must not verify
	CanonicalHex  string          `json:"canonical_hex"`
}

func restingVectorOffer() *seqobv1.Offer {
	a := strings.Repeat("1f", 32)
	b := strings.Repeat("2e", 32)
	b32 := func(c byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = c
		}
		return out
	}
	return &seqobv1.Offer{
		OfferId: "resting-vector-1", SchemaVersion: 1,
		Pair:     &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b},
		TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL, BaseAmount: 600000, OfferAmount: 600000, OfferAsset: a,
		WantAmount: 1200000, WantAsset: b, AllowPartial: true, MinFill: 10000,
		CreatedAtUnix: 1792000000, ExpiresAtUnix: 1792600000, MinAnchorDepth: 2,
		Settlement: &seqobv1.Offer_RestingLeaf{RestingLeaf: &seqobv1.RestingLeafTerms{
			OperatorUrl: "https://sequentiatestnet.com/arca", OperatorKey: strings.Repeat("5c", 32),
			OrderId: strings.Repeat("d1", 32), AssetA: a, AssetB: b, RateNum: 2, RateDen: 1, MinLot: 10000,
			LandedMaker: b32(0x31), LandedExpiry: 1792611631, RefundKey: b32(0x41),
			Coin: strings.Repeat("d1", 32), Value: 600000,
		}},
	}
}

func TestRestingOfferVerifiesWhereTheRelayFollowsIt(t *testing.T) {
	if *updateRestingVectors {
		priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 1))
		o := restingVectorOffer()
		if err := SignOffer(o, priv); err != nil {
			t.Fatal(err)
		}
		canon, _ := CanonicalOfferBytes(o)
		rested, _ := wire.Marshal(o)
		moved := cloneOffer(o)
		SetRestingLocator(moved, strings.Repeat("e2", 32), 300000)
		mv, _ := wire.Marshal(moved)
		badRate := cloneOffer(o)
		badRate.GetRestingLeaf().RateNum = 3
		br, _ := wire.Marshal(badRate)
		badOrder := cloneOffer(o)
		badOrder.GetRestingLeaf().OrderId = strings.Repeat("d2", 32)
		bo, _ := wire.Marshal(badOrder)
		v := restingVectors{
			About: "An order resting as a leaf in an operator's tree, signed where it first rests (rested), served by the relay " +
				"after a part fill rested it again at another coin holding less (moved), and with a signed term changed: the rate " +
				"(tampered_rate) or the order id (tampered_order). The locator (coin, value) is outside the signed bytes: rested " +
				"and moved verify, in Go (seqdex internal/seqob/offer) and in the web wallet's seqob.js, with the same canonical " +
				"bytes and the same RFC 6979 signature by maker_priv, a test key; the tampered ones verify in neither.",
			MakerPriv: hex.EncodeToString(append(make([]byte, 31), 1)),
			Rested:    rested, Moved: mv, TamperedRate: br, TamperedOrder: bo, CanonicalHex: hex.EncodeToString(canon),
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(restingVectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(restingVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var v restingVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	read := func(name string, m json.RawMessage) *seqobv1.Offer {
		var o seqobv1.Offer
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m, &o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return &o
	}
	rested, moved := read("rested", v.Rested), read("moved", v.Moved)
	if c, val, _ := RestingLocator(moved); c == rested.GetRestingLeaf().GetCoin() || val != 300000 {
		t.Fatal("the moved offer does not rest elsewhere")
	}
	for name, o := range map[string]*seqobv1.Offer{"rested": rested, "moved": moved} {
		if err := VerifyOffer(o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := CheckRestingTerms(o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, _ := CanonicalOfferBytes(o)
		if hex.EncodeToString(c) != v.CanonicalHex {
			t.Fatalf("%s: canonical bytes\n got %x\nwant %s", name, c, v.CanonicalHex)
		}
	}
	for name, m := range map[string]json.RawMessage{"tampered_rate": v.TamperedRate, "tampered_order": v.TamperedOrder} {
		if err := VerifyOffer(read(name, m)); err == nil {
			t.Fatalf("%s verified", name)
		}
	}
	pk, _ := hex.DecodeString(v.MakerPriv)
	priv, _ := btcec.PrivKeyFromBytes(pk)
	again := cloneOffer(moved)
	if err := SignOffer(again, priv); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(again.GetMakerSig()) != hex.EncodeToString(rested.GetMakerSig()) {
		t.Fatal("signing the moved offer again does not give the rested offer's signature")
	}
}

func TestCheckRestingTermsRefuses(t *testing.T) {
	cases := map[string]struct {
		edit func(o *seqobv1.Offer)
		want string
	}{
		"buy":          {func(o *seqobv1.Offer) { o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY }, "TRADE_DIR_SELL"},
		"rate":         {func(o *seqobv1.Offer) { o.GetRestingLeaf().RateNum = 3 }, "does not equal the offer's want/offer"},
		"height":       {func(o *seqobv1.Offer) { o.GetRestingLeaf().LandedExpiry = 50000 }, "is a height"},
		"url":          {func(o *seqobv1.Offer) { o.GetRestingLeaf().OperatorUrl = "file:///etc" }, "http(s) base URL"},
		"upper":        {func(o *seqobv1.Offer) { o.GetRestingLeaf().OrderId = strings.Repeat("D1", 32) }, "order_id must be 32-byte lowercase hex"},
		"same asset":   {func(o *seqobv1.Offer) { o.GetRestingLeaf().AssetB = o.GetRestingLeaf().GetAssetA() }, "same asset"},
		"other asset":  {func(o *seqobv1.Offer) { o.GetRestingLeaf().AssetB = strings.Repeat("3d", 32) }, "but the offer gives"},
		"below lot":    {func(o *seqobv1.Offer) { o.GetRestingLeaf().Value = 9999 }, "below the order's min_lot"},
		"above signed": {func(o *seqobv1.Offer) { o.GetRestingLeaf().Value = 600001 }, "more than the offer_amount 600000 its maker signed"},
		"no key":       {func(o *seqobv1.Offer) { o.GetRestingLeaf().RefundKey = nil }, "32 bytes"},
	}
	if err := CheckRestingTerms(restingVectorOffer()); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, c := range cases {
		o := restingVectorOffer()
		c.edit(o)
		err := CheckRestingTerms(o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if !sameRatio(1<<63, 2, 3<<62, 3) || sameRatio(1<<63, 2, 3<<62, 4) {
		t.Fatal("sameRatio is not exact on 128-bit products")
	}
}
