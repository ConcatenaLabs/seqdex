package validator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A Simplicity covenant offer: 2e13 atoms of A at 9,000,000,000,007 B for every
// 10^13 A. The tapscript leaf could not carry it (its fill product is far past
// 2^63); this one compares the price on 128-bit products, so the relay takes it.
func simOffer(o *seqobv1.Offer) {
	a := strings.Repeat("01", 31) + "ff"
	b := strings.Repeat("02", 31) + "ee"
	o.Pair = &seqobv1.AssetPair{BaseAsset: a, QuoteAsset: b}
	o.OfferAsset, o.WantAsset = a, b
	o.BaseAmount, o.OfferAmount, o.WantAmount = 20_000_000_000_000, 20_000_000_000_000, 18_000_000_000_014
	k := sha256.Sum256([]byte("refund"))
	_, pub := btcec.PrivKeyFromBytes(k[:])
	o.Settlement = &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: &seqobv1.SimplicityCovenantTerms{
		CovenantTxid: strings.Repeat("ab", 32), AssetA: "ff" + strings.Repeat("01", 31), AssetB: "ee" + strings.Repeat("02", 31),
		RateNum: 9_000_000_000_007, RateDen: 10_000_000_000_000, MinLot: 1_000_000,
		MakerScript: append([]byte{0x00, 0x14}, bytes.Repeat([]byte{7}, 20)...), ExpiryLocktime: 1_792_611_631,
		RefundKey: schnorr.SerializePubKey(pub), TemplateHash: covenant.SimOrderTemplateHash,
	}}
}

func TestSimplicityCovenantOffer(t *testing.T) {
	v := New(cfg(), nil)
	check := func(name string, mut func(*seqobv1.SimplicityCovenantTerms, *seqobv1.Offer), wantErr string) {
		t.Helper()
		o := signed(t, key(t), func(o *seqobv1.Offer) {
			simOffer(o)
			if mut != nil {
				mut(o.GetSimplicityCovenant(), o)
			}
		})
		err := v.ValidateOffer(context.Background(), o, "")
		if wantErr == "" && err != nil {
			t.Fatalf("%s: unexpected %v", name, err)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("%s: err = %v, want containing %q", name, err, wantErr)
		}
	}
	check("consistent, beyond the tapscript bound", nil, "")
	check("a median-time and a height expiry", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.ExpiryLocktime = 499_999_999 }, "")
	check("another template", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.TemplateHash = strings.Repeat("0", 64) }, "is not seqdex/order")
	check("rate above the offer", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.RateNum++ }, "does not equal the offer's want/offer")
	check("no expiry", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.ExpiryLocktime = 0 }, "expiry_locktime must be set")
	check("min_lot above the lock", func(st *seqobv1.SimplicityCovenantTerms, o *seqobv1.Offer) { st.MinLot = o.OfferAmount + 1 }, "exceeds the locked amount")
	check("assets swapped", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) {
		st.AssetA, st.AssetB = st.AssetB, st.AssetA
	}, "is not the offer_asset")
	check("a BUY", func(_ *seqobv1.SimplicityCovenantTerms, o *seqobv1.Offer) {
		// A well-formed BUY of the base B paid in A, which locks A: the covenant
		// rule, not the BUY shape, refuses it.
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.Pair = &seqobv1.AssetPair{BaseAsset: o.WantAsset, QuoteAsset: o.OfferAsset}
		o.BaseAmount = o.WantAmount
	}, "TRADE_DIR_SELL")
	check("no refund key", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.RefundKey = nil }, "refund_key")
	check("bad outpoint", func(st *seqobv1.SimplicityCovenantTerms, _ *seqobv1.Offer) { st.CovenantTxid = "ab" }, "covenant_txid")
	check("confidential", func(_ *seqobv1.SimplicityCovenantTerms, o *seqobv1.Offer) { o.Confidential = true }, "co-sign rail")
}
