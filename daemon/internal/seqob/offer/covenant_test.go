package offer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"google.golang.org/protobuf/encoding/protojson"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

var updateSimOfferVectors = flag.Bool("update-simplicity-offer-vectors", false, "rewrite testdata/simplicity_offer_vectors.json")

// The vector pins, across Go and the web wallet's seqob.js, a Simplicity
// covenant offer's signed bytes: like CovenantTerms', they exclude its
// outpoint, so the offer verifies where its maker funded it and where the
// relay serves it after a partial fill rested it again.
const simOfferVectorFile = "testdata/simplicity_offer_vectors.json"

func simVectorOffer() *seqobv1.Offer {
	a := strings.Repeat("1f", 32) // display ids of the pair
	b := strings.Repeat("2e", 32)
	rev := func(h string) string {
		x, _ := hex.DecodeString(h)
		for i, j := 0, len(x)-1; i < j; i, j = i+1, j-1 {
			x[i], x[j] = x[j], x[i]
		}
		return hex.EncodeToString(x)
	}
	k := sha256.Sum256([]byte("simplicity offer vector refund key"))
	_, pub := btcec.PrivKeyFromBytes(k[:])
	return &seqobv1.Offer{
		OfferId: "simplicity-order-vector-1", SchemaVersion: 1,
		Pair:     &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b},
		TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL, BaseAmount: 20000000000000, OfferAmount: 20000000000000, OfferAsset: a,
		// 2e13 at 9,000,000,000,007 / 10^13: want = offer * num / den exactly.
		WantAmount: 18_000_000_000_014, WantAsset: b, AllowPartial: true, MinFill: 1000000,
		CreatedAtUnix: 1792000000, ExpiresAtUnix: 1792600000, MinAnchorDepth: 2,
		Settlement: &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: &seqobv1.SimplicityCovenantTerms{
			CovenantTxid: strings.Repeat("a1", 32), CovenantVout: 0,
			AssetA: rev(a), AssetB: rev(b), RateNum: 9_000_000_000_007, RateDen: 10_000_000_000_000, MinLot: 1000000,
			MakerScript:    append([]byte{0x00, 0x14}, bytes.Repeat([]byte{0x31}, 20)...),
			ExpiryLocktime: 1792611631, RefundKey: schnorr.SerializePubKey(pub),
			TemplateHash: covenant.SimOrderTemplateHash,
		}},
	}
}

func TestSimplicityOfferVerifiesAfterTheRelayMovesIt(t *testing.T) {
	if *updateSimOfferVectors {
		priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 1))
		o := simVectorOffer()
		if err := SignOffer(o, priv); err != nil {
			t.Fatal(err)
		}
		canon, _ := CanonicalOfferBytes(o)
		funded, _ := wire.Marshal(o)
		moved := cloneOffer(o)
		SetCovenantOutpoint(moved, strings.Repeat("c7", 32), 3)
		mv, _ := wire.Marshal(moved)
		bad := cloneOffer(o)
		bad.GetSimplicityCovenant().RateNum--
		bd, _ := wire.Marshal(bad)
		v := offerVectors{
			About: "A Simplicity covenant offer (SimplicityCovenantTerms, the template seqdex/order) signed at its funding " +
				"outpoint (funded), served by the relay after a partial fill rested it again at (fill txid, 3) (moved), and " +
				"with a signed term changed (tampered). The outpoint is outside the signed bytes: funded and moved verify, in " +
				"Go (seqdex internal/seqob/offer) and in the web wallet's seqob.js, with the same canonical bytes and the same " +
				"RFC 6979 signature by maker_priv, a test key; tampered verifies in neither.",
			MakerPriv: hex.EncodeToString(append(make([]byte, 31), 1)),
			Funded:    funded, Moved: mv, Tampered: bd, CanonicalHex: hex.EncodeToString(canon),
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(simOfferVectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(simOfferVectorFile)
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
	ft, _, _ := CovenantOutpoint(funded)
	mt, mv, _ := CovenantOutpoint(moved)
	if mv != 3 || mt == ft || moved.GetSimplicityCovenant() == nil {
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
	// The terms make an order this build fills.
	if _, err := SimOrderOf(funded.GetSimplicityCovenant()); err != nil {
		t.Fatal(err)
	}
}

func TestCovenantAccessorsTakeEitherMember(t *testing.T) {
	sim := simVectorOffer()
	tap := vectorOffer()
	plain := &seqobv1.Offer{Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{}}}
	if !IsCovenant(sim) || !IsCovenant(tap) || IsCovenant(plain) {
		t.Fatal("IsCovenant")
	}
	for _, o := range []*seqobv1.Offer{sim, tap} {
		if !SetCovenantOutpoint(o, strings.Repeat("dd", 32), 5) {
			t.Fatal("SetCovenantOutpoint")
		}
		if txid, vout, ok := CovenantOutpoint(o); !ok || vout != 5 || txid != strings.Repeat("dd", 32) {
			t.Fatal("CovenantOutpoint")
		}
	}
	if SetCovenantOutpoint(plain, "x", 1) {
		t.Fatal("a same-chain offer was re-pointed")
	}
	if CovenantMinLot(sim) != 1000000 || CovenantMinLot(tap) != 10000 || CovenantMinLot(plain) != 0 {
		t.Fatal("CovenantMinLot")
	}
}

func TestSimOrderOfRefusesWhatNoOrderShouldCarry(t *testing.T) {
	for name, f := range map[string]func(*seqobv1.SimplicityCovenantTerms){
		"another template": func(t *seqobv1.SimplicityCovenantTerms) { t.TemplateHash = strings.Repeat("00", 32) },
		"no template":      func(t *seqobv1.SimplicityCovenantTerms) { t.TemplateHash = "" },
		"short asset":      func(t *seqobv1.SimplicityCovenantTerms) { t.AssetA = "ab" },
		"short refund key": func(t *seqobv1.SimplicityCovenantTerms) { t.RefundKey = t.RefundKey[:31] },
		"no point":         func(t *seqobv1.SimplicityCovenantTerms) { t.RefundKey = make([]byte, 32) },
		"rate_den 0":       func(t *seqobv1.SimplicityCovenantTerms) { t.RateDen = 0 },
		"min_lot 0":        func(t *seqobv1.SimplicityCovenantTerms) { t.MinLot = 0 },
		"no maker":         func(t *seqobv1.SimplicityCovenantTerms) { t.MakerScript = nil },
		"one asset":        func(t *seqobv1.SimplicityCovenantTerms) { t.AssetB = t.AssetA },
	} {
		o := simVectorOffer()
		f(o.GetSimplicityCovenant())
		if _, err := SimOrderOf(o.GetSimplicityCovenant()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
