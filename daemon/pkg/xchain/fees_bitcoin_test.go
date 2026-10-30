package xchain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

// feeNode answers estimatesmartfee and getrawmempool from fixed values.
func feeNode(t *testing.T, estimateBTCkvB float64, mempool map[string]interface{}) *BitcoinChain {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var res interface{}
		switch req.Method {
		case "estimatesmartfee":
			res = map[string]interface{}{"feerate": estimateBTCkvB, "blocks": 2}
		case "getrawmempool":
			res = mempool
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": res, "error": nil})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, port, _ := strings.Cut(u.Host, ":")
	p, _ := strconv.Atoi(port)
	return NewBitcoinChain(NewRPC(host, p, "u", "p"), "w", &chaincfg.RegressionNetParams)
}

func TestGoingRate(t *testing.T) {
	// The floor when the node knows nothing.
	c := feeNode(t, 0, map[string]interface{}{})
	c.SetFeePolicy(FeePolicy{Floor: 30, Cap: 500})
	if r, why := c.GoingRate(); r != 30 {
		t.Fatalf("%v (%s), want the floor 30", r, why)
	}
	// The node's estimate above the floor: 0.0004 BTC/kvB is 40 sat/vB.
	c = feeNode(t, 0.0004, map[string]interface{}{})
	c.SetFeePolicy(FeePolicy{Floor: 30, Cap: 500})
	if r, _ := c.GoingRate(); r != 40 {
		t.Fatalf("%v, want the estimate 40", r)
	}
	// A minimum set by the operator.
	c.SetFeeRate(55)
	if r, _ := c.GoingRate(); r != 55 {
		t.Fatalf("%v, want the set minimum 55", r)
	}
	// A full block in the mempool at 60 sat/vB and more: one above its last.
	m := map[string]interface{}{}
	for i := 0; i < 20; i++ {
		m[strconv.Itoa(i)] = map[string]interface{}{"vsize": 50000, "fees": map[string]interface{}{"base": 0.03}} // 60 sat/vB
	}
	c = feeNode(t, 0, m)
	c.SetFeePolicy(FeePolicy{Floor: 1, Cap: 500})
	if r, _ := c.GoingRate(); r != 61 {
		t.Fatalf("%v, want the next block's 61", r)
	}
	// Capped.
	c.SetFeePolicy(FeePolicy{Floor: 1, Cap: 50})
	if r, why := c.GoingRate(); r != 50 || !strings.Contains(why, "capped") {
		t.Fatalf("%v (%s), want the cap 50", r, why)
	}
	// testnet4's default policy: a floor of 30 and the public mempool.
	if p := DefaultFeePolicy(testNet4Params()); p.Floor != 30 || p.API == "" {
		t.Fatalf("testnet4 policy %+v", p)
	}
	if p := DefaultFeePolicy(&chaincfg.MainNetParams); p.API != "" {
		t.Fatalf("mainnet reads no public mempool by default: %+v", p)
	}
}

func TestSpendFee(t *testing.T) {
	if f := spendFee(1000, 30, 300, 1000000); f != 9000 {
		t.Fatalf("%d, want 30 x 300", f)
	}
	if f := spendFee(20000, 30, 300, 1000000); f != 20000 {
		t.Fatalf("%d, want the caller's larger fee", f)
	}
	if f := spendFee(1000, 30, 300, 10000); f != 5000 {
		t.Fatalf("%d, want half the output", f)
	}
}
