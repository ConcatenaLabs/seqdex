// Package bridge is the SILENT cross-rail settlement bridge for the SeqOB CLOB.
//
// It lets an order placed ON-CHAIN (a funded covenant resting order) match an
// order placed on LIGHTNING, invisibly. The two owners each rest on the rail
// THEY chose and never learn a bridge was involved: no "LP", no "submarine", no
// "take" vs "post", no on-chain-vs-Lightning label. The relay's matcher crosses
// them by price-time priority exactly like any other cross and surfaces a
// CrossRail match (matcher.Match.CrossRail); the bridge turns that match into a
// single atomic settlement that moves value between the two rails.
//
// What the bridge is NOT. It is not a market maker and not a liquidity pool /
// AMM (that is a separate future feature any user can run). Its ONLY job is to
// bridge one on-chain order to one Lightning order so their cross settles. It is
// value-flat except for the fee/spread it keeps.
//
// The settlement (one shared SHA256 preimage P binds both legs):
//
//   - On-chain leg: the bridge FILLS the covenant. It pays the on-chain maker
//     that maker's own pinned ceil price in asset Y (covenant.Order.PlanFill,
//     consensus-enforced) and receives the sold asset X. The bridge cannot
//     underpay, redirect, or skim: the FILL leaf re-checks every maker-facing
//     output, so a cheating bridge builds a transaction the covenant rejects
//     (script-verify-flag-failed) — identical to the settler's guarantee.
//
//   - Lightning leg: the bridge delivers X to the Lightning counterparty and
//     collects Y from it over Lightning, via the submarine / pure-LN orchestrator
//     (pkg/xchain) — REUSED unchanged, never rebuilt.
//
// Net: asset X moves on-chain -> Lightning, asset Y moves Lightning -> on-chain;
// the bridge keeps FeeY = (Y collected over LN) - (Y paid to the on-chain maker).
//
// Trust / no-loss model. The bridge is a SEPARATE process from the keyless,
// fundless relay (seqobd) and from the fundless settler/watcher: unlike them it
// DOES hold its own inventory and an LN node (it must, to front). But it can
// never take USER funds — the on-chain maker is protected by consensus (the FILL
// leaf) and the Lightning counterparty is protected by the preimage atomicity.
// The ONLY capital ever at risk is the bridge's OWN fronted inventory, and only
// on a Bitcoin reorg of the on-chain FILL, bounded by the per-offer 0-conf cap
// (max_0conf_amount) and required to be covered by the fee (principle #4). Above
// the cap the bridge refuses to 0-conf-front and waits for the FILL to be
// anchor-buried before releasing the Lightning leg (RunNormal/RunReverse already
// implement exactly this gate). So no counterparty can be cheated and the bridge
// itself can lose at most what its own fee already covers.
package bridge

import (
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// FeeScale is 1e8, the open-fee-market exchange-rate scale shared by
// price_server.py / exchangerates.cpp / the submarine claim-fee sizing. A rate is
// the fee asset's value in SEQ-sats * 1e8; asset_fee_atoms = ceil(policy_fee_sats
// * 1e8 / rate). See MEMORY principle #4 — this preserves fee VALUE across assets
// and is NEVER inverted.
const FeeScale = 100_000_000

// LNSide is the Lightning counterparty's terms the bridge needs to price and
// gate the off-chain leg. The bridge never sees or needs the counterparty's keys;
// these come straight from the Lightning order the relay matched.
type LNSide struct {
	// OfferY / WantX are the Lightning order's price as integers (the counterparty
	// offers OfferY atoms of Y to receive WantX atoms of X). The executed LN Y is
	// prorated from this at the covenant fill size, floored in the bridge's favour.
	OfferY uint64
	WantX  uint64
	// Max0Conf is the per-offer 0-conf cap (LightningTerms.max_0conf_amount): the
	// most X-value the bridge may front at 0-conf before the on-chain FILL is
	// anchor-buried. 0 disables fronting (always wait for confirmations).
	Max0Conf uint64
}

// Settlement is the bridge's complete, enforced recipe for one cross-rail cross:
// the consensus-checked on-chain FILL leg plus the priced/ gated Lightning leg.
type Settlement struct {
	// On-chain FILL leg (the covenant the bridge fills). Fill.RequiredB == PayY and
	// Fill.Filled == RecvX; the witness [FillLeaf, ControlBlock] is introspection-
	// only (no bridge signature authorises the maker payout — consensus does).
	Fill  *covenant.FillPlan
	PayY  uint64 // asset-Y atoms paid to the on-chain maker (its own ceil price)
	RecvX uint64 // asset-X atoms the bridge receives on-chain from the FILL

	// Lightning leg (settled via the submarine/pure-LN orchestrator, one preimage).
	LNDeliverX uint64 // asset-X atoms the bridge delivers to the LN counterparty
	LNRecvY    uint64 // asset-Y atoms the bridge collects from the LN counterparty

	// Economics.
	FeeY    uint64 // spread the bridge keeps = LNRecvY - PayY (>= 0 by crossing)
	MinFeeY uint64 // principle-#4 sized policy-fee floor the fee must clear to front

	// Fronting / atomicity decision.
	FrontAmount   uint64 // X-value exposed to a FILL reorg if released 0-conf (== RecvX)
	Max0Conf      uint64 // the per-offer 0-conf cap echoed for the caller
	ZeroConfFront bool   // release the LN leg at 0-conf (instant, bridge fronts the risk)
	RequireConfs  bool   // must wait for the FILL to be anchor-buried before releasing
	FrontReason   string // why fronting was/ wasn't chosen (audit)
}

// SizePolicyFee sizes a native-sats policy fee into asset-Y atoms via the
// open-fee-market rate: asset_fee_atoms = ceil(policyFeeSats * 1e8 / rateY).
// rateY <= 0 (or the native/policy asset, where rate == 1e8) falls back to the
// flat sats value. This is the SAME arithmetic as the submarine claim-fee sizing
// and MUST NOT be inverted (principle #4).
func SizePolicyFee(policyFeeSats, rateY uint64) uint64 {
	if rateY == 0 {
		return policyFeeSats
	}
	f := (policyFeeSats*FeeScale + rateY - 1) / rateY // ceil
	if f == 0 {
		f = 1
	}
	return f
}

// Plan builds the bridge settlement for taking `fillX` atoms of asset X from the
// on-chain covenant `covOrder` (currently holding `locked` atoms), spent at input
// k of the bridge's fill transaction (its credit at output 2k, its remainder or
// gap at 2k+1; k > 0 when the bridge fills it beside other covenant inputs), and
// crossing it against the Lightning order `ln`. policyFeeSats is the network
// policy fee the bridge wants covered before it fronts; rateY is asset Y's
// open-fee-market rate.
//
// Plan fails closed if the cross would underpay the on-chain maker (LN Y received
// < the covenant's ceil price) — so a bad advisory cross from the matcher can
// never make the bridge settle at a loss to the maker. It also computes the
// fronting/no-front decision (0-conf only within the cap AND when the fee covers
// the reorg exposure) so the caller never fronts more than the fee covers.
func Plan(covOrder covenant.Order, locked, fillX uint64, k uint32, ln LNSide, policyFeeSats, rateY uint64) (*Settlement, error) {
	if ln.WantX == 0 {
		return nil, fmt.Errorf("lightning side has zero want_x")
	}
	// On-chain FILL leg at input index k. This enforces the covenant's own
	// min_lot floors and the ceil price the maker MUST receive.
	fp, err := covOrder.PlanFill(locked, fillX, k)
	if err != nil {
		return nil, fmt.Errorf("on-chain FILL leg: %w", err)
	}
	payY := fp.RequiredB

	// Lightning leg price: what the LN order pays in Y for fillX of X, prorated
	// from its (OfferY, WantX) limit and floored (never assume the counterparty
	// pays more than it offered).
	recvY := mulDiv(fillX, ln.OfferY, ln.WantX)
	if recvY < payY {
		return nil, fmt.Errorf("cross underpays the on-chain maker: LN pays %d Y but the covenant's ceil price is %d Y", recvY, payY)
	}
	feeY := recvY - payY
	minFeeY := SizePolicyFee(policyFeeSats, rateY)

	s := &Settlement{
		Fill:        fp,
		PayY:        payY,
		RecvX:       fp.Filled,
		LNDeliverX:  fp.Filled,
		LNRecvY:     recvY,
		FeeY:        feeY,
		MinFeeY:     minFeeY,
		FrontAmount: fp.Filled,
		Max0Conf:    ln.Max0Conf,
	}

	// Fronting decision. 0-conf-front the Lightning leg (instant settlement) ONLY
	// when the exposed X-value is within the per-offer cap AND the fee covers that
	// reorg exposure; otherwise wait for the on-chain FILL to be anchor-buried.
	switch {
	case ln.Max0Conf == 0:
		s.RequireConfs = true
		s.FrontReason = "no 0-conf cap on the offer: wait for the FILL to be anchor-buried before releasing the LN leg"
	case s.FrontAmount > ln.Max0Conf:
		s.RequireConfs = true
		s.FrontReason = fmt.Sprintf("fill %d X exceeds the 0-conf cap %d: require confirmations", s.FrontAmount, ln.Max0Conf)
	case feeY < minFeeY:
		s.RequireConfs = true
		s.FrontReason = fmt.Sprintf("fee %d Y does not cover the sized policy fee %d Y: refuse to front, require confirmations", feeY, minFeeY)
	default:
		s.ZeroConfFront = true
		s.FrontReason = fmt.Sprintf("fill %d X within cap %d and fee %d Y covers %d Y: 0-conf front (instant)", s.FrontAmount, ln.Max0Conf, feeY, minFeeY)
	}
	return s, nil
}

// PlanFromMatch is the convenience wrapper the always-online loop uses: it takes
// a CrossRail matcher.Match, reconstructs the on-chain covenant order from the
// covenant side's advertised terms, and derives the Lightning side's price from
// the matched offers, then delegates to Plan. It errors on a non-cross-rail
// match so the bridge only ever touches the crosses that are its job.
func PlanFromMatch(m matcher.Match, ln LNSide, policyFeeSats, rateY uint64) (*Settlement, error) {
	terms, resting := m.BridgeCovenant()
	if terms == nil {
		return nil, fmt.Errorf("match is not cross-rail (no single covenant + Lightning pair)")
	}
	order, err := OrderFromTerms(terms)
	if err != nil {
		return nil, fmt.Errorf("covenant terms: %w", err)
	}
	// The covenant side's locked size: RestingLocked when the covenant rests, else
	// the incoming covenant's whole advertised size (the base amount it funds).
	locked := m.RestingLocked
	if !resting {
		locked = m.FillBase // an incoming covenant fills against a resting LN order
	}
	fillX := m.FillBase
	// One covenant crossed against one Lightning order: the bridge's fill spends
	// it alone, at input 0.
	return Plan(order, locked, fillX, 0, ln, policyFeeSats, rateY)
}

// mulDiv computes floor(a * b / c) without overflow for the values in play.
func mulDiv(a, b, c uint64) uint64 {
	if c == 0 {
		return 0
	}
	// a and b are asset atom counts (< 2^53 in practice); use 128-bit-safe path via
	// big only if needed. Here direct math stays within uint64 for realistic sizes,
	// but guard against overflow by splitting a.
	hi := (a / c) * b
	lo := ((a % c) * b) / c
	return hi + lo
}
