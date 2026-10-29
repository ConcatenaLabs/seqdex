package main

// book.go — the per-pair UNION book. The unified view of a market is built by
// reading every relay's REST orderbook DIRECTLY (no LSP dependency): each relay
// keys markets by its exact base/quote, so both orientations are fetched, every
// offer's maker signature is verified locally (the relay is untrusted — same
// stance as seqob-cli's book command), expired offers are dropped, and the same
// signed offer served by several relays is deduped to one order.

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func getProtoJSON(url string, out proto.Message) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return jsonUnmarshal.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FetchPairOffers pulls one pair's offers (BOTH orientations) from every relay.
// A relay that is down or lacks the market is skipped with a log line, never
// fatal — the union is whatever the reachable relays serve.
func FetchPairOffers(relays []string, base, quote string, logf func(string, ...interface{})) (map[string][]*seqobv1.Offer, map[string]*seqobv1.OrderStatus) {
	out := map[string][]*seqobv1.Offer{}
	statuses := map[string]*seqobv1.OrderStatus{}
	for _, r := range relays {
		var offers []*seqobv1.Offer
		for _, orient := range [][2]string{{base, quote}, {quote, base}} {
			var book seqobv1.PublicBook
			url := fmt.Sprintf("%s/v1/market/%s/%s/orderbook", r, orient[0], orient[1])
			if err := getProtoJSON(url, &book); err != nil {
				// 404/empty market on this orientation is normal; log at trace level only.
				continue
			}
			offers = append(offers, book.GetOffers()...)
			for _, st := range book.GetStatuses() {
				statuses[st.GetMakerPubkey()+":"+st.GetOfferId()] = st
			}
		}
		if len(offers) > 0 {
			out[r] = offers
		}
	}
	return out, statuses
}

// ApplyStatuses keeps the offers the relays call liftable (open, or partly
// filled with something left) and caps each one's size to what is left of it:
// the relay refuses a take over an offer's active_amount, so a leg sized to the
// signed whole of a partly filled offer could never open. active_amount is in
// the offer's own base; a flipped offer's own base is our quote. An offer
// without a status (an older relay) passes as signed.
func ApplyStatuses(union []*NormOrder, statuses map[string]*seqobv1.OrderStatus) []*NormOrder {
	out := union[:0]
	for _, n := range union {
		st, ok := statuses[n.ID()]
		if !ok {
			out = append(out, n)
			continue
		}
		switch st.GetStatus() {
		case seqobv1.OfferStatus_OFFER_STATUS_OPEN, seqobv1.OfferStatus_OFFER_STATUS_PARTIAL:
		default:
			continue
		}
		left := st.GetActiveAmount()
		if n.Flipped {
			left = floorMulDiv(left, n.BaseDen, n.QuoteNum)
		}
		if left == 0 {
			continue
		}
		if left < n.BaseSize {
			n.BaseSize = left
		}
		out = append(out, n)
	}
	return out
}

// BuildUnion normalizes, verifies, filters and dedupes one pair's raw offers
// into the union book. minTTL drops offers expiring too soon to settle a leg
// against. Pure given its inputs (now injected) — unit-tested.
func BuildUnion(offersByRelay map[string][]*seqobv1.Offer, base, quote string, now time.Time, minTTL time.Duration, logf func(string, ...interface{})) []*NormOrder {
	seen := map[string]bool{}
	var out []*NormOrder
	// Deterministic relay iteration so dedupe keeps a stable representative.
	relays := make([]string, 0, len(offersByRelay))
	for r := range offersByRelay {
		relays = append(relays, r)
	}
	sort.Strings(relays)
	for _, relay := range relays {
		for _, o := range offersByRelay[relay] {
			key := o.GetMakerPubkey() + ":" + o.GetOfferId()
			if seen[key] {
				continue
			}
			// The relay is untrusted: verify the maker's signature before treating
			// the row as a genuine resting offer (same rule as seqob-cli book).
			if err := offer.VerifyOffer(o); err != nil {
				logf("drop unverified offer %s from %s: %v", key, relay, err)
				continue
			}
			if exp := o.GetExpiresAtUnix(); exp > 0 && time.Unix(int64(exp), 0).Before(now.Add(minTTL)) {
				continue // expired or expiring inside the settlement window
			}
			n, why := Normalize(o, relay, base, quote)
			if n == nil {
				if why != "not this pair" {
					logf("drop offer %s from %s: %s", key, relay, why)
				}
				continue
			}
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// DiscoverPairs asks every relay for its market list (-auto). Orientation
// dedupe: if two relays serve the same two assets in opposite orientations, the
// first-seen orientation wins, preferring the one quoted in BTC (cross/LN books
// quote the asset in the BTC sentinel, matching the taker CLIs' <asset>/BTC
// convention).
func DiscoverPairs(relays []string, logf func(string, ...interface{})) [][2]string {
	type pk struct{ a, b string }
	seen := map[pk][2]string{}
	var order []pk
	for _, r := range relays {
		var ml seqobv1.MarketList
		if err := getProtoJSON(r+"/v1/markets", &ml); err != nil {
			logf("relay %s market list: %v", r, err)
			continue
		}
		for _, m := range ml.GetMarkets() {
			b, q := m.GetPair().GetBaseAsset(), m.GetPair().GetQuoteAsset()
			if b == "" || q == "" || b == q {
				continue
			}
			key := pk{a: minStr(b, q), b: maxStr(b, q)}
			cur, ok := seen[key]
			switch {
			case !ok:
				seen[key] = [2]string{b, q}
				order = append(order, key)
			case cur[1] != offer.BTCSentinel && q == offer.BTCSentinel:
				seen[key] = [2]string{b, q} // upgrade to the BTC-quoted orientation
			}
		}
	}
	out := make([][2]string, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}

func minStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}
func maxStr(a, b string) string {
	if a < b {
		return b
	}
	return a
}
