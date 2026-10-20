package main

// norm.go — the family-blind NORMALIZED view of a resting offer.
//
// Every settlement family (same-chain co-sign, covenant CLOB, cross-chain HTLC,
// submarine, pure-LN, sub-asset buy/sell) advertises the same signed price
// fields (offer_amount/want_amount over base_amount), so one normalized order
// shape covers all of them — including future families, which normalize the same
// way as long as they keep the Offer price contract. The crosser detects a cross
// purely on this normalized view; only EXECUTION dispatches per family.

import (
	"fmt"
	"math/big"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// Family is the settlement family of a resting offer — the rail its TAKER side
// must be driven on.
type Family int

const (
	FamUnknown   Family = iota
	FamSameChain        // interactive co-signed same-chain swap (SameChainTerms)
	FamCovenant         // funded passive CLOB order (CovenantTerms)
	FamCross            // on-chain BTC<->asset HTLC pair (CrossChainTerms)
	FamSubmarine        // asset on-chain <-> BTC over Lightning (ln_direction 0/1)
	FamPureLN           // BOTH legs over Lightning (ln_direction 2/3)
	FamSubAsset         // asset over Lightning <-> BTC (or quote asset) on-chain (ln_direction 4/5)
)

func (f Family) String() string {
	switch f {
	case FamSameChain:
		return "samechain"
	case FamCovenant:
		return "covenant"
	case FamCross:
		return "cross"
	case FamSubmarine:
		return "submarine"
	case FamPureLN:
		return "pureln"
	case FamSubAsset:
		return "subasset"
	default:
		return "unknown"
	}
}

// Side is the offer's side of the canonical book from the CROSSER's seat: an Ask
// is an offer the crosser can BUY base from (the maker sells base); a Bid is one
// the crosser can SELL base into (the maker buys base).
type Side int

const (
	SideBid Side = iota
	SideAsk
)

func (s Side) String() string {
	if s == SideAsk {
		return "ask"
	}
	return "bid"
}

// NormOrder is one resting offer normalized onto the canonical pair.
type NormOrder struct {
	Relay  string // relay base URL that served (and can courier a lift of) this offer
	Offer  *seqobv1.Offer
	Family Family
	Side   Side

	Base, Quote string // canonical pair (display asset id hex, or the "BTC" sentinel)
	// Flipped marks an offer that was posted on the REVERSED pair orientation
	// (its own base is the canonical quote). It still joins price detection, but
	// execution is not wired for flipped offers (the taker CLIs size -amount in
	// the offer's OWN base units) — executability() rejects them with a reason.
	Flipped bool

	// BaseSize is the offer's full size in canonical-base atoms. QuoteNum/BaseDen
	// is the price ratio: quote atoms per BaseDen base atoms (integers straight
	// from the signed offer — never a float).
	BaseSize uint64
	QuoteNum uint64 // quote-side atoms for the whole offer
	BaseDen  uint64 // base-side atoms for the whole offer (== BaseSize)

	AllowPartial bool
	MinFill      uint64 // canonical-base atoms
	// Sliceable reports whether the crosser's TAKER driver for this family can
	// take a slice (< the whole offer). The submarine taker CLIs (xsublift /
	// xsubbuy) have no partial flag, so submarine offers are whole-take only even
	// when the offer itself says allow_partial.
	Sliceable bool

	MinLot uint64 // covenant only: floor on filled AND on any non-zero remainder

	ExpiresAt uint64 // unix
	CreatedAt uint64 // unix; the time half of price-time priority
}

// ID is the global dedupe key: the same signed offer served by several relays is
// one order.
func (n *NormOrder) ID() string {
	return n.Offer.GetMakerPubkey() + ":" + n.Offer.GetOfferId()
}

// CostFor is the quote the crosser PAYS to take n base atoms from an ASK,
// rounded UP (in the maker's favour — exactly the drivers' ProportionalBtc ceil,
// so the gate prices what execution will actually pay).
func (n *NormOrder) CostFor(take uint64) uint64 {
	return ceilMulDiv(take, n.QuoteNum, n.BaseDen)
}

// RevenueFor is the quote the crosser RECEIVES selling n base atoms into a BID,
// rounded DOWN (maker's favour — the drivers' ProportionalBtcFloor).
func (n *NormOrder) RevenueFor(take uint64) uint64 {
	return floorMulDiv(take, n.QuoteNum, n.BaseDen)
}

// PriceGE reports a.price >= b.price without floats: cross-multiplied in big.Int
// so no overflow at any atom scale.
func (a *NormOrder) PriceGE(b *NormOrder) bool {
	// a.QuoteNum/a.BaseDen >= b.QuoteNum/b.BaseDen
	lhs := new(big.Int).Mul(new(big.Int).SetUint64(a.QuoteNum), new(big.Int).SetUint64(b.BaseDen))
	rhs := new(big.Int).Mul(new(big.Int).SetUint64(b.QuoteNum), new(big.Int).SetUint64(a.BaseDen))
	return lhs.Cmp(rhs) >= 0
}

func ceilMulDiv(a, num, den uint64) uint64 {
	if den == 0 {
		return 0
	}
	p := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(num))
	d := new(big.Int).SetUint64(den)
	q, r := new(big.Int).QuoRem(p, d, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsUint64() {
		return ^uint64(0)
	}
	return q.Uint64()
}

func floorMulDiv(a, num, den uint64) uint64 {
	if den == 0 {
		return 0
	}
	p := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(num))
	q := new(big.Int).Quo(p, new(big.Int).SetUint64(den))
	if !q.IsUint64() {
		return ^uint64(0)
	}
	return q.Uint64()
}

// classifyFamily maps the settlement oneof (+ ln_direction) to a Family.
func classifyFamily(o *seqobv1.Offer) Family {
	switch {
	case o.GetSameChain() != nil:
		return FamSameChain
	case o.GetCovenant() != nil:
		return FamCovenant
	case o.GetCrossChain() != nil:
		return FamCross
	case o.GetLightning() != nil:
		ld := o.GetLightning().GetLnDirection()
		switch {
		case offer.IsPureLN(ld):
			return FamPureLN
		case offer.IsSubAsset(ld):
			return FamSubAsset
		default: // LnAssetForBTC / LnBTCForAsset
			return FamSubmarine
		}
	default:
		// A Simplicity covenant order (SimplicityCovenantTerms) lands here: the
		// crosser's executor fills tapscript covenants only, so it skips one.
		return FamUnknown
	}
}

// makerSells reports whether the offer's MAKER gives up the canonical base
// (i.e. the offer is an ASK), derived from the price fields themselves —
// offer_asset == base — which is orientation-independent and part of the signed
// bytes. trade_dir is then cross-checked where it is set.
func makerSells(o *seqobv1.Offer, base string) (bool, error) {
	switch {
	case o.GetOfferAsset() == base:
		return true, nil
	case o.GetWantAsset() == base:
		return false, nil
	default:
		return false, fmt.Errorf("offer trades %s/%s, not the requested base %s",
			o.GetOfferAsset(), o.GetWantAsset(), base)
	}
}

// Normalize maps one signed offer served by `relay` onto the canonical pair
// (base, quote). It returns nil with a reason when the offer does not belong to
// the pair or is internally inconsistent (a defensive skip, never a guess).
func Normalize(o *seqobv1.Offer, relay, base, quote string) (*NormOrder, string) {
	oa, wa := o.GetOfferAsset(), o.GetWantAsset()
	pairOK := (oa == base && wa == quote) || (oa == quote && wa == base)
	if !pairOK {
		return nil, "not this pair"
	}
	if o.GetOfferAmount() == 0 || o.GetWantAmount() == 0 {
		return nil, "zero-sized offer"
	}
	fam := classifyFamily(o)
	if fam == FamUnknown {
		return nil, "unknown settlement family"
	}

	isAsk, err := makerSells(o, base)
	if err != nil {
		return nil, err.Error()
	}
	// Cross-check the signed trade_dir when set: SELL = the maker gives its own
	// pair's BASE. On a flipped orientation the offer's own base is our quote, so
	// the expectation inverts.
	ownBase := o.GetPair().GetBaseAsset()
	flipped := ownBase == quote
	if td := o.GetTradeDir(); td != seqobv1.TradeDir_TRADE_DIR_UNSPECIFIED {
		makerSellsOwnBase := td == seqobv1.TradeDir_TRADE_DIR_SELL
		wantAsk := makerSellsOwnBase != flipped
		if wantAsk != isAsk {
			return nil, "trade_dir contradicts offer/want assets"
		}
	}

	var baseSize, quoteNum uint64
	if isAsk {
		baseSize, quoteNum = o.GetOfferAmount(), o.GetWantAmount()
	} else {
		baseSize, quoteNum = o.GetWantAmount(), o.GetOfferAmount()
	}
	// base_amount is what the taker CLIs slice against (-amount / take_amount);
	// it must agree with the base-side price field or a partial would be priced
	// off a different whole than we planned.
	if !flipped && o.GetBaseAmount() != baseSize {
		return nil, fmt.Sprintf("base_amount %d != base-side amount %d", o.GetBaseAmount(), baseSize)
	}

	minFill := o.GetMinFill()
	if flipped && minFill > 0 {
		// min_fill is signed in the offer's OWN base = our quote; convert to
		// canonical-base atoms, rounding UP (conservative: never under-enforce).
		minFill = ceilMulDiv(minFill, baseSize, quoteNum)
	}

	n := &NormOrder{
		Relay:        relay,
		Offer:        o,
		Family:       fam,
		Base:         base,
		Quote:        quote,
		Flipped:      flipped,
		BaseSize:     baseSize,
		QuoteNum:     quoteNum,
		BaseDen:      baseSize,
		AllowPartial: o.GetAllowPartial(),
		MinFill:      minFill,
		ExpiresAt:    o.GetExpiresAtUnix(),
		CreatedAt:    o.GetCreatedAtUnix(),
	}
	if isAsk {
		n.Side = SideAsk
	} else {
		n.Side = SideBid
	}

	// Per-family taker slice capability: which drivers accept a partial take.
	//   cross      : xlift/xsell -amount
	//   subasset   : xsubas / xsubas-sell -amount
	//   pureln     : RunTakerPureLN TakeSeqMsat
	//   samechain  : seqob-cli lift -amount
	//   covenant   : partial fills native to the FILL leaf (min_lot rules)
	//   submarine  : NO — xsublift/xsubbuy have no partial flag (whole-take only)
	switch fam {
	case FamCross, FamSubAsset, FamPureLN, FamSameChain, FamCovenant:
		n.Sliceable = true
	default:
		n.Sliceable = false
	}
	if fam == FamCovenant {
		n.MinLot = o.GetCovenant().GetMinLot()
	}
	return n, ""
}
