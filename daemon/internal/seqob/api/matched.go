package api

import (
	"crypto/rand"
	"encoding/hex"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// matchedProto builds the From.matched payload for one recipient role.
func matchedProto(mt matcher.Match, role, sessionID string) *seqobv1.Matched {
	m := &seqobv1.Matched{
		SessionId:       sessionID,
		OfferId:         mt.RestingKey.OfferID,
		MakerPubkey:     mt.RestingKey.MakerPubkey,
		Role:            role,
		FillBaseAmount:  mt.FillBase,
		FillQuoteAmount: mt.FillQuote,
	}
	if mt.RestingCovenant != nil {
		m.RestingIsCovenant = true
		m.Covenant = mt.RestingCovenant
		m.CovenantLocked = mt.RestingLocked
	}
	return m
}

// routeMatches delivers From.matched to the counterparties of each match. The
// incoming submitter (taker connection `tc`, may be nil for the REST path) is
// told about every fill it triggered. For an INTERACTIVE resting order with an
// online maker, the maker is also notified so it can co-sign; a COVENANT resting
// order needs no maker round-trip (the taker settles the FILL spend itself), so
// its offline maker is not contacted.
func (s *Server) routeMatches(tc *wsConn, matches []matcher.Match) {
	for _, mt := range matches {
		sid := newSessionID()
		if tc != nil {
			_ = tc.send(&seqobv1.From{Msg: &seqobv1.From_Matched{Matched: matchedProto(mt, "taker", sid)}})
		}
		if mt.RestingCovenant == nil {
			if mc, ok := s.makerConns.get(mt.RestingKey.MakerPubkey); ok && mc != tc {
				_ = mc.send(&seqobv1.From{Msg: &seqobv1.From_Matched{Matched: matchedProto(mt, "maker", sid)}})
			}
		}
	}
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// applyDisposition retires the just-accepted incoming order per its time-in-force
// disposition (P2.9). It returns true iff it retired the order (removed it from the
// book), so the caller reports CANCELLED instead of a resting status.
//
//   - DispReject (FOK could not fill in full, or POST_ONLY would have taken): execute
//     nothing, do not rest — retire the order. A covenant's on-chain coin stays
//     refundable via its REFUND leaf, so delisting it from the relay is safe.
//   - DispCancelRemainder (IOC): the routed matches settle; the UNFILLED remainder
//     must not rest — retire it. EXCEPTION: a COVENANT that PARTIALLY filled is left
//     in place, because the settler needs its book entry to settle the filled slice
//     on-chain and the watcher re-rests the on-chain remainder; a funded covenant's
//     time-in-force is governed by its own on-chain expiry, not a relay retire.
//   - DispRest: keep resting (returns false).
func (s *Server) applyDisposition(o *seqobv1.Offer, res matcher.CrossResult) bool {
	k := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
	switch res.Disposition {
	case matcher.DispReject:
		_ = s.store.RetireIncoming(k, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED)
		return true
	case matcher.DispCancelRemainder:
		if res.Remainder > 0 && (!offer.IsCovenant(o) || res.Filled == 0) {
			_ = s.store.RetireIncoming(k, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED)
			return true
		}
		return false
	default:
		return false
	}
}

// cancelledStatus is the terminal OrderStatus reported to the submitter when the
// relay retired its order per its time-in-force (IOC remainder / FOK / POST_ONLY).
func cancelledStatus(o *seqobv1.Offer) *seqobv1.OrderStatus {
	return &seqobv1.OrderStatus{
		OfferId: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_CANCELLED,
	}
}
