package matcher

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

const (
	assetA = "aaaa" // base
	assetB = "bbbb" // quote
)

func newKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sell = maker sells `base` of A wanting `quote` of B (ask = quote/base).
func sell(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId: id, SchemaVersion: 1,
		Pair:       &seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB},
		TradeDir:   seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount: base, OfferAmount: base, OfferAsset: assetA,
		WantAmount: quote, WantAsset: assetB,
		AllowPartial: partial, CreatedAtUnix: 1750000000, ExpiresAtUnix: 1799999999,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// buy = maker buys `base` of A giving `quote` of B (bid = quote/base).
func buy(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId: id, SchemaVersion: 1,
		Pair:       &seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB},
		TradeDir:   seqobv1.TradeDir_TRADE_DIR_BUY,
		BaseAmount: base, WantAmount: base, WantAsset: assetA,
		OfferAmount: quote, OfferAsset: assetB,
		AllowPartial: partial, CreatedAtUnix: 1750000000, ExpiresAtUnix: 1799999999,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// covSell is a SELL like sell(), but settled by an on-chain covenant (the taker builds the FILL
// spend). The matcher treats it the same as any order — pure plan+emit, no trade-truth mutation; the
// covenant terms are carried on the Match so the taker/settler and the watcher can act. The display
// price stays want/offer (quote/base); the covenant rate mirrors it. min_lot 1 so any fill clears the floor.
func covSell(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := sell(t, k, id, base, quote, partial)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "cov" + id, CovenantVout: 0,
		AssetA: assetA, AssetB: assetB, RateNum: quote, RateDen: base,
		MinLot: 1, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// covBuy is a BUY like buy(), but settled by an on-chain covenant (its covenant
// locks the quote asset B and buys base A). Used to build BOTH-covenant crosses so
// the owner-guard (decision 3) lets them auto-cross (a covenant<->covenant cross is
// settled by the always-online settler). min_lot 1 so any fill clears the floor.
func covBuy(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := buy(t, k, id, base, quote, partial)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "covbuy" + id, CovenantVout: 0,
		AssetA: assetB, AssetB: assetA, RateNum: base, RateDen: quote,
		MinLot: 1, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

func mustSubmit(t *testing.T, s *offerstore.Store, o *seqobv1.Offer) {
	t.Helper()
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit %s: %v", o.GetOfferId(), err)
	}
}

// An INTERACTIVE <-> INTERACTIVE cross has NO auto-settle owner (it settles only via
// a TAKER-INITIATED StartLift + the maker's SettleAck), so the owner-guard (decision
// 3: every match settles) must NOT emit it — an auto-cross would advertise a
// From.matched whose session is never registered and then drop it, leaving the book
// crossed bid>=ask. No match, no mutation, no trade.
func TestInteractiveCrossIsGuardedNoOwner(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting SELL: 100 A for 45 B (ask 0.45).
	mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true))
	// Incoming BUY: 100 A for 50 B (bid 0.50 >= 0.45) -> would cross, but neither side
	// is a covenant, so there is no auto-settle owner: guarded (never auto-crossed).
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 0 {
		t.Fatalf("interactive<->interactive has no auto-settle owner; want 0 matches, got %+v", got)
	}
	// Both orders remain untouched (they rest until a taker explicitly lifts).
	if activeOf(s, "s1") != 100 {
		t.Fatalf("resting must NOT be decremented, got %d", activeOf(s, "s1"))
	}
	if e, ok := s.Get(offerstore.Key{MakerPubkey: in.GetMakerPubkey(), OfferID: "b1"}); !ok || e.ActiveAmount != 100 {
		t.Fatalf("incoming must NOT be decremented")
	}
	if tr := s.TradesFor(&seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB}, 0); len(tr) != 0 {
		t.Fatalf("guarded cross must record no trade, got %d", len(tr))
	}
	_ = mk
}

// A COVENANT resting order crossed by an INTERACTIVE incoming DOES have an owner:
// the incoming counterparty settles the permissionless covenant FILL itself from
// the terms carried on the Match. So the owner-guard emits it (planned, no mutation).
func TestCovenantVsInteractiveHasOwner(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true)) // resting covenant ask 0.45
	in := buy(t, tk, "b1", 100, 50, true)                 // interactive incoming
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 100 {
		t.Fatalf("covenant<->interactive has an owner; want 1 planned match of 100, got %+v", got)
	}
	if got[0].RestingCovenant == nil {
		t.Fatal("covenant terms must be carried so the taker can settle the FILL")
	}
	_ = mk
}

// A COVENANT that is the INCOMING order crossing a resting INTERACTIVE order has NO owner
// as wired: the Match would set IncomingCovenant, but matchedProto only delivers
// RestingCovenant, so the covenant terms reach no one and the resting interactive maker
// would get a phantom From.matched. Only a RESTING covenant is an owner — guard this off.
func TestIncomingCovenantVsInteractiveRestingNoOwner(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true)) // resting INTERACTIVE ask 0.45
	in := covBuy(t, tk, "b1", 100, 50, true)           // incoming COVENANT bid 0.50 -> would cross
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 0 {
		t.Fatalf("incoming covenant vs resting interactive has no owner as wired; want 0 matches, got %+v", got)
	}
	if activeOf(s, "s1") != 100 {
		t.Fatalf("resting interactive must NOT be decremented, got %d", activeOf(s, "s1"))
	}
	_ = mk
}

// A COVENANT cross is PLANNED (the Match carries the fill + covenant terms) but the matcher commits NO
// trade-truth. A cross becomes a trade only once it SETTLES on-chain, and it is recorded then — reorg-
// safely — by the chain watcher, with the fill's record read from the filling transaction
// (RecordCovenantFill). Recording at match would double-count with the watcher and surface trades
// that never settled.
func TestCovenantCrossPlansNoMutation(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true)) // resting covenant ask 0.45
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 100 {
		t.Fatalf("want 1 planned match of 100, got %+v", got)
	}
	if got[0].RestingCovenant == nil {
		t.Fatal("covenant terms must be carried on the match (the taker/settler + watcher need them)")
	}
	// NO trade recorded and NO decrement at match — trade-truth is committed on settlement, not here.
	if tr := s.TradesFor(&seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB}, 0); len(tr) != 0 {
		t.Fatalf("covenant cross must record no trade at match, got %d", len(tr))
	}
	if activeOf(s, "s1") != 100 {
		t.Fatalf("resting covenant must NOT be decremented at match, got %d", activeOf(s, "s1"))
	}
	if e, ok := s.Get(offerstore.Key{MakerPubkey: in.GetMakerPubkey(), OfferID: "b1"}); !ok || e.ActiveAmount != 100 {
		t.Fatalf("incoming must NOT be decremented at match")
	}
	_ = mk
}

func TestNonCrossingLeavesResting(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting SELL asking 0.60 (100 A for 60 B).
	mustSubmit(t, s, sell(t, mk, "s1", 100, 60, true))
	// Incoming BUY bidding only 0.50 -> does NOT cross.
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)

	if got := m.Cross(in); len(got) != 0 {
		t.Fatalf("expected no match, got %d", len(got))
	}
	// Both remain resting at full size.
	e, ok := s.Get(offerstore.Key{MakerPubkey: in.GetMakerPubkey(), OfferID: "b1"})
	if !ok || e.ActiveAmount != 100 {
		t.Fatalf("incoming should rest at 100, got ok=%v amt=%v", ok, e)
	}
}

// A partial COVENANT cross plans the partial fill (FillBase=30) but still mutates nothing at match; the
// watcher re-rests the on-chain remainder once the FILL confirms. (The size stays full until then.)
func TestCovenantPartialCrossPlansNoMutation(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL of 100 A (ask 0.45), partial allowed.
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true))
	// Incoming BUY for only 30 A at bid 0.50.
	in := buy(t, tk, "b1", 30, 15, true)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 30 {
		t.Fatalf("want 1 planned match of 30, got %+v", got)
	}
	// No mutation at match: the resting covenant stays at full size until the FILL settles on-chain.
	if rem := activeOf(s, "s1"); rem != 100 {
		t.Fatalf("resting covenant must NOT be decremented at match, got %d", rem)
	}
	_ = mk
}

func TestPriceTimePriority(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	a, b, c, tk := newKey(t), newKey(t), newKey(t), newKey(t)
	// Three resting COVENANT SELLs at asks 0.50, 0.45, 0.45 (b older than c). Covenant
	// so the owner-guard (decision 3) lets the cross auto-emit — the settler settles a
	// covenant<->covenant cross; the price-time-priority walk is rail-agnostic.
	s5 := covSell(t, a, "ask50", 100, 50, true)
	s45old := covSell(t, b, "ask45old", 100, 45, true)
	s45new := covSell(t, c, "ask45new", 100, 45, true)
	s45old.CreatedAtUnix = 1750000000
	s45new.CreatedAtUnix = 1750000500
	// Re-sign after touching created_at (it is inside the signed bytes).
	_ = offer.SignOffer(s5, a)
	_ = offer.SignOffer(s45old, b)
	_ = offer.SignOffer(s45new, c)
	mustSubmit(t, s, s5)
	mustSubmit(t, s, s45old)
	mustSubmit(t, s, s45new)

	// Incoming covenant BUY of 150 A bidding 0.50: fills best price first (both 0.45s),
	// oldest 0.45 before newer, then the 0.50.
	in := covBuy(t, tk, "big", 150, 75, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 2 {
		t.Fatalf("want 2 matches, got %d", len(got))
	}
	if got[0].RestingKey.OfferID != "ask45old" {
		t.Fatalf("first fill should be the oldest 0.45, got %s", got[0].RestingKey.OfferID)
	}
	if got[1].RestingKey.OfferID != "ask45new" || got[1].FillBase != 50 {
		t.Fatalf("second fill should be 50 of ask45new, got %s/%d", got[1].RestingKey.OfferID, got[1].FillBase)
	}
	if activeOf(s, "ask50") != 100 {
		t.Fatal("the 0.50 ask should be untouched")
	}
}

func TestAllOrNothingIncomingNoPartial(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Only 40 A resting (covenant, so the cross has an owner); incoming covenant BUY
	// wants 100 all-or-nothing and the book cannot fill it -> execute nothing.
	mustSubmit(t, s, covSell(t, mk, "s1", 40, 18, true))
	in := covBuy(t, tk, "b1", 100, 50, false) // allow_partial = false
	mustSubmit(t, s, in)
	if got := m.Cross(in); len(got) != 0 {
		t.Fatalf("all-or-nothing incoming must not partially fill, got %d", len(got))
	}
	if activeOf(s, "s1") != 40 || activeOf(s, "b1") != 100 {
		t.Fatal("nothing should have been filled")
	}
}

func TestCovenantMatchCarriesTerms(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL: 90 A for 30 B (rate 1/3), min_lot 5.
	o := sell(t, mk, "cov1", 90, 30, true)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "deadbeef", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 1, RateDen: 3,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	if err := offer.SignOffer(o, mk); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, o)

	in := buy(t, tk, "b1", 90, 40, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if got[0].RestingCovenant == nil {
		t.Fatal("covenant terms not carried on the match")
	}
	if got[0].RestingLocked != 90 {
		t.Fatalf("covenant locked = %d, want 90", got[0].RestingLocked)
	}
	// Quote owed uses the on-chain rate: ceil(90 * 1/3) = 30.
	if got[0].FillQuote != 30 {
		t.Fatalf("fill quote = %d, want 30", got[0].FillQuote)
	}
}

func TestBothCovenantMatch(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL: 30 A wanting 90 B (sells A at 3 B/A).
	rest := sell(t, mk, "covA", 30, 90, true)
	rest.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "aa", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 3, RateDen: 1,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(rest, mk)
	mustSubmit(t, s, rest)

	// Incoming covenant BUY: buys 30 A giving 90 B (its own covenant sells B).
	in := buy(t, tk, "covB", 30, 90, true)
	in.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "bb", CovenantVout: 1,
		AssetA: "00bb", AssetB: "00aa", RateNum: 1, RateDen: 3,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(in, tk)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if !got[0].BothCovenant() {
		t.Fatal("both-covenant case not detected (settler path)")
	}
	if got[0].RestingCovenant == nil || got[0].IncomingCovenant == nil {
		t.Fatal("both covenant terms must be carried for the settler")
	}
	if got[0].IncomingCovenant.GetCovenantTxid() != "bb" || got[0].IncomingCovenant.GetCovenantVout() != 1 {
		t.Fatal("incoming covenant outpoint not carried")
	}
}

func TestCovenantMinLotRemainderTrim(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Covenant of 90 A, min_lot 5. Incoming would take 88, leaving a 2-atom
	// remainder < min_lot; the matcher trims the fill to 85 so the remainder is 5.
	o := sell(t, mk, "cov1", 90, 30, true)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		AssetA: "00aa", AssetB: "00bb", RateNum: 1, RateDen: 3, MinLot: 5, MakerProgVer: 1,
	}}
	_ = offer.SignOffer(o, mk)
	mustSubmit(t, s, o)
	in := buy(t, tk, "b1", 88, 40, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 85 {
		t.Fatalf("want a trimmed fill of 85, got %+v", got)
	}
	// The trim is a PLANNING guarantee (the FILL leaf enforces the >= min_lot remainder). The matcher
	// does not decrement at match; the on-chain remainder (5) is re-rested by the watcher on settlement.
}

func TestCrossRailBridgeMatch(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL of X (asset A) wanting Y (asset B), 30 X for 90 B.
	rest := sell(t, mk, "covA", 30, 90, true)
	rest.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "aa", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 3, RateDen: 1,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(rest, mk)
	mustSubmit(t, s, rest)

	// Incoming LIGHTNING BUY of 30 X giving 100 B (its owner chose the LN rail).
	in := buy(t, tk, "lnB", 30, 100, true)
	in.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
		LnDirection: 1, Max_0ConfAmount: 50,
	}}
	_ = offer.SignOffer(in, tk)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if !got[0].CrossRail() {
		t.Fatal("covenant vs Lightning cross not classified CrossRail (bridge path)")
	}
	if got[0].BothCovenant() {
		t.Fatal("a cross-rail match must NOT be BothCovenant")
	}
	terms, resting := got[0].BridgeCovenant()
	if terms == nil || !resting || terms.GetCovenantTxid() != "aa" {
		t.Fatalf("bridge covenant side wrong: terms=%v resting=%v", terms, resting)
	}
	ln := got[0].BridgeLightning()
	if ln == nil || ln.GetMax_0ConfAmount() != 50 {
		t.Fatalf("bridge Lightning side not carried: %v", ln)
	}
}

func TestSameRailNotCrossRail(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Two covenant orders (both-covenant): a same-rail cross the settler owns, never a
	// bridge job. (An interactive<->interactive cross would now be owner-guarded, so we
	// exercise the CrossRail-negative on a combination that still auto-crosses.)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true))
	in := covBuy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if got[0].CrossRail() {
		t.Fatal("a covenant/covenant cross must not be CrossRail")
	}
	if !got[0].BothCovenant() {
		t.Fatal("a covenant/covenant cross must be BothCovenant")
	}
}

// confidential re-signs o in the blinded-book namespace (confidential=true). The
// namespace is part of the canonical signed bytes, so it must be set before signing.
func confidential(t *testing.T, k *btcec.PrivateKey, o *seqobv1.Offer) *seqobv1.Offer {
	t.Helper()
	o.Confidential = true
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestConfidentialNeverCrossesTransparent asserts the separate blinded book: a
// confidential offer must NEVER cross a transparent offer on the same pair, even
// when their prices cross. Both legs of a confidential swap must blind on-chain, so
// a confidential order can only fill against another confidential order.
func TestConfidentialNeverCrossesTransparent(t *testing.T) {
	// Case 1: a confidential incoming must not cross a transparent resting order.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true)) // transparent SELL
		in := confidential(t, tk, buy(t, tk, "b1", 100, 50, true))
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 0 {
			t.Fatalf("confidential incoming must not cross a transparent resting order, got %d matches", len(got))
		}
		if activeOf(s, "s1") != 100 {
			t.Fatal("transparent resting SELL must be untouched by a confidential incoming")
		}
	}

	// Case 2: a transparent incoming must not cross a resting confidential order.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, confidential(t, mk, sell(t, mk, "s2", 100, 45, true)))
		in := buy(t, tk, "b2", 100, 50, true)
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 0 {
			t.Fatalf("transparent incoming must not cross a confidential resting order, got %d matches", len(got))
		}
	}

	// Case 3: a confidential incoming does NOT AUTO-cross a confidential resting order.
	// A confidential offer must settle on the interactive same-chain co-sign rail (the
	// covenant rail introspects explicit amounts and is CT-incompatible), so a
	// confidential<->confidential cross has no auto-settle owner and is owner-guarded
	// like every interactive<->interactive cross. Confidential orders settle via a
	// TAKER-INITIATED lift, not the continuous auto-cross.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, confidential(t, mk, sell(t, mk, "s3", 100, 45, true)))
		in := confidential(t, tk, buy(t, tk, "b3", 100, 50, true))
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 0 {
			t.Fatalf("confidential (interactive) auto-cross is owner-guarded; want 0 matches, got %d", len(got))
		}
	}
}

// tif sets an incoming order's time-in-force and re-signs it (the option is part of
// the signed canonical bytes).
func tif(t *testing.T, k *btcec.PrivateKey, o *seqobv1.Offer, v seqobv1.TimeInForce) *seqobv1.Offer {
	t.Helper()
	o.TimeInForce = v
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// P2.9: IOC fills what it can now and cancels the remainder (never rests). A covenant
// BUY of 100 against 40 resting covenant liquidity fills 40, remainder 60 -> the
// matches route and the disposition tells the caller to retire the remainder.
func TestCrossOrderIOCFillsThenCancelsRemainder(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 40, 18, true))
	in := tif(t, tk, covBuy(t, tk, "b1", 100, 50, true), seqobv1.TimeInForce_TIME_IN_FORCE_IOC)
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 1 || res.Matches[0].FillBase != 40 {
		t.Fatalf("IOC want 1 match of 40, got %+v", res.Matches)
	}
	if res.Disposition != DispCancelRemainder {
		t.Fatalf("IOC disposition = %v, want DispCancelRemainder", res.Disposition)
	}
	if res.Filled != 40 || res.Remainder != 60 {
		t.Fatalf("IOC filled=%d remainder=%d, want 40/60", res.Filled, res.Remainder)
	}
}

// P2.9: FOK is all-or-nothing. If the book cannot fill the WHOLE incoming at once,
// execute NOTHING and reject (unlike a non-partial GTC, which rests). 40 resting vs a
// 100 FOK -> no matches, DispReject.
func TestCrossOrderFOKUnfillableRejects(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 40, 18, true))
	in := tif(t, tk, covBuy(t, tk, "b1", 100, 50, true), seqobv1.TimeInForce_TIME_IN_FORCE_FOK)
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 0 {
		t.Fatalf("FOK unfillable must emit no match, got %+v", res.Matches)
	}
	if res.Disposition != DispReject {
		t.Fatalf("FOK unfillable disposition = %v, want DispReject", res.Disposition)
	}
}

// P2.9: a FOK that CAN fill in full executes fully and rests nothing (remainder 0).
func TestCrossOrderFOKFillableExecutes(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true))
	in := tif(t, tk, covBuy(t, tk, "b1", 100, 50, true), seqobv1.TimeInForce_TIME_IN_FORCE_FOK)
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 1 || res.Matches[0].FillBase != 100 {
		t.Fatalf("FOK fillable want 1 match of 100, got %+v", res.Matches)
	}
	if res.Disposition != DispRest || res.Remainder != 0 {
		t.Fatalf("FOK fillable disposition=%v remainder=%d, want DispRest/0", res.Disposition, res.Remainder)
	}
}

// P2.9: POST_ONLY rejects if it would take (cross) anything (rest-only).
func TestCrossOrderPostOnlyRejectsIfWouldTake(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 45, true)) // ask 0.45
	// A post-only BUY bidding 0.50 crosses the 0.45 ask -> would take -> reject.
	in := tif(t, tk, covBuy(t, tk, "b1", 100, 50, true), seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY)
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 0 || res.Disposition != DispReject {
		t.Fatalf("post-only that would take must reject with no matches, got %+v disp=%v", res.Matches, res.Disposition)
	}
}

// P2.9: POST_ONLY that would NOT take rests (adds liquidity).
func TestCrossOrderPostOnlyRestsIfNoTake(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 100, 60, true)) // ask 0.60
	// A post-only BUY bidding only 0.50 does NOT cross the 0.60 ask -> rests.
	in := tif(t, tk, covBuy(t, tk, "b1", 100, 50, true), seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY)
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 0 || res.Disposition != DispRest {
		t.Fatalf("post-only with no cross must rest, got %+v disp=%v", res.Matches, res.Disposition)
	}
}

// P2.9: GTC (default) is unchanged — cross what you can, rest the remainder. A
// non-allow_partial GTC that cannot fully fill executes nothing and rests.
func TestCrossOrderGTCRestsRemainder(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	mustSubmit(t, s, covSell(t, mk, "s1", 40, 18, true))
	in := covBuy(t, tk, "b1", 100, 50, true) // GTC (unspecified), allow_partial
	mustSubmit(t, s, in)
	res := m.CrossOrder(in)
	if len(res.Matches) != 1 || res.Matches[0].FillBase != 40 || res.Disposition != DispRest {
		t.Fatalf("GTC want 1 match of 40 then rest, got %+v disp=%v", res.Matches, res.Disposition)
	}
	if res.Remainder != 60 {
		t.Fatalf("GTC remainder=%d, want 60 (rests)", res.Remainder)
	}
}

// activeOf finds a resting order's active amount by scanning the pair.
func activeOf(s *offerstore.Store, offerID string) uint64 {
	for _, e := range s.SnapshotPairEntries(&seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB}, false) {
		if e.Offer.GetOfferId() == offerID {
			return e.ActiveAmount
		}
	}
	return 0
}

var _ = time.Now
