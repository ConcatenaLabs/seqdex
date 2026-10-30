package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpendableIn(t *testing.T) {
	j := []byte(`{"channels":[
	 {"state":"CHANNELD_NORMAL","channel_asset":"aa11","spendable_msat":"5000000msat"},
	 {"state":"CHANNELD_NORMAL","channel_asset":"aa11","spendable_msat":2000000},
	 {"state":"CHANNELD_AWAITING_LOCKIN","channel_asset":"aa11","spendable_msat":9000000},
	 {"state":"CHANNELD_NORMAL","channel_asset":"bb22","spendable_msat":7000000},
	 {"state":"CHANNELD_NORMAL","spendable_msat":3000000}]}`)
	if got, _ := spendableIn(j, "aa11"); got != 7000 {
		t.Fatalf("an asset's usable channels only: %d", got)
	}
	if got, _ := spendableIn(j, ""); got != 3000 {
		t.Fatalf("bitcoin: the channels with no asset: %d", got)
	}
}

func TestPriceWithin(t *testing.T) {
	failedAsk := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 1000, 2000), tBase, tQuote) // 2.0
	near := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 1000, 2050), tBase, tQuote)        // +2.5%
	far := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 1000, 2200), tBase, tQuote)         // +10%
	if !priceWithin(near, failedAsk, 300) || priceWithin(far, failedAsk, 300) {
		t.Fatal("a buy pays at most 300 bps more")
	}
	failedBid := norm(mkOffer(FamCovenant, SideBid, tBase, tQuote, 1000, 2000), tBase, tQuote)
	lower := norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 1000, 1960), tBase, tQuote) // -2%
	much := norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 1000, 1800), tBase, tQuote)  // -10%
	if !priceWithin(lower, failedBid, 300) || priceWithin(much, failedBid, 300) {
		t.Fatal("a sell gets at most 300 bps less")
	}
}

// A failed second leg (an ask the crosser was buying from) is completed with the
// next-best asks within the bound: the first fails its preflight, the next runs
// and fails, two more fill it (the last sliced to what is left), and an ask
// outside the bound and the bids are never touched.
func TestCompleteLeg(t *testing.T) {
	failed := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 1000, 2000), tBase, tQuote)
	spent := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 600, 1200), tBase, tQuote)
	broken := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 600, 1205), tBase, tQuote)
	a := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 600, 1210), tBase, tQuote)
	b := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 600, 1220), tBase, tQuote)
	far := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 5000, 11000), tBase, tQuote)
	bid := norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 5000, 10000), tBase, tQuote)
	ran := map[string]uint64{}
	pre := func(n *NormOrder, take uint64) string {
		if n == spent {
			return "its covenant coin is spent"
		}
		return ""
	}
	run := func(n *NormOrder, take uint64) LegResult {
		ran[n.ID()] = take
		if n == broken {
			return LegResult{Err: "maker is not connected"}
		}
		return LegResult{Settled: true}
	}
	done, lines := completeLegWith([]*NormOrder{failed, spent, broken, a, b, far, bid}, failed, 1000, 300, execAll, pre, run)
	if done != 1000 {
		t.Fatalf("completed %d of 1000: %v", done, lines)
	}
	if ran[a.ID()] != 600 || ran[b.ID()] != 400 {
		t.Fatalf("the next-best asks fill it, the last sliced: %v", ran)
	}
	if _, ok := ran[failed.ID()]; ok {
		t.Fatal("the failed offer is not taken again")
	}
	if _, ok := ran[far.ID()]; ok {
		t.Fatal("an ask outside the bound is not taken")
	}
	if _, ok := ran[bid.ID()]; ok {
		t.Fatal("the other side is not taken")
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "its covenant coin is spent") || !strings.Contains(all, "maker is not connected") {
		t.Fatalf("each skip and failure is said: %s", all)
	}
	// nothing within the bound: nothing taken, the drift stays booked
	if d, _ := completeLegWith([]*NormOrder{failed, far, bid}, failed, 1000, 300, execAll, pre, run); d != 0 {
		t.Fatalf("nothing within the bound: %d", d)
	}
}

// A leg spending from the crosser's node wallet fails its preflight while that
// wallet is not loaded, and passes once it is.
func TestPreflightWalletLoaded(t *testing.T) {
	loaded := []string{"other"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&q)
		switch q.Method {
		case "listwallets":
			b, _ := json.Marshal(loaded)
			fmt.Fprintf(w, `{"result":%s,"error":null}`, b)
		case "gettxout":
			fmt.Fprint(w, `{"result":{"confirmations":3},"error":null}`)
		}
	}))
	defer srv.Close()
	e := &Executor{Caps: Caps{SeqRPC: srv.URL, SeqWallet: "lsp-inventory"}}
	cov := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 1000, 2000), tBase, tQuote)
	if why := e.Preflight(cov, 1000); !strings.Contains(why, "lsp-inventory is not loaded") {
		t.Fatalf("an unloaded wallet fails the preflight: %q", why)
	}
	loaded = append(loaded, "lsp-inventory")
	if why := e.Preflight(cov, 1000); why != "" {
		t.Fatalf("loaded, and the coin unspent: %q", why)
	}
}
