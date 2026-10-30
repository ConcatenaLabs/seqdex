// Feed guard: retire a quote the moment the market has moved through it.
//
// A maker's quote is the flag pair it was launched with; -max-quote-age bounds
// how old it gets, but on a fast market a quote is stale long before that: once
// the price feed's mid has moved to within -guard-bps of the quote (an ask at or
// below mid*(1+bps), a bid at or above mid*(1-bps)), the quote is a free option
// to any taker and crosses every maker that priced after the move. With
// -guard-feed set, the maker reads the feed every -guard-every and, when that
// happens to its touch level, exits for a re-quote exactly as -max-quote-age
// does: when idle, the supervisor re-runs it at the price of now.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

type feedGuardConfig struct {
	URL                 string        // the price server's /prices
	BaseKey, QuoteKey   string        // the feed's key for each asset
	BasePrec, QuotePrec int           // display precision: the feed prices whole units
	Bps                 float64       // the margin
	Every               time.Duration // how often to read the feed
}

var feedGuard feedGuardConfig

// atomPrice is an offer's price in quote atoms per base atom, and whether the
// maker sells the base (an ask).
func atomPrice(o *seqobv1.Offer) (price float64, ask bool) {
	off, want := float64(o.GetOfferAmount()), float64(o.GetWantAmount())
	if off <= 0 || want <= 0 {
		return 0, false
	}
	if o.GetOfferAsset() == o.GetPair().GetBaseAsset() {
		return want / off, true
	}
	return off / want, false
}

// feedMid is the feed's price of the pair in quote atoms per base atom.
func feedMid(base, quote float64, basePrec, quotePrec int) float64 {
	if base <= 0 || quote <= 0 {
		return 0
	}
	m := base / quote
	for i := basePrec; i < quotePrec; i++ {
		m *= 10
	}
	for i := quotePrec; i < basePrec; i++ {
		m /= 10
	}
	return m
}

// throughFeed reports whether the market has moved through the quote: an ask
// at or below mid*(1+bps), or a bid at or above mid*(1-bps).
func throughFeed(o *seqobv1.Offer, mid, bps float64) bool {
	p, ask := atomPrice(o)
	if p <= 0 || mid <= 0 {
		return false
	}
	if ask {
		return p <= mid*(1+bps/1e4)
	}
	return p >= mid*(1-bps/1e4)
}

// feedMaxAge is how old a price the feed dates (as_of, the time of the reading
// it serves) may be: an older one is its source's last answer, and reads as no
// price, so the guard leaves the quote alone as it does when the feed is down.
const feedMaxAge = 10 * time.Minute

// readFeedPrice reads one key of the feed: a number or {"price": number}. A
// price dated more than feedMaxAge ago reads as 0; a seeded one ("static") has
// no age.
func readFeedPrice(feed map[string]json.RawMessage, key string) float64 {
	return readFeedPriceAt(feed, key, time.Now())
}

func readFeedPriceAt(feed map[string]json.RawMessage, key string, now time.Time) float64 {
	raw, ok := feed[key]
	if !ok {
		return 0
	}
	var v float64
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	var obj struct {
		Price  float64 `json:"price"`
		AsOf   int64   `json:"as_of"`
		Static bool    `json:"static"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if !obj.Static && obj.AsOf > 0 && now.Sub(time.Unix(obj.AsOf, 0)) > feedMaxAge {
			return 0
		}
		return obj.Price
	}
	return 0
}

// armFeedGuard watches the feed for o (the touch level) when -guard-feed is set.
// A feed that does not answer leaves the quote alone: -max-quote-age still
// bounds it.
func armFeedGuard(o *seqobv1.Offer, idle func() bool) {
	g := feedGuard
	if g.URL == "" || g.BaseKey == "" || g.QuoteKey == "" {
		return
	}
	every := g.Every
	if every <= 0 {
		every = 20 * time.Second
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	go func() {
		for range time.Tick(every) {
			resp, err := cl.Get(g.URL)
			if err != nil {
				continue
			}
			var feed map[string]json.RawMessage
			err = json.NewDecoder(resp.Body).Decode(&feed)
			resp.Body.Close()
			if err != nil {
				continue
			}
			mid := feedMid(readFeedPrice(feed, g.BaseKey), readFeedPrice(feed, g.QuoteKey), g.BasePrec, g.QuotePrec)
			if throughFeed(o, mid, g.Bps) {
				p, _ := atomPrice(o)
				requoteExitIfIdle(fmt.Sprintf("the feed moved through the quote (touch %.6g, mid %.6g quote atoms per base atom, margin %g bps)", p, mid, g.Bps), idle)
				return
			}
		}
	}()
}
