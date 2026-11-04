package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// fakeNode is a lightning-rpc socket that records each method and its params and answers
// every invoice with a fixed bolt11.
type fakeNode struct {
	mu    sync.Mutex
	calls []map[string]interface{}
}

func startFakeNode(t *testing.T, name string) (string, *fakeNode) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeNode{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req struct {
					ID     json.RawMessage        `json:"id"`
					Method string                 `json:"method"`
					Params map[string]interface{} `json:"params"`
				}
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				f.mu.Lock()
				f.calls = append(f.calls, map[string]interface{}{"method": req.Method, "params": req.Params})
				f.mu.Unlock()
				res := map[string]interface{}{}
				switch req.Method {
				case "invoice":
					pre, _ := hex.DecodeString(fmt.Sprint(req.Params["preimage"]))
					h := sha256.Sum256(pre)
					res = map[string]interface{}{"bolt11": "lnbcrt1" + name, "payment_hash": hex.EncodeToString(h[:])}
				case "getinfo":
					network := "liquid-regtest"
					if name == "btc" {
						network = "testnet4"
					}
					res = map[string]interface{}{"id": "02" + name, "network": network}
				}
				_ = json.NewEncoder(c).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": res})
			}(c)
		}
	}()
	return path, f
}

func (f *fakeNode) invoices() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]interface{}
	for _, c := range f.calls {
		if c["method"] == "invoice" {
			out = append(out, c["params"].(map[string]interface{}))
		}
	}
	return out
}

const (
	usdx = "2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
	eurx = "e39685e718516156679088d9400d11a1eb82bf7cc27c5b9f5a614b8c91246d13"
)

// A taker selling USDX for EURX receives EURX: its invoice is made on the node -ln-socket names
// (the taker's EURX node) and in EURX, never on the USDX node (issue: the EURX invoice was asked
// of the USDX node, which refused it).
func TestSellCounterInvoiceOnQuoteNode(t *testing.T) {
	baseSock, base := startFakeNode(t, "usdx")
	quoteSock, quote := startFakeNode(t, "eurx")
	swap := pureLNTakerSwap(baseSock, quoteSock, usdx, eurx)
	if _, _, err := swap.PrepareTakerSell(make([]byte, 32), 7_000_000); err != nil {
		t.Fatalf("prepare sell: %v", err)
	}
	if n := len(base.invoices()); n != 0 {
		t.Fatalf("the USDX node was asked for %d invoice(s): %v", n, base.invoices())
	}
	inv := quote.invoices()
	if len(inv) != 1 || inv[0]["asset"] != eurx {
		t.Fatalf("the EURX node made %v, want one invoice in EURX", inv)
	}
}

// A taker buying USDX with EURX receives USDX: the invoice is on the USDX node, in USDX.
func TestBuyAssetInvoiceOnBaseNode(t *testing.T) {
	baseSock, base := startFakeNode(t, "usdx")
	quoteSock, quote := startFakeNode(t, "eurx")
	swap := pureLNTakerSwap(baseSock, quoteSock, usdx, eurx)
	if _, _, err := swap.PrepareTakerBuy(make([]byte, 32), 7_000_000); err != nil {
		t.Fatalf("prepare buy: %v", err)
	}
	if n := len(quote.invoices()); n != 0 {
		t.Fatalf("the EURX node was asked for %d invoice(s)", n)
	}
	inv := base.invoices()
	if len(inv) != 1 || inv[0]["asset"] != usdx {
		t.Fatalf("the USDX node made %v, want one invoice in USDX", inv)
	}
}

// A BTC quote: the counter invoice is on the -ln-socket node and names no asset.
func TestSellForBTCInvoiceOnBTCNode(t *testing.T) {
	baseSock, base := startFakeNode(t, "usdx")
	btcSock, btc := startFakeNode(t, "btc")
	swap := pureLNTakerSwap(baseSock, btcSock, usdx, "")
	if _, _, err := swap.PrepareTakerSell(make([]byte, 32), 10_000_000); err != nil {
		t.Fatalf("prepare sell: %v", err)
	}
	if n := len(base.invoices()); n != 0 {
		t.Fatalf("the USDX node was asked for %d invoice(s)", n)
	}
	inv := btc.invoices()
	if len(inv) != 1 {
		t.Fatalf("the BTC node made %d invoices", len(inv))
	}
	if a, ok := inv[0]["asset"]; ok {
		t.Fatalf("a BTC invoice names asset %v", a)
	}
}
