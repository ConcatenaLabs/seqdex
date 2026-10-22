package watcher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// SubmitProbe is the relay validator's liveness probe backed by the chain view:
// a COVENANT offer is accepted only if its advertised outpoint exists unspent at
// the tip, pays the program derived from its own signed terms, carries asset A
// explicitly, and holds at least the order's min_lot and at most the
// offer_amount its maker signed. Every other offer passes (the interactive and
// cross rails are proven at settlement, not at submit).
//
// An outpoint holding less than offer_amount is the order resting at its
// remainder: a part fill re-creates it under the same program at 2k+1, and the
// maker's signature (which leaves the outpoint out) still covers it. The probe
// returns what the outpoint holds and the relay lists the order at that size,
// so the signed offer can be posted again at its remainder, by its maker or by
// anyone holding it, after a relay restart. The rest of the order is the
// maker's signed terms; nothing in it is listed beyond what the maker signed.
//
// Without this, a submitted covenant with a bogus outpoint rested in the book
// until the watcher's next pass and cost that pass a window of getblock scans;
// and a maker could advertise more than it had locked.
//
// A node that cannot be asked is not chain evidence: the probe passes on an RPC
// error and leaves the decision to the watcher, which reconciles within its
// interval. It fails closed only on what the chain actually says.
type SubmitProbe struct {
	Chain ChainView
}

// ErrNotUnspent is the probe's refusal of an outpoint that is not in the UTXO
// set at the tip: spent (a fill or a refund), or gone.
var ErrNotUnspent = errors.New("is not an unspent output at the tip")

// CheckOffer implements validator.LivenessProbe.
func (p SubmitProbe) CheckOffer(_ context.Context, o *seqobv1.Offer) (uint64, error) {
	return ProbeCovenant(p.Chain, o)
}

// ProbeCovenant is CheckOffer without the interface. It returns the atoms of
// asset A the order's outpoint holds, which the relay lists it at; 0 (no chain
// evidence: list at the signed size) for a non-covenant offer or when the chain
// could not be asked; and an error when the chain contradicts the offer.
func ProbeCovenant(chain ChainView, o *seqobv1.Offer) (uint64, error) {
	if offer.IsPool(o) {
		return ProbePool(chain, o)
	}
	if !offer.IsCovenant(o) || chain == nil {
		return 0, nil
	}
	c := CovEntry{Offer: o, Terms: o.GetCovenant(), Sim: o.GetSimplicityCovenant(), Round: o.GetRoundOrder()}
	exp, err := c.expect()
	if err != nil {
		return 0, fmt.Errorf("covenant terms: %w", err)
	}
	txid, vout := c.outpoint()
	st, err := chain.Inspect(txid, vout, exp.OrderSPKHex, exp.AssetADisplay)
	if err != nil {
		return 0, nil // node unavailable: not evidence; the watcher reconciles
	}
	if !st.Unspent {
		return 0, fmt.Errorf("covenant outpoint %s:%d %w", txid, vout, ErrNotUnspent)
	}
	if !strings.EqualFold(st.SPKHex, exp.OrderSPKHex) {
		return 0, fmt.Errorf("covenant outpoint does not pay the program derived from its terms")
	}
	if st.Confidential {
		return 0, fmt.Errorf("covenant outpoint %s:%d is confidential (its asset or amount is a commitment): the order's leaf reads only explicit amounts, so no fill can spend it", txid, vout)
	}
	if !strings.EqualFold(st.AssetDisplay, exp.AssetADisplay) {
		return 0, fmt.Errorf("covenant outpoint holds asset %s, not asset_a %s", st.AssetDisplay, exp.AssetADisplay)
	}
	if st.Value > o.GetOfferAmount() {
		return 0, fmt.Errorf("covenant outpoint holds %d atoms, more than the offer_amount %d its maker signed", st.Value, o.GetOfferAmount())
	}
	if st.Value < exp.MinLot {
		return 0, fmt.Errorf("covenant outpoint holds %d atoms, below the order's min_lot %d: no fill can take it", st.Value, exp.MinLot)
	}
	return st.Value, nil
}
