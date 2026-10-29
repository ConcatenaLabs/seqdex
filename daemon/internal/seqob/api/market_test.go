package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
	"net/http/httptest"
)

const gold = "1111111111111111111111111111111111111111111111111111111111111111"
const usdx = "2222222222222222222222222222222222222222222222222222222222222222"

func reportServer(t *testing.T, reporters ...string) (*httptest.Server, *offerstore.Store) {
	t.Helper()
	store := offerstore.New(nil)
	srv := New(store, validator.New(validator.DefaultConfig(), nil), session.NewRouter(session.Options{Deadline: time.Minute}), nil)
	srv.SetTradeReporters(reporters)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func post(t *testing.T, url string, v interface{}) (int, map[string]interface{}) {
	t.Helper()
	b, _ := json.Marshal(v)
	r, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func getJSON(t *testing.T, url string) map[string]interface{} {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTradeReportRecordedOnceFromATrustedMaker(t *testing.T) {
	mk := key(t)
	pub := func() string { r := &offer.TradeReport{}; offer.SignTradeReport(r, mk); return r.Reporter }()
	ts, _ := reportServer(t, pub)
	rep := &offer.TradeReport{Rail: offerstore.RailLeafMaker, ID: "sw1", GiveAsset: gold, Give: "200", WantAsset: usdx, Want: "90"}
	offer.SignTradeReport(rep, mk)
	if code, out := post(t, ts.URL+"/v1/trades/report", rep); code != 200 || out["recorded"] != true {
		t.Fatalf("report: %d %v", code, out)
	}
	if code, out := post(t, ts.URL+"/v1/trades/report", rep); code != 200 || out["recorded"] != false {
		t.Fatalf("repeat report: %d %v", code, out)
	}
	// Read from the other direction: one trade, usdx/gold, its taker sold usdx.
	tr := getJSON(t, ts.URL+"/v1/market/"+usdx+"/"+gold+"/trades")["trades"].([]interface{})
	if len(tr) != 1 {
		t.Fatalf("trades %v", tr)
	}
	x := tr[0].(map[string]interface{})
	if x["size"].(float64) != 90 || x["quote_size"].(float64) != 200 || x["side"] != "sell" || x["rail"] != offerstore.RailLeafMaker {
		t.Fatalf("trade %v", x)
	}
	tk := getJSON(t, ts.URL+"/v1/market/"+gold+"/"+usdx+"/ticker")
	if tk["last"].(float64) != 0.45 || tk["trades_24h"].(float64) != 1 || tk["volume_24h"].(float64) != 200 || tk["change_pct_24h"].(float64) != 0 {
		t.Fatalf("ticker %v", tk)
	}
	cd := getJSON(t, ts.URL+"/v1/market/"+gold+"/"+usdx+"/candles?interval=60")["candles"].([]interface{})
	if len(cd) != 1 || cd[0].(map[string]interface{})["c"].(float64) != 0.45 {
		t.Fatalf("candles %v", cd)
	}
}

func TestTradeReportRefusals(t *testing.T) {
	trusted, stranger := key(t), key(t)
	pub := func() string { r := &offer.TradeReport{}; offer.SignTradeReport(r, trusted); return r.Reporter }()
	ts, store := reportServer(t, pub)
	mk := func() *offer.TradeReport {
		r := &offer.TradeReport{Rail: offerstore.RailBTCGateway, ID: "b1", GiveAsset: "BTC", Give: "1000", WantAsset: usdx, Want: "5"}
		return r
	}
	// A key the relay does not trust.
	r1 := mk()
	offer.SignTradeReport(r1, stranger)
	if code, out := post(t, ts.URL+"/v1/trades/report", r1); code != 403 {
		t.Fatalf("stranger: %d %v", code, out)
	}
	// A trusted key's report altered after signing.
	r2 := mk()
	offer.SignTradeReport(r2, trusted)
	r2.Want = "50"
	if code, out := post(t, ts.URL+"/v1/trades/report", r2); code != 400 || out["message"] != "invalid trade report: trade report sig verification failed" {
		t.Fatalf("altered: %d %v", code, out)
	}
	// A rail the relay records for itself.
	r3 := mk()
	r3.Rail = offerstore.RailOnchain
	offer.SignTradeReport(r3, trusted)
	if code, _ := post(t, ts.URL+"/v1/trades/report", r3); code != 400 {
		t.Fatalf("own rail: %d", code)
	}
	if n := len(store.TradesFor(nil, 0)); n != 0 {
		t.Fatal("nil pair")
	}
	tk := getJSON(t, ts.URL+"/v1/market/BTC/"+usdx+"/ticker")
	if _, has := tk["last"]; has || tk["trades_24h"].(float64) != 0 {
		t.Fatalf("a refused report reached the ticker: %v", tk)
	}
}

// One relay's market view includes the trades of the relays it follows, each
// fill once, and drops one the followed relay retracted.
func TestFollowTradesOneMarketView(t *testing.T) {
	_, mainStore := reportServer(t)
	peerStore := offerstore.New(nil)
	peer := httptest.NewServer(New(peerStore, validator.New(validator.DefaultConfig(), nil), session.NewRouter(session.Options{Deadline: time.Minute}), nil).Handler())
	t.Cleanup(peer.Close)
	k := key(t)
	o := mkSignedOffer(t, k, "pln1") // SELL 100 gold for 45 usdx
	peerStore.RecordTradeVia(o, 100, "txP", "fill:p", offerstore.RailLightning)
	mainStore.RecordReported(usdx, 9, gold, 20, offerstore.RailLeafMaker, "m:leaf-maker:s")
	mainStore.RecordTradeVia(o, 100, "txP", "fill:p", offerstore.RailLightning) // the same fill seen here too

	srvMain := New(mainStore, validator.New(validator.DefaultConfig(), nil), session.NewRouter(session.Options{Deadline: time.Minute}), nil)
	stop := make(chan struct{})
	defer close(stop)
	srvMain.FollowTrades([]string{peer.URL}, 20*time.Millisecond, stop)
	view := httptest.NewServer(srvMain.Handler())
	t.Cleanup(view.Close)
	pair := o.GetPair()
	waitFor := func(n int) []interface{} {
		for i := 0; i < 100; i++ {
			tr := getJSON(t, view.URL+"/v1/market/"+pair.QuoteAsset+"/"+pair.BaseAsset+"/trades")["trades"].([]interface{})
			if len(tr) == n {
				return tr
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("market view never held %d trades", n)
		return nil
	}
	waitFor(1) // only the peer's fill exists in this pair (gold/usdx of mkSignedOffer)
	peerStore.RecordReported(pair.BaseAsset, 10, pair.QuoteAsset, 5, offerstore.RailBTCGateway, "b2")
	tr := waitFor(2)
	if x := tr[1].(map[string]interface{}); x["rail"] != offerstore.RailBTCGateway || x["size"].(float64) != 5 {
		t.Fatalf("followed trade read from the other direction %v", x)
	}
	peerStore.RetractTrade(pair, "txP")
	// Retracted at the peer, but the main relay recorded the same fill itself.
	waitFor(2)
	// A fill only the peer saw, then undone there by a reorg: gone from the view.
	peerStore.RecordTradeVia(o, 50, "txQ", "fill:q", offerstore.RailOnchain)
	waitFor(3)
	peerStore.RetractTrade(pair, "txQ")
	waitFor(2)
}
