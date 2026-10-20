// Package matcher is the SeqOB relay's continuous limit-order-book matching
// engine: per-pair, price-time-priority crossing on top of the offerstore.
//
// On every accepted offer_submit the relay runs Cross(incoming): it walks the
// resting OPPOSITE side of the incoming order's pair, best-price-first then
// oldest-first, and while the incoming crosses the top resting order (integer-
// ratio compare, the same price floor ValidateRequestAgainstOffer enforces) it
// computes a fill size (honoring allow_partial / min_fill, and — for a covenant
// order — the covenant's own min_lot floor on both the fill and the remainder),
// and emits a Match. Any incoming remainder rests.
//
// The matcher is PURE plan + emit: it commits NO trade-truth. It never decrements
// an order and never records a trade — a cross is only a trade once it SETTLES
// on-chain, and only that is recorded, reorg-safely, by the owner of settlement:
// a covenant fill by the chain watcher on confirmation (a delta off the order's
// live size), an interactive lift by the maker's SettleAck. Recording/decrementing
// at match time surfaced phantom trades (an interactive auto-cross never settles)
// and, once the watcher was enabled, double-counted covenant fills. So the matcher
// is I/O-free and returns []Match for the caller (the api layer, which owns the
// websocket connections) to route as From.matched, and nothing more. For a COVENANT
// resting order the maker is offline; the Match carries the covenant terms + this
// fill's recipe so the counterparty (taker) settles the permissionless FILL spend
// itself, with nobody online but the taker.
package matcher

import (
	"math/big"
	"sort"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// isSubAssetOffer reports whether an offer is a sub-asset swap (asset over LN <->
// BTC on-chain HTLC). Both directions rest on the <asset>/BTC pair on opposite
// sides with lined-up assets, so without a guard the matcher would auto-cross a
// sub-asset BUY (ln 4) against a sub-asset SELL (ln 5).
func isSubAssetOffer(o *seqobv1.Offer) bool {
	ln := o.GetLightning()
	return ln != nil && offer.IsSubAsset(ln.GetLnDirection())
}

// hasSettlementOwner reports whether an auto-cross between incoming and resting
// has a live settlement OWNER — the precondition (decision 3: EVERY MATCH SETTLES)
// for the continuous matcher to emit the cross. The auto-cross path (offer_submit
// -> Cross) is fire-and-forget: nothing online co-signs it, so it may emit a Match
// only for combinations an out-of-band owner settles WITHOUT the two makers being
// live on the socket:
//
//   - at least one side is a COVENANT (the funded, self-enforcing CLOB order).
//     A covenant fill is permissionless, so the cross is settleable by an owner
//     that needs neither maker online:
//   - covenant <-> covenant: the always-online seqob-settler polls the book,
//     pairs the two funded covenants, and broadcasts the joint FILL. LIVE.
//   - covenant <-> Lightning (CrossRail): the silent seqob-bridge fills the
//     covenant and settles the LN leg under one preimage. The bridge is the
//     DESIGNATED owner; its standalone run-loop is not built in this pass
//     (cmd/seqob-bridge 'run' is documented-only), so a CrossRail emit is a
//     match FOR the bridge to pick up, not a self-settling one.
//   - covenant <-> interactive: the counterparty settles the permissionless
//     covenant FILL itself from the terms carried on the Match.
//
// Every combination with NO covenant side has NO auto-settle owner and must NOT
// auto-cross (it would advertise a From.matched that is then dropped, leaving the
// book crossed bid>=ask):
//   - interactive <-> interactive: settles only via a TAKER-INITIATED lift
//     (StartLift + the maker's SettleAck). An auto-cross mints an unregistered
//     session that never settles (phantom). The engine-initiated lift owner
//     (orderbook-design §4 v2) is not built in this pass.
//   - pure-LN, submarine, cross-chain, and any interactive/Lightning or
//     interactive/cross-chain mix: no poller settles them from an auto-cross;
//     they too are taker-initiated only.
//
// This extends the sub-asset never-auto-cross guard to every ownerless
// combination, so no advisory match ever ships.
//
// A Simplicity covenant order (SimplicityCovenantTerms) on EITHER side has no
// auto-cross owner: the settler pairs two tapscript covenants, the bridge
// fills a tapscript one, and Matched carries tapscript terms. It is filled by
// a taker who reads it from the book (seqob-cli covfill) or by an operator's
// round, so it never auto-crosses; it rests, and the watcher records its fills.
func hasSettlementOwner(incoming, resting *seqobv1.Offer) bool {
	// The owner must key on the RESTING side being a covenant, because that is the
	// only covenant the Match delivers: matchedProto carries RestingCovenant, so an
	// out-of-band owner (the settler for a resting covenant vs another covenant; the
	// bridge for a resting covenant vs Lightning; the taker's own permissionless FILL
	// for a resting covenant vs interactive) only ever sees the RESTING covenant's
	// terms. A covenant that is the INCOMING order crossing a non-covenant resting
	// order has NO owner as wired (its terms reach no one) — guard it off rather than
	// ship a phantom From.matched to the resting maker. (Both-covenant is covered here
	// too, since the resting side is a covenant; the settler polls both regardless.)
	if incoming.GetSimplicityCovenant() != nil || resting.GetSimplicityCovenant() != nil {
		return false
	}
	return resting.GetCovenant() != nil
}

// Book is the subset of offerstore.Store the matcher needs (so it can be faked
// in tests). The matcher only READS the book (it commits no trade-truth), so this
// is read-only — ApplyPartialFill/RecordTrade were removed when the matcher became
// pure plan+emit; trade-truth is owned by the watcher (covenant) and SettleAck.
type Book interface {
	SnapshotPairEntries(p *seqobv1.AssetPair, confidential bool) []offerstore.Entry
	Get(k offerstore.Key) (*offerstore.Entry, bool)
}

// Match is one crossing of an incoming order against a resting order.
type Match struct {
	Pair        *seqobv1.AssetPair
	RestingKey  offerstore.Key
	Resting     *seqobv1.Offer
	IncomingKey offerstore.Key
	Incoming    *seqobv1.Offer
	FillBase    uint64 // base atoms filled
	FillQuote   uint64 // quote atoms owed for the fill (ceil price, maker's favour)

	// RestingCovenant is non-nil iff the resting order settles via a covenant
	// (maker offline; taker builds the FILL spend). RestingLocked is the atoms of
	// asset A still locked in that covenant UTXO before this fill.
	RestingCovenant *seqobv1.CovenantTerms
	RestingLocked   uint64
	// IncomingCovenant is non-nil iff the INCOMING order is also a covenant: both
	// makers are offline and an always-online settler must build the joint spend.
	IncomingCovenant *seqobv1.CovenantTerms

	// RestingLightning / IncomingLightning are non-nil iff that side chose the
	// Lightning rail (its owner settles the cross off-chain, not with an on-chain
	// covenant FILL or same-chain co-sign). They are the cross-rail dual of the
	// covenant fields: when EXACTLY ONE side is a covenant and the OTHER is
	// Lightning, this Match is a CROSS-RAIL cross (CrossRail) that no same-rail
	// path can settle — the two owners each rest on a different rail. It is bridged
	// by the silent settlement BRIDGE (cmd/seqob-bridge), which fills the on-chain
	// covenant and settles the Lightning leg atomically under one preimage so the
	// two orders cross invisibly to their owners.
	RestingLightning  *seqobv1.LightningTerms
	IncomingLightning *seqobv1.LightningTerms
}

// BothCovenant reports the two-covenant / both-offline case.
func (m *Match) BothCovenant() bool {
	return m.RestingCovenant != nil && m.IncomingCovenant != nil
}

// CrossRail reports the cross-rail case the bridge settles: exactly one side
// rests on-chain as a covenant and the other rests on Lightning. This is the
// only match kind neither the covenant FILL (single or joint) nor the same-chain
// co-sign path can settle by itself, because the two owners chose different
// rails; the bridge crosses them by moving value between the rails under one
// shared preimage. A same-rail match (both covenant, both same-chain, both
// Lightning) is NOT cross-rail and keeps its existing settlement path.
func (m *Match) CrossRail() bool {
	covs, lns := 0, 0
	if m.RestingCovenant != nil {
		covs++
	}
	if m.IncomingCovenant != nil {
		covs++
	}
	if m.RestingLightning != nil {
		lns++
	}
	if m.IncomingLightning != nil {
		lns++
	}
	return covs == 1 && lns == 1
}

// BridgeCovenant returns the covenant (on-chain) side's terms and whether it is
// the RESTING side, for a CrossRail match. The bridge fills this covenant. It
// returns (nil, false) when the match is not cross-rail.
func (m *Match) BridgeCovenant() (terms *seqobv1.CovenantTerms, resting bool) {
	if !m.CrossRail() {
		return nil, false
	}
	if m.RestingCovenant != nil {
		return m.RestingCovenant, true
	}
	return m.IncomingCovenant, false
}

// BridgeLightning returns the Lightning side's terms for a CrossRail match (the
// leg the bridge settles off-chain via the submarine/pure-LN orchestrator), or
// nil when the match is not cross-rail.
func (m *Match) BridgeLightning() *seqobv1.LightningTerms {
	if !m.CrossRail() {
		return nil
	}
	if m.RestingLightning != nil {
		return m.RestingLightning
	}
	return m.IncomingLightning
}

// Matcher crosses incoming orders against a Book.
type Matcher struct{ book Book }

// New returns a Matcher over book.
func New(book Book) *Matcher { return &Matcher{book: book} }

// Disposition tells the caller what to do with the incoming order's UNFILLED
// remainder after crossing, per its time-in-force.
type Disposition int

const (
	// DispRest keeps the incoming's unfilled remainder RESTING as a maker order
	// (GTC / Limit; the default). A non-allow_partial GTC order that could not fully
	// fill also rests (execute-nothing-then-rest, the pre-existing behavior).
	DispRest Disposition = iota
	// DispCancelRemainder routes the matches but the unfilled remainder must NOT rest
	// — the caller removes the incoming from the book (IOC: immediate-or-cancel).
	DispCancelRemainder
	// DispReject emits NO matches and removes the incoming entirely: either FOK could
	// not fill the WHOLE order at once (all-or-nothing failed) or a POST_ONLY order
	// would have taken (crossed). Nothing executes and nothing rests.
	DispReject
)

// CrossResult is the outcome of crossing an incoming order under its time-in-force:
// the matches to route plus the incoming's disposition.
type CrossResult struct {
	Matches     []Match
	Disposition Disposition
	Filled      uint64 // incoming base atoms crossed by Matches
	Remainder   uint64 // incoming base atoms left unfilled by Matches
}

// Cross matches a freshly-rested incoming order against the resting opposite side
// under GTC (good-till-cancel) semantics: it returns the matches (best-price,
// oldest-first) for the caller to route and leaves the unfilled remainder resting.
// It honors a non-allow_partial incoming as all-or-nothing-then-REST. It is the
// pure planning primitive; CrossOrder wraps it with the IOC/FOK/POST_ONLY options.
func (m *Matcher) Cross(incoming *seqobv1.Offer) []Match {
	matches, remainder := m.walk(incoming)
	// All-or-nothing GTC incoming: if it disallows partials and the crossing liquidity
	// cannot fill it completely, execute nothing (leave it resting).
	if !incoming.GetAllowPartial() && remainder != 0 {
		return nil
	}
	return matches
}

// CrossOrder crosses the incoming order under its TimeInForce, returning the
// matches to route and the incoming's disposition (rest / cancel-remainder /
// reject). GTC (the default) preserves Cross's behavior. IOC fills-then-cancels the
// remainder; FOK is all-or-nothing-then-cancel; POST_ONLY rejects if it would take.
func (m *Matcher) CrossOrder(incoming *seqobv1.Offer) CrossResult {
	matches, remainder := m.walk(incoming)
	var filled uint64
	for _, mt := range matches {
		filled += mt.FillBase
	}
	switch incoming.GetTimeInForce() {
	case seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY:
		// Rest-only: any cross means it would take -> reject (execute nothing, don't rest).
		if len(matches) > 0 {
			return CrossResult{Disposition: DispReject, Remainder: filled + remainder}
		}
		return CrossResult{Disposition: DispRest, Remainder: remainder}
	case seqobv1.TimeInForce_TIME_IN_FORCE_FOK:
		// All-or-nothing-then-cancel: if the whole incoming can't fill at once, reject.
		if remainder != 0 {
			return CrossResult{Disposition: DispReject, Remainder: filled + remainder}
		}
		return CrossResult{Matches: matches, Disposition: DispRest, Filled: filled} // fully filled: nothing to rest
	case seqobv1.TimeInForce_TIME_IN_FORCE_IOC:
		// Fill now, cancel the remainder (never rest it).
		return CrossResult{Matches: matches, Disposition: DispCancelRemainder, Filled: filled, Remainder: remainder}
	default: // GTC / unspecified
		if !incoming.GetAllowPartial() && remainder != 0 {
			return CrossResult{Disposition: DispRest, Remainder: filled + remainder} // execute nothing, leave resting
		}
		return CrossResult{Matches: matches, Disposition: DispRest, Filled: filled, Remainder: remainder}
	}
}

// walk is the pure price-time-priority book walk: it plans the crossing of the
// incoming order against the resting opposite side and returns the matches (best-
// price, oldest-first) and the incoming's UNFILLED remainder. It applies no
// time-in-force policy (Cross / CrossOrder do) and commits no trade-truth.
func (m *Matcher) walk(incoming *seqobv1.Offer) ([]Match, uint64) {
	inKey := offerstore.Key{MakerPubkey: incoming.GetMakerPubkey(), OfferID: incoming.GetOfferId()}
	inEntry, ok := m.book.Get(inKey)
	if !ok {
		return nil, 0
	}
	incomingRemaining := inEntry.ActiveAmount
	if incomingRemaining == 0 {
		return nil, 0
	}

	rest := m.candidates(incoming)
	if len(rest) == 0 {
		return nil, incomingRemaining
	}

	// Plan (pure) so we can honor an all-or-nothing incoming before mutating.
	type planned struct {
		e         offerstore.Entry
		key       offerstore.Key
		fillBase  uint64
		fillQuote uint64
	}
	var plan []planned
	rem := incomingRemaining
	for i := range rest {
		if rem == 0 {
			break
		}
		e := rest[i]
		restActive := e.ActiveAmount
		if restActive == 0 {
			continue
		}
		fillBase := restActive
		if rem < fillBase {
			fillBase = rem
		}
		partialOfResting := fillBase < restActive

		// allow_partial / min_fill on the resting order.
		if partialOfResting && !e.Offer.GetAllowPartial() {
			continue
		}
		if partialOfResting && e.Offer.GetMinFill() > 0 && fillBase < e.Offer.GetMinFill() {
			continue
		}

		// Covenant floors: filled >= min_lot; remainder == 0 or >= min_lot. The
		// on-chain FILL leaf enforces these, so a match that violated them would be
		// unsettleable — reject it here.
		cov := e.Offer.GetCovenant()
		if cov != nil {
			ml := cov.GetMinLot()
			if fillBase < ml {
				continue
			}
			if r := restActive - fillBase; r != 0 && r < ml {
				// Trim to leave exactly a >= min_lot remainder, if the incoming can.
				if restActive >= ml && rem >= restActive-ml {
					fillBase = restActive - ml
				} else {
					continue
				}
				if fillBase < ml {
					continue // the trim itself fell below min_lot (restActive < 2*min_lot): no valid partial exists
				}
			}
		}

		fq := fillQuote(e.Offer, fillBase)
		if fq == 0 {
			// fillBase > 0 but the quote came out 0: either a zero rate denominator or a
			// quote that overflowed uint64 (fillQuote returns 0 for both). Emitting the Match
			// anyway would advertise "0 quote owed" for real base, so skip this candidate.
			continue
		}
		plan = append(plan, planned{
			e: e, key: keyOf(e.Offer),
			fillBase:  fillBase,
			fillQuote: fq,
		})
		rem -= fillBase
	}

	if len(plan) == 0 {
		return nil, rem
	}

	out := make([]Match, 0, len(plan))
	for _, p := range plan {
		// The matcher is PURE plan + emit: it computes the crossing and advertises the Match, but commits
		// NO trade-truth (no decrement, no RecordTrade). A cross is only a trade once it SETTLES on-chain,
		// and only that is recorded by the party that owns the settlement:
		//   - COVENANT resting order: the chain watcher, once the FILL is in a block, records the fill from
		//     the filling transaction's outputs (RecordCovenantFill, which records its trade with the txid)
		//     and re-rests or holds the order; it undoes the fill, retracting that trade, if the block leaves
		//     the chain (Principle 1). So the matcher must NOT pre-decrement or record here.
		//   - INTERACTIVE resting order: the maker's SettleAck after the lift broadcasts (B-1).
		// Recording/decrementing at match time was the phantom-trade + double-count source (an auto-cross
		// mints an unregistered session for interactive, and double-recorded covenants once the watcher
		// was enabled). Leaving the order at full size until settlement can briefly over-advertise a covenant
		// to two takers, but on-chain double-spend enforces a single fill and the watcher reconciles size on
		// the first FILL it sees (mempool -> HoldCovenantForSpend). (B-2 / on-confirm recording)

		mt := Match{
			Pair:        incoming.GetPair(),
			RestingKey:  p.key,
			Resting:     p.e.Offer,
			IncomingKey: inKey,
			Incoming:    incoming,
			FillBase:    p.fillBase,
			FillQuote:   p.fillQuote,
		}
		if cov := p.e.Offer.GetCovenant(); cov != nil {
			mt.RestingCovenant = cov
			mt.RestingLocked = p.e.ActiveAmount // atoms of A locked before this fill
		}
		if cov := incoming.GetCovenant(); cov != nil {
			mt.IncomingCovenant = cov
		}
		// Surface the Lightning rail on either side so a covenant-vs-Lightning cross
		// classifies as CrossRail (the bridge's job). Same-rail Lightning matches
		// (both sides Lightning) still carry these but are not CrossRail.
		if ln := p.e.Offer.GetLightning(); ln != nil {
			mt.RestingLightning = ln
		}
		if ln := incoming.GetLightning(); ln != nil {
			mt.IncomingLightning = ln
		}
		out = append(out, mt)
	}
	return out, rem
}

// candidates returns the resting opposite-side orders of incoming's pair that
// (a) are live with active size and (b) cross incoming's price, sorted best-
// price-first then oldest-first (price-time priority).
func (m *Matcher) candidates(incoming *seqobv1.Offer) []offerstore.Entry {
	// Snapshot only the incoming order's OWN book namespace (confidential vs
	// transparent), so a confidential offer only ever sees confidential candidates.
	all := m.book.SnapshotPairEntries(incoming.GetPair(), incoming.GetConfidential())
	inKey := keyOf(incoming)
	buy := incoming.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY

	var c []offerstore.Entry
	for _, e := range all {
		if keyOf(e.Offer) == inKey {
			continue // never match against self
		}
		if e.ActiveAmount == 0 {
			continue
		}
		// Belt-and-suspenders even with namespaced snapshots: a confidential offer
		// crosses ONLY another confidential offer. Both legs must blind on-chain, else
		// an observer reads the confidential leg's amount off the public swap ratio.
		if e.Offer.GetConfidential() != incoming.GetConfidential() {
			continue
		}
		// Opposite side only.
		if (e.Offer.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY) == buy {
			continue
		}
		// Same-pair opposite orders line up assets by construction; require the
		// swap to be well-formed and the price to cross.
		if incoming.GetOfferAsset() != e.Offer.GetWantAsset() ||
			incoming.GetWantAsset() != e.Offer.GetOfferAsset() {
			continue
		}
		// Sub-asset offers (asset-LN <-> BTC on-chain HTLC) are COURIER-LIFT ONLY: a
		// taker runs the HTLC handshake, and the relay has no maker-vs-maker settlement
		// path for them, so they must never auto-cross. This keeps a sub-asset BUY
		// (ln 4) and SELL (ln 5) coexisting on the asset/BTC pair without crossing.
		if isSubAssetOffer(incoming) || isSubAssetOffer(e.Offer) {
			continue
		}
		// Settlement-owner precondition (decision 3: every match settles). Auto-cross
		// ONLY a combination a live out-of-band owner can settle (>= 1 covenant side:
		// the settler for covenant<->covenant, the bridge for covenant<->Lightning,
		// the taker's own permissionless FILL for covenant<->interactive). Guard every
		// ownerless combination (interactive/pure-LN/submarine/cross-chain with no
		// covenant side) so no advisory match ever ships — those settle taker-initiated.
		if !hasSettlementOwner(incoming, e.Offer) {
			continue
		}
		if !crosses(incoming, e.Offer) {
			continue
		}
		c = append(c, e)
	}
	sort.SliceStable(c, func(i, j int) bool {
		pi, pj := quotePerBase(c[i].Offer), quotePerBase(c[j].Offer)
		cmp := pi.Cmp(pj)
		if cmp != 0 {
			if buy {
				return cmp < 0 // incoming BUY wants the cheapest ask first
			}
			return cmp > 0 // incoming SELL wants the richest bid first
		}
		return c[i].CreatedAt.Before(c[j].CreatedAt) // time priority
	})
	return c
}

// crosses reports whether the incoming (taker) order is willing to pay at least
// the resting (maker) order's price. The taker pays maker.want_asset and
// receives maker.offer_asset; the floor is taker.offer/taker.want >=
// maker.want/maker.offer, cleared of division:
//
//	incoming.offer_amount * resting.offer_amount >= resting.want_amount * incoming.want_amount
func crosses(incoming, resting *seqobv1.Offer) bool {
	lhs := new(big.Int).Mul(u(incoming.GetOfferAmount()), u(resting.GetOfferAmount()))
	rhs := new(big.Int).Mul(u(resting.GetWantAmount()), u(incoming.GetWantAmount()))
	return lhs.Cmp(rhs) >= 0
}

// quotePerBase is the resting order's price as quote atoms per base atom, for
// sorting. For a SELL (gives base=offer, wants quote=want): want/offer. For a
// BUY (gives quote=offer, wants base=want): offer/want.
func quotePerBase(o *seqobv1.Offer) *big.Rat {
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		return new(big.Rat).SetFrac(u(o.GetWantAmount()), u(o.GetOfferAmount()))
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		return new(big.Rat).SetFrac(u(o.GetOfferAmount()), u(o.GetWantAmount()))
	default:
		return new(big.Rat)
	}
}

// fillQuote is the quote atoms owed for filling fillBase of the resting order,
// ceil-rounded in the maker's favour. For a covenant it uses the on-chain rate
// (authoritative); otherwise the offer's want/offer ratio. In both cases the
// quote is measured against the resting order's BASE size.
func fillQuote(resting *seqobv1.Offer, fillBase uint64) uint64 {
	var num, den uint64
	if cov := resting.GetCovenant(); cov != nil {
		num, den = cov.GetRateNum(), cov.GetRateDen()
	} else {
		// quote per base: SELL want/offer; BUY offer/want. base == offer(SELL) or
		// want(BUY), and fillBase is in base atoms, so quote = fillBase * (quote/base).
		switch resting.GetTradeDir() {
		case seqobv1.TradeDir_TRADE_DIR_SELL:
			num, den = resting.GetWantAmount(), resting.GetOfferAmount()
		case seqobv1.TradeDir_TRADE_DIR_BUY:
			num, den = resting.GetOfferAmount(), resting.GetWantAmount()
		}
	}
	if den == 0 {
		return 0
	}
	// ceil(fillBase * num / den) with big.Int to avoid overflow.
	p := new(big.Int).Mul(u(fillBase), u(num))
	p.Add(p, u(den-1))
	p.Quo(p, u(den))
	if !p.IsUint64() {
		return 0 // B-6: quote doesn't fit uint64 — p.Uint64() would silently wrap to a wrong amount; reject the match instead
	}
	return p.Uint64()
}

func u(x uint64) *big.Int { return new(big.Int).SetUint64(x) }

func keyOf(o *seqobv1.Offer) offerstore.Key {
	return offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
}
