package api

import (
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

const silvr = "3333333333333333333333333333333333333333333333333333333333333333"

// GET /v1/tickers lists every traded market once, in its canonical direction,
// each entry equal to that market's own ticker plus its seven-day line.
func TestTickersEveryMarketOnce(t *testing.T) {
	mk := key(t)
	pub := func() string { r := &offer.TradeReport{}; offer.SignTradeReport(r, mk); return r.Reporter }()
	ts, _ := reportServer(t, pub)
	report := func(id, give, giveAmt, want, wantAmt string) {
		rep := &offer.TradeReport{Rail: offerstore.RailLeafMaker, ID: id, GiveAsset: give, Give: giveAmt, WantAsset: want, Want: wantAmt}
		offer.SignTradeReport(rep, mk)
		if code, out := post(t, ts.URL+"/v1/trades/report", rep); code != 200 || out["recorded"] != true {
			t.Fatalf("report %s: %d %v", id, code, out)
		}
	}
	// One market reported from both directions, and a second market written
	// quote-first: silvr sorts after usdx, so its canonical base is usdx.
	report("a", gold, "200", usdx, "90")
	report("b", usdx, "100", gold, "200")
	report("c", silvr, "10", usdx, "40")

	got := getJSON(t, ts.URL+"/v1/tickers")
	list, ok := got["tickers"].([]interface{})
	if !ok || len(list) != 2 {
		t.Fatalf("want 2 markets, got %v", got)
	}
	if _, ok := got["at"].(float64); !ok {
		t.Fatalf("no at: %v", got)
	}
	want := [][2]string{{gold, usdx}, {usdx, silvr}}
	for i, w := range want {
		e := list[i].(map[string]interface{})
		if e["base"] != w[0] || e["quote"] != w[1] {
			t.Fatalf("entry %d is %v/%v, want %s/%s", i, e["base"], e["quote"], w[0][:4], w[1][:4])
		}
		one := getJSON(t, ts.URL+"/v1/market/"+w[0]+"/"+w[1]+"/ticker")
		for _, k := range []string{"last", "last_ts", "trades_24h", "open_24h", "high_24h", "low_24h", "volume_24h", "quote_volume_24h", "change_pct_24h"} {
			if e[k] != one[k] {
				t.Fatalf("%s: tickers %v, ticker %v", k, e[k], one[k])
			}
		}
		sp, ok := e["spark_7d"].([]interface{})
		if !ok || len(sp) != sparkSteps || sp[len(sp)-1].(float64) != one["last"].(float64) {
			t.Fatalf("spark_7d %v (last %v)", e["spark_7d"], one["last"])
		}
		if _, ok := one["spark_7d"]; ok {
			t.Fatalf("the one-market ticker carries no line: %v", one)
		}
	}
	if e := list[0].(map[string]interface{}); e["trades_24h"].(float64) != 2 {
		t.Fatalf("both directions' trades are one market's: %v", e)
	}
}

func TestTickersEmptyRelay(t *testing.T) {
	ts, _ := reportServer(t)
	got := getJSON(t, ts.URL+"/v1/tickers")
	if l, ok := got["tickers"].([]interface{}); !ok || len(l) != 0 {
		t.Fatalf("an empty relay lists no market: %v", got)
	}
}

// sparkOf carries each price to the next trade and reads 0 before the first.
func TestSparkOf(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tr := []offerstore.Trade{
		{TS: now.Add(-150 * time.Minute), Price: 2},
		{TS: now.Add(-60 * time.Minute), Price: 3},
		{TS: now.Add(-60 * time.Minute), Price: 4},
	}
	got := sparkOf(tr, now, 4, time.Hour)
	want := []float64{0, 2, 4, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sparkOf %v, want %v", got, want)
		}
	}
}
