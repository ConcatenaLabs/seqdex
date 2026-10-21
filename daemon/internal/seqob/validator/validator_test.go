package validator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

var now = time.Unix(1750000000, 0)

func cfg() Config {
	c := DefaultConfig()
	c.Now = func() time.Time { return now }
	return c
}

func signed(t *testing.T, k *btcec.PrivateKey, mut func(*seqobv1.Offer)) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       "aaaa",
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: uint64(now.Add(time.Hour).Unix()),
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if mut != nil {
		mut(o)
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestValidOfferPasses(t *testing.T) {
	v := New(cfg(), nil)
	o := signed(t, key(t), nil)
	if err := v.ValidateOffer(context.Background(), o, "1.2.3.4"); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*seqobv1.Offer)
	}{
		{"zero amount", func(o *seqobv1.Offer) { o.OfferAmount = 0 }},
		{"wrong dir mapping", func(o *seqobv1.Offer) { o.WantAsset = "gold" }},
		{"identical assets", func(o *seqobv1.Offer) { o.Pair.QuoteAsset = "gold" }},
		{"no expiry", func(o *seqobv1.Offer) { o.ExpiresAtUnix = 0 }},
		{"expiry too far", func(o *seqobv1.Offer) { o.ExpiresAtUnix = uint64(now.Add(100 * 24 * time.Hour).Unix()) }},
		{"already expired", func(o *seqobv1.Offer) { o.ExpiresAtUnix = uint64(now.Add(-time.Hour).Unix()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New(cfg(), nil)
			o := signed(t, key(t), tc.mut)
			if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestBadSignatureRejected(t *testing.T) {
	v := New(cfg(), nil)
	o := signed(t, key(t), nil)
	o.WantAmount = 1 // breaks the signature
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected signature rejection")
	}
}

func TestUnknownAssetRejected(t *testing.T) {
	c := cfg()
	c.KnownAssets = map[string]bool{"gold": true, "usdx": true}
	v := New(c, nil)
	o := signed(t, key(t), func(o *seqobv1.Offer) {
		o.Pair.QuoteAsset = "silvr"
		o.WantAsset = "silvr"
	})
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected unknown-asset rejection")
	}
}

func TestRateLimitPerPubkey(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 2
	c.MaxOffersPerMinPerIP = 0
	v := New(c, nil)
	k := key(t)
	for i := 0; i < 2; i++ {
		o := signed(t, k, func(o *seqobv1.Offer) { o.OfferId = "id" })
		if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
			t.Fatalf("offer %d should pass: %v", i, err)
		}
	}
	o := signed(t, k, nil)
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected rate-limit rejection on 3rd offer")
	}
}

// TestForgedOfferDoesNotConsumeVictimBudget proves the per-maker_pubkey budget is
// charged only after maker_sig is verified: an attacker flooding forged offers
// that bear a victim's maker_pubkey (but an invalid signature) cannot lock the
// victim out, because those offers are rejected before the per-pubkey budget is
// touched.
func TestForgedOfferDoesNotConsumeVictimBudget(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 2
	c.MaxOffersPerMinPerIP = 0
	v := New(c, nil)
	victim := key(t)
	victimPub := hex.EncodeToString(victim.PubKey().SerializeCompressed())

	// Attacker forges offers bearing the victim's maker_pubkey but breaks the sig.
	for i := 0; i < 10; i++ {
		forged := signed(t, victim, nil)
		forged.MakerPubkey = victimPub
		forged.WantAmount = 999 // mutate after signing -> invalid maker_sig
		if err := v.ValidateOffer(context.Background(), forged, "9.9.9.9"); err == nil {
			t.Fatalf("forged offer %d must be rejected", i)
		}
	}

	// The victim's own per-pubkey budget is untouched: it can still post.
	for i := 0; i < 2; i++ {
		o := signed(t, victim, func(o *seqobv1.Offer) { o.OfferId = fmt.Sprintf("ok%d", i) })
		if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
			t.Fatalf("victim offer %d should pass: %v", i, err)
		}
	}
}

// fakeBook is an in-memory RestingOffers that mirrors what the offerstore would
// hold: the maker_sig currently resting under each (maker_pubkey, offer_id).
type fakeBook struct {
	mu   sync.Mutex
	sigs map[string][]byte
}

func newFakeBook() *fakeBook { return &fakeBook{sigs: map[string][]byte{}} }

func (b *fakeBook) RestingMakerSig(makerPubkey, offerID string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sigs[makerPubkey+"|"+offerID]
	return s, ok
}

// put records an offer as resting, mirroring the store.Submit the api layer runs
// AFTER a successful ValidateOffer.
func (b *fakeBook) put(o *seqobv1.Offer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sigs[o.GetMakerPubkey()+"|"+o.GetOfferId()] = o.GetMakerSig()
}

// TestReplayDoesNotExhaustBudget is the ITEM A regression: an attacker who
// harvests a victim maker's GENUINE signed offer from the public book and replays
// it on the submit path must NOT consume the victim's per-maker_pubkey budget
// (each replay is a byte-identical no-op), while N DISTINCT new offers do.
func TestReplayDoesNotExhaustBudget(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 3
	c.MaxOffersPerMinPerIP = 0
	v := New(c, nil)
	book := newFakeBook()
	v.SetBook(book)
	victim := key(t)
	ctx := context.Background()

	// The victim posts ONE genuine offer: it consumes one budget unit and rests.
	rest := signed(t, victim, func(o *seqobv1.Offer) { o.OfferId = "rest1" })
	if err := v.ValidateOffer(ctx, rest, ""); err != nil {
		t.Fatalf("first genuine offer should pass: %v", err)
	}
	book.put(rest) // mirror the store.Submit the api layer performs on success.

	// Replaying that genuine offer many times is recognized as a no-op: ErrReplay,
	// and crucially it charges NO budget. (Way more replays than the budget of 3.)
	for i := 0; i < 50; i++ {
		replay := proto.Clone(rest).(*seqobv1.Offer)
		if err := v.ValidateOffer(ctx, replay, ""); !errors.Is(err, ErrReplay) {
			t.Fatalf("replay %d: want ErrReplay, got %v", i, err)
		}
	}

	// The victim is NOT locked out: it can still post 2 more DISTINCT new offers
	// (budget 3, one already spent on rest1).
	for i := 0; i < 2; i++ {
		n := signed(t, victim, func(o *seqobv1.Offer) { o.OfferId = fmt.Sprintf("new%d", i) })
		if err := v.ValidateOffer(ctx, n, ""); err != nil {
			t.Fatalf("distinct new offer %d should pass: %v", i, err)
		}
		book.put(n)
	}

	// N distinct new offers DO exhaust the budget: the 4th distinct offer_id is
	// rate-limited (budget was spent by genuine new offers, never by replays).
	overflow := signed(t, victim, func(o *seqobv1.Offer) { o.OfferId = "new-overflow" })
	if err := v.ValidateOffer(ctx, overflow, ""); err == nil || errors.Is(err, ErrReplay) {
		t.Fatalf("4th distinct offer must be rate-limited, got %v", err)
	}
}

// TestEditConsumesBudget proves a genuine offer_edit (same offer_id, CHANGED and
// re-signed terms) is NOT treated as a replay: it has a different maker_sig, so it
// falls through and consumes budget (and would be stored by the api layer).
func TestEditConsumesBudget(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 2
	c.MaxOffersPerMinPerIP = 0
	v := New(c, nil)
	book := newFakeBook()
	v.SetBook(book)
	maker := key(t)
	ctx := context.Background()

	o := signed(t, maker, func(o *seqobv1.Offer) { o.OfferId = "edit-me" })
	if err := v.ValidateOffer(ctx, o, ""); err != nil {
		t.Fatalf("initial offer should pass: %v", err)
	}
	book.put(o)

	// A genuine edit re-signs changed terms under the SAME offer_id: different
	// maker_sig => not a replay => budget is charged.
	edit := signed(t, maker, func(e *seqobv1.Offer) {
		e.OfferId = "edit-me"
		e.WantAmount = 90 // changed price -> new signature
	})
	if err := v.ValidateOffer(ctx, edit, ""); err != nil {
		t.Fatalf("genuine edit should pass and consume budget: %v", err)
	}
	book.put(edit)

	// Budget (2) is now spent by the original + the edit; the next NEW offer is
	// rejected, confirming the edit really did consume a unit (not a free replay).
	next := signed(t, maker, func(o *seqobv1.Offer) { o.OfferId = "another" })
	if err := v.ValidateOffer(ctx, next, ""); err == nil {
		t.Fatalf("expected rate-limit: edit must have consumed budget")
	}
}

func TestMinAnchorDepthNotFloored(t *testing.T) {
	// Policy override: 0-conf tolerant. min_anchor_depth=0 must be accepted.
	v := New(cfg(), nil)
	o := signed(t, key(t), func(o *seqobv1.Offer) { o.MinAnchorDepth = 0 })
	if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
		t.Fatalf("min_anchor_depth=0 must be accepted, got %v", err)
	}
}

type failProbe struct{}

func (failProbe) CheckOffer(context.Context, *seqobv1.Offer) (uint64, error) {
	return 0, errors.New("no funds")
}

func TestLivenessProbeWired(t *testing.T) {
	v := New(cfg(), failProbe{})
	o := signed(t, key(t), nil)
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected liveness-probe rejection")
	}
}

// TestConfidentialOfferRules locks in the blinded-book validator invariants: a
// confidential offer must publish a valid maker_blinding_pub and a blinded
// (blech32) receive address, must use the same-chain rail, and must not touch BTC.
func TestConfidentialOfferRules(t *testing.T) {
	k := key(t)
	blindingPub := hex.EncodeToString(k.PubKey().SerializeCompressed())

	// Accepted: confidential + blinding pub + blech32 (tsqb1) receive address.
	good := signed(t, k, func(o *seqobv1.Offer) {
		o.Confidential = true
		o.GetSameChain().MakerRecvAddress = "tsqb1qexampleblindedaddressxxxxxxxxxxxxxxxxxxxxxxxxx"
		o.GetSameChain().MakerBlindingPub = blindingPub
	})
	if err := New(cfg(), nil).ValidateOffer(context.Background(), good, ""); err != nil {
		t.Fatalf("valid confidential offer rejected: %v", err)
	}

	// Rejected: confidential but transparent (tb1) receive address.
	badAddr := signed(t, k, func(o *seqobv1.Offer) {
		o.Confidential = true
		o.GetSameChain().MakerRecvAddress = "tb1qexampletransparentaddressxxxxxxxxxxxxxxxxxxx"
		o.GetSameChain().MakerBlindingPub = blindingPub
	})
	if err := New(cfg(), nil).ValidateOffer(context.Background(), badAddr, ""); err == nil {
		t.Fatal("confidential offer with a transparent receive address must be rejected")
	}

	// Rejected: confidential but no maker_blinding_pub (only one leg could blind).
	noBlind := signed(t, k, func(o *seqobv1.Offer) {
		o.Confidential = true
		o.GetSameChain().MakerRecvAddress = "tsqb1qexampleblindedaddressxxxxxxxxxxxxxxxxxxxxxxxxx"
	})
	if err := New(cfg(), nil).ValidateOffer(context.Background(), noBlind, ""); err == nil {
		t.Fatal("confidential offer without maker_blinding_pub must be rejected")
	}

	// A transparent offer (confidential unset) with a plain address stays accepted:
	// the unblinded book is untouched.
	plain := signed(t, k, nil)
	if err := New(cfg(), nil).ValidateOffer(context.Background(), plain, ""); err != nil {
		t.Fatalf("transparent offer must still validate: %v", err)
	}
}

// Readmit (a kept covenant order read back at start) runs every check on the
// offer and the probe, but no rate limit: a relay lists again what it held.
func TestReadmitChecksTheOfferButMetersNothing(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 1
	v := New(c, nil)
	k := key(t)
	for i := 0; i < 3; i++ {
		o := signed(t, k, func(o *seqobv1.Offer) { o.OfferId = fmt.Sprintf("kept%d", i) })
		if _, err := v.Readmit(context.Background(), o); err != nil {
			t.Fatalf("readmit %d: %v", i, err)
		}
	}
	if _, err := v.Admit(context.Background(), signed(t, k, nil), ""); err != nil {
		t.Fatalf("a post after the readmits: %v", err)
	}
	if _, err := v.Admit(context.Background(), signed(t, k, func(o *seqobv1.Offer) { o.OfferId = "next" }), ""); err == nil {
		t.Fatal("the rate limit no longer applies to posts")
	}
	forged := signed(t, k, nil)
	forged.OfferAmount++
	if _, err := v.Readmit(context.Background(), forged); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a kept line altered after signing: %v", err)
	}
	if _, err := New(cfg(), failProbe{}).Readmit(context.Background(), signed(t, k, nil)); err == nil {
		t.Fatal("readmit skipped the probe")
	}
}
