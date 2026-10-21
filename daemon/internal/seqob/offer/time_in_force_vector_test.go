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

var updateTIFVectors = flag.Bool("update-time-in-force-vectors", false, "rewrite testdata/time_in_force_offer_vectors.json")

// The vector pins, across Go and the web wallet's seqob.js, that an offer's
// time_in_force (field 24) is part of its signed bytes: an offer signed with
// each value verifies on both sides with the same canonical bytes and the
// same RFC 6979 signature, and one whose time_in_force was changed after
// signing verifies on neither. TIME_IN_FORCE_UNSPECIFIED is proto3's zero and
// is omitted from the bytes.
const tifVectorFile = "testdata/time_in_force_offer_vectors.json"

type tifCase struct {
	Name         string          `json:"name"`
	Offer        json.RawMessage `json:"offer"`
	CanonicalHex string          `json:"canonical_hex"`
}

type tifVectors struct {
	About     string          `json:"about"`
	MakerPriv string          `json:"maker_priv"`
	Cases     []tifCase       `json:"cases"`
	Tampered  json.RawMessage `json:"tampered"`
}

func tifOffer(tif seqobv1.TimeInForce) *seqobv1.Offer {
	a, b := strings.Repeat("1f", 32), strings.Repeat("2e", 32)
	return &seqobv1.Offer{
		OfferId: "time-in-force-vector-" + strings.ToLower(tif.String()), SchemaVersion: 1,
		Pair:     &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b},
		TradeDir: seqobv1.TradeDir_TRADE_DIR_BUY, BaseAmount: 600000, OfferAmount: 1200000, OfferAsset: b,
		WantAmount: 600000, WantAsset: a, AllowPartial: true, MinFill: 10000,
		CreatedAtUnix: 1792000000, ExpiresAtUnix: 1792600000, TimeInForce: tif,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "tb1qtimeinforcevector"}},
	}
}

func TestTimeInForceIsSigned(t *testing.T) {
	if *updateTIFVectors {
		priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 1))
		v := tifVectors{
			About: "Offers signed with each time_in_force (field 24; UNSPECIFIED, proto3's zero, is omitted from the bytes), " +
				"and one whose time_in_force was changed after signing (tampered). Each case verifies in Go (seqdex " +
				"internal/seqob/offer) and in the web wallet's seqob.js with the same canonical bytes and the same RFC 6979 " +
				"signature by maker_priv, a test key; tampered verifies in neither.",
			MakerPriv: hex.EncodeToString(append(make([]byte, 31), 1)),
		}
		for _, tif := range []seqobv1.TimeInForce{seqobv1.TimeInForce_TIME_IN_FORCE_UNSPECIFIED, seqobv1.TimeInForce_TIME_IN_FORCE_IOC,
			seqobv1.TimeInForce_TIME_IN_FORCE_FOK, seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY} {
			o := tifOffer(tif)
			if err := SignOffer(o, priv); err != nil {
				t.Fatal(err)
			}
			c, _ := CanonicalOfferBytes(o)
			w, _ := wire.Marshal(o)
			v.Cases = append(v.Cases, tifCase{Name: tif.String(), Offer: w, CanonicalHex: hex.EncodeToString(c)})
		}
		bad := tifOffer(seqobv1.TimeInForce_TIME_IN_FORCE_IOC)
		if err := SignOffer(bad, priv); err != nil {
			t.Fatal(err)
		}
		bad.TimeInForce = seqobv1.TimeInForce_TIME_IN_FORCE_FOK
		v.Tampered, _ = wire.Marshal(bad)
		out, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(tifVectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(tifVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var v tifVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	read := func(m json.RawMessage) *seqobv1.Offer {
		var o seqobv1.Offer
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m, &o); err != nil {
			t.Fatal(err)
		}
		return &o
	}
	if len(v.Cases) != 4 {
		t.Fatalf("%d cases", len(v.Cases))
	}
	pk, _ := hex.DecodeString(v.MakerPriv)
	priv, _ := btcec.PrivKeyFromBytes(pk)
	for _, c := range v.Cases {
		o := read(c.Offer)
		if o.GetTimeInForce().String() != c.Name {
			t.Fatalf("%s: read %s", c.Name, o.GetTimeInForce())
		}
		if err := VerifyOffer(o); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		b, _ := CanonicalOfferBytes(o)
		if hex.EncodeToString(b) != c.CanonicalHex {
			t.Fatalf("%s: canonical bytes", c.Name)
		}
		again := cloneOffer(o)
		if err := SignOffer(again, priv); err != nil || hex.EncodeToString(again.GetMakerSig()) != hex.EncodeToString(o.GetMakerSig()) {
			t.Fatalf("%s: signing again gives another signature", c.Name)
		}
	}
	if VerifyOffer(read(v.Tampered)) == nil {
		t.Fatal("a changed time_in_force verified")
	}
}
