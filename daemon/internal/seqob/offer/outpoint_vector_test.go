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

var updateOfferVectors = flag.Bool("update-offer-vectors", false, "rewrite testdata/covenant_offer_vectors.json")

// The vector pins, across Go and the web wallet's seqob.js, that a covenant
// offer's signature is over terms that exclude its outpoint: an order signed
// where its maker funded it still verifies when the relay serves it where a
// fill round rested it again, (round txid, 2k+1). Both sides read the offers
// in the relay's wire form (protojson, proto names, bytes in base64), compute
// the same canonical bytes, and sign them to the same signature (RFC 6979).
const offerVectorFile = "testdata/covenant_offer_vectors.json"

type offerVectors struct {
	About        string          `json:"about"`
	MakerPriv    string          `json:"maker_priv"` // a test key (1), not a secret
	Funded       json.RawMessage `json:"funded"`     // as served at the outpoint its maker funded
	Moved        json.RawMessage `json:"moved"`      // as served after a fill round rested it again at (round, 3)
	Tampered     json.RawMessage `json:"tampered"`   // a signed term changed: must not verify
	CanonicalHex string          `json:"canonical_hex"`
}

var wire = protojson.MarshalOptions{UseProtoNames: true}

func vectorOffer() *seqobv1.Offer {
	a := strings.Repeat("1f", 32) // display ids of the pair
	b := strings.Repeat("2e", 32)
	rev := func(h string) string {
		x, _ := hex.DecodeString(h)
		for i, j := 0, len(x)-1; i < j; i, j = i+1, j-1 {
			x[i], x[j] = x[j], x[i]
		}
		return hex.EncodeToString(x)
	}
	b32 := func(c byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = c
		}
		return out
	}
	nums, _ := hex.DecodeString("50929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0")
	return &seqobv1.Offer{
		OfferId: "order-vector-1", SchemaVersion: 1,
		Pair:     &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b},
		TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL, BaseAmount: 600000, OfferAmount: 600000, OfferAsset: a,
		WantAmount: 1200000, WantAsset: b, AllowPartial: true, MinFill: 10000,
		CreatedAtUnix: 1792000000, ExpiresAtUnix: 1792600000, MinAnchorDepth: 2,
		Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
			CovenantTxid: strings.Repeat("a1", 32), CovenantVout: 0,
			AssetA: rev(a), AssetB: rev(b), RateNum: 2, RateDen: 1,
			MakerProg: b32(0x31), MakerProgVer: 1, MinLot: 10000, ExpiryLocktime: 1792611631,
			MakerX: b32(0x41), InternalKey: nums,
		}},
	}
}

func TestCovenantOfferVerifiesAfterTheRelayMovesIt(t *testing.T) {
	if *updateOfferVectors {
		priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 1))
		o := vectorOffer()
		if err := SignOffer(o, priv); err != nil {
			t.Fatal(err)
		}
		canon, _ := CanonicalOfferBytes(o)
		funded, _ := wire.Marshal(o)
		moved := cloneOffer(o)
		moved.GetCovenant().CovenantTxid = strings.Repeat("c7", 32)
		moved.GetCovenant().CovenantVout = 3
		mv, _ := wire.Marshal(moved)
		bad := cloneOffer(o)
		bad.GetCovenant().RateNum = 3
		bd, _ := wire.Marshal(bad)
		v := offerVectors{
			About: "A covenant offer signed at its funding outpoint (funded), served by the relay after a fill round rested it " +
				"again at (round txid, 3) (moved), and with a signed term changed (tampered). The outpoint is outside the signed " +
				"bytes: funded and moved verify, in Go (seqdex internal/seqob/offer) and in the web wallet's seqob.js, with the " +
				"same canonical bytes and the same RFC 6979 signature by maker_priv, a test key; tampered verifies in neither.",
			MakerPriv: hex.EncodeToString(append(make([]byte, 31), 1)),
			Funded:    funded, Moved: mv, Tampered: bd, CanonicalHex: hex.EncodeToString(canon),
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(offerVectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(offerVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var v offerVectors
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
	funded, moved, tampered := read("funded", v.Funded), read("moved", v.Moved), read("tampered", v.Tampered)
	if moved.GetCovenant().GetCovenantVout() != 3 || moved.GetCovenant().GetCovenantTxid() == funded.GetCovenant().GetCovenantTxid() {
		t.Fatal("the moved offer does not rest elsewhere")
	}
	for name, o := range map[string]*seqobv1.Offer{"funded": funded, "moved": moved} {
		if err := VerifyOffer(o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, _ := CanonicalOfferBytes(o)
		if hex.EncodeToString(c) != v.CanonicalHex {
			t.Fatalf("%s: canonical bytes\n got %x\nwant %s", name, c, v.CanonicalHex)
		}
	}
	if err := VerifyOffer(tampered); err == nil {
		t.Fatal("a changed rate verified")
	}
	// The signature is the one SignOffer makes again (RFC 6979), which the
	// wallet's signer makes too.
	pk, _ := hex.DecodeString(v.MakerPriv)
	priv, _ := btcec.PrivKeyFromBytes(pk)
	again := cloneOffer(moved)
	if err := SignOffer(again, priv); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(again.GetMakerSig()) != hex.EncodeToString(funded.GetMakerSig()) {
		t.Fatal("signing the moved offer again does not give the funded offer's signature")
	}
}
