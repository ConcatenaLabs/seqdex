package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func guardOffer(sell bool, baseAmt, quoteAmt uint64) *seqobv1.Offer {
	o := &seqobv1.Offer{Pair: &seqobv1.AssetPair{BaseAsset: "b", QuoteAsset: "q"}}
	if sell {
		o.OfferAsset, o.OfferAmount, o.WantAsset, o.WantAmount = "b", baseAmt, "q", quoteAmt
	} else {
		o.OfferAsset, o.OfferAmount, o.WantAsset, o.WantAmount = "q", quoteAmt, "b", baseAmt
	}
	return o
}

func TestThroughFeed(t *testing.T) {
	// An ask 20 bps over a mid of 1.0: fresh until the mid rises to within 10 bps of it.
	ask := guardOffer(true, 10000, 10020)
	for _, c := range []struct {
		mid  float64
		want bool
	}{{1.0, false}, {1.0009, false}, {1.0011, true}, {1.05, true}, {0.9, false}} {
		if got := throughFeed(ask, c.mid, 10); got != c.want {
			t.Errorf("ask at 1.002, mid %v: through=%v want %v", c.mid, got, c.want)
		}
	}
	// A bid 20 bps under 1.0: stale once the mid falls to within 10 bps of it.
	bid := guardOffer(false, 10000, 9980)
	for _, c := range []struct {
		mid  float64
		want bool
	}{{1.0, false}, {0.9991, false}, {0.9989, true}, {0.9, true}, {1.1, false}} {
		if got := throughFeed(bid, c.mid, 10); got != c.want {
			t.Errorf("bid at 0.998, mid %v: through=%v want %v", c.mid, got, c.want)
		}
	}
}

func TestFeedMidPrecision(t *testing.T) {
	// GOLD 4000 (8 decimals) in USDC.e 1.0 (6 decimals): 4000 * 1e6 / 1e8 = 40 quote atoms per base atom.
	if m := feedMid(4000, 1, 8, 6); m != 40 {
		t.Fatalf("mid %v want 40", m)
	}
	if m := feedMid(1, 4000, 6, 8); m != 0.025 {
		t.Fatalf("mid %v want 0.025", m)
	}
}

func TestReadFeedPrice(t *testing.T) {
	var feed map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"SEQ":{"price":0.37},"USDX":1.0}`), &feed)
	if readFeedPrice(feed, "SEQ") != 0.37 || readFeedPrice(feed, "USDX") != 1.0 || readFeedPrice(feed, "GOLD") != 0 {
		t.Fatal("feed read")
	}
}

func TestReadFeedPriceSkipsAnOldReading(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var feed map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"GOLD":{"price":4000,"as_of":1799999400},"EURX":{"price":1.1,"as_of":1799999399},"ACME":{"price":2,"as_of":1700000000,"static":true}}`), &feed)
	if readFeedPriceAt(feed, "GOLD", now) != 4000 {
		t.Fatal("a reading ten minutes old is still a price")
	}
	if readFeedPriceAt(feed, "EURX", now) != 0 {
		t.Fatal("a reading over ten minutes old reads as no price")
	}
	if readFeedPriceAt(feed, "ACME", now) != 2 {
		t.Fatal("a seeded price has no age")
	}
}

// The guard end to end in a child process: a feed that moves through the ask
// makes the maker print the reason and exit 0 for its supervisor to re-quote.
func TestFeedGuardExitsWhenFeedMovesThrough(t *testing.T) {
	if os.Getenv("FEED_GUARD_CHILD") != "" {
		feedGuard = feedGuardConfig{URL: os.Getenv("FEED_GUARD_CHILD"), BaseKey: "SEQ", QuoteKey: "USDX",
			BasePrec: 8, QuotePrec: 8, Bps: 10, Every: 50 * time.Millisecond}
		armFeedGuard(guardOffer(true, 10000, 3760), func() bool { return true }) // ask at 0.376
		time.Sleep(5 * time.Second)
		os.Exit(3) // the guard never fired
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p := 0.370 // under the ask
		if calls.Add(1) > 3 {
			p = 0.380 // the market moved through it
		}
		fmt.Fprintf(w, `{"SEQ":{"price":%v},"USDX":{"price":1.0}}`, p)
	}))
	defer srv.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestFeedGuardExitsWhenFeedMovesThrough")
	cmd.Env = append(os.Environ(), "FEED_GUARD_CHILD="+srv.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "the feed moved through the quote") || calls.Load() < 4 {
		t.Fatalf("child output (feed read %d times):\n%s", calls.Load(), out)
	}
	t.Logf("child: %s", strings.TrimSpace(string(out)))
}
