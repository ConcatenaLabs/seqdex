package xchain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// rawTxServer answers getrawtransaction with the given verbose result.
func rawTxServer(t *testing.T, result map[string]interface{}) *RPC {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "getrawtransaction" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": nil,
				"error": map[string]interface{}{"code": -32601, "message": "not scripted: " + req.Method}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	return NewRPC(host, port, "u", "p")
}

// A transaction whose block a reorg disconnected keeps that block in the
// txindex, and the node reports it with 0 confirmations: it is not mined.
func TestBlockHashOfTxRefusesABlockOffTheActiveChain(t *testing.T) {
	stale := NewChain(rawTxServer(t, map[string]interface{}{"txid": "aa", "blockhash": "bb", "confirmations": 0}), "")
	if bh, err := stale.BlockHashOfTx("aa"); err == nil || bh != "" {
		t.Fatalf("a block off the active chain was reported as the tx's: %q %v", bh, err)
	}
	noConf := NewChain(rawTxServer(t, map[string]interface{}{"txid": "aa", "blockhash": "bb"}), "")
	if bh, err := noConf.BlockHashOfTx("aa"); err == nil || bh != "" {
		t.Fatalf("a block the node does not place was reported as the tx's: %q %v", bh, err)
	}
	mined := NewChain(rawTxServer(t, map[string]interface{}{"txid": "aa", "blockhash": "bb", "confirmations": 2}), "")
	if bh, err := mined.BlockHashOfTx("aa"); err != nil || bh != "bb" {
		t.Fatalf("a mined tx: %q %v", bh, err)
	}
	pool := NewChain(rawTxServer(t, map[string]interface{}{"txid": "aa"}), "")
	if bh, err := pool.BlockHashOfTx("aa"); err == nil || bh != "" {
		t.Fatalf("a mempool tx: %q %v", bh, err)
	}
}
