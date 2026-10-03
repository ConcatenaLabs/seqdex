package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Lightning leg on a node whose channels hold several assets: every hold and
// every invoice names the leg's asset, a hold accepted in another asset is
// refused before anything settles, and a bare-hash payment asks the node's signer
// to approve it before any HTLC is offered. These run against scriptCLN, a fake
// lightning-rpc socket that answers each method from a table and records every
// call, so the tests can assert what was (and was not) sent.

const (
	gold = "1111111111111111111111111111111111111111111111111111111111111111"
	silv = "2222222222222222222222222222222222222222222222222222222222222222"
)

type rpcCall struct {
	method string
	params map[string]interface{}
}

type scriptCLN struct {
	path string
	ln   net.Listener
	// answer gives the result or the JSON-RPC error for a call; nil, nil is an
	// empty result.
	answer func(method string, params map[string]interface{}) (interface{}, *rpcErr)

	mu    sync.Mutex
	calls []rpcCall
}

func startScriptCLN(t *testing.T, answer func(string, map[string]interface{}) (interface{}, *rpcErr)) *scriptCLN {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lightning-rpc")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &scriptCLN{path: path, ln: ln, answer: answer}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(path) })
	return f
}

func (f *scriptCLN) handle(conn net.Conn) {
	defer conn.Close()
	var req struct {
		ID     json.RawMessage        `json:"id"`
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, rpcCall{req.Method, req.Params})
	f.mu.Unlock()
	res, rerr := f.answer(req.Method, req.Params)
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		resp["error"] = map[string]interface{}{"code": rerr.Code, "message": rerr.Message}
	} else if res != nil {
		resp["result"] = res
	} else {
		resp["result"] = map[string]interface{}{}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func (f *scriptCLN) called(method string) []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]interface{}
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.params)
		}
	}
	return out
}

func (f *scriptCLN) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func on(network string) func(string, map[string]interface{}) (interface{}, *rpcErr) {
	return func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		switch m {
		case "getinfo":
			return map[string]interface{}{"id": "02aa", "network": network, "blockheight": 100}, nil
		case "holdinvoice":
			r := map[string]interface{}{"payment_hash": p["payment_hash"], "state": "waiting"}
			if a, ok := p["asset"]; ok {
				r["asset"] = a
			}
			return r, nil
		case "invoice":
			return map[string]interface{}{"bolt11": "lnbc1", "payment_hash": ""}, nil
		}
		return nil, nil
	}
}

var h32 = func() []byte { s := sha256.Sum256([]byte("p")); return s[:] }()

// On a Sequentia node a leg that names no asset makes no hold and no invoice:
// the node's own choice of asset is never taken as the swap's.
func TestHoldWithoutAssetIsNeverCreatedOnSequentia(t *testing.T) {
	f := startScriptCLN(t, on("sequentia-regtest"))
	leg := NewCLNLNLeg(f.path)
	if _, err := leg.CreateHoldInvoice(h32, 1000, 0, "l", "d"); !errors.Is(err, ErrLNHoldAsset) {
		t.Fatalf("CreateHoldInvoice with no asset on Sequentia: err = %v, want ErrLNHoldAsset", err)
	}
	if _, err := leg.CreateInvoice(make([]byte, 32), 1000, 0, "l", "d"); !errors.Is(err, ErrLNHoldAsset) {
		t.Fatalf("CreateInvoice with no asset on Sequentia: err = %v, want ErrLNHoldAsset", err)
	}
	if n := len(f.called("holdinvoice")) + len(f.called("invoice")); n != 0 {
		t.Fatalf("%d hold/invoice calls reached the node; want none", n)
	}
}

// An asset leg names its asset on the hold and on the invoice, the Sequence
// token's id like any other.
func TestHoldAndInvoiceNameTheLegsAsset(t *testing.T) {
	f := startScriptCLN(t, on("sequentia-regtest"))
	leg := NewCLNAssetLNLeg(f.path, strings.ToUpper(gold))
	if _, err := leg.CreateHoldInvoice(h32, 5000, 0, "l", "d"); err != nil {
		t.Fatalf("CreateHoldInvoice: %v", err)
	}
	if got := f.called("holdinvoice")[0]["asset"]; got != gold {
		t.Fatalf("holdinvoice asset = %v, want %s", got, gold)
	}
	_, _ = leg.CreateInvoice(make([]byte, 32), 5000, 0, "l", "d") // the fake's hash does not match; only the params matter
	inv := f.called("invoice")
	if len(inv) == 0 || inv[0]["asset"] != gold {
		t.Fatalf("invoice params = %v, want asset %s", inv, gold)
	}
}

// A plugin that does not register the hold in the asset it was asked for (one
// that predates assets and holds anything) is not trusted: the hold is cancelled
// and the call fails.
func TestHoldFromAPluginWithoutAssetsIsCancelled(t *testing.T) {
	f := startScriptCLN(t, func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		if m == "holdinvoice" {
			return map[string]interface{}{"payment_hash": p["payment_hash"], "state": "waiting"}, nil
		}
		return on("sequentia-regtest")(m, p)
	})
	_, err := NewCLNAssetLNLeg(f.path, gold).CreateHoldInvoice(h32, 5000, 0, "l", "d")
	if !errors.Is(err, ErrLNHoldAsset) {
		t.Fatalf("err = %v, want ErrLNHoldAsset", err)
	}
	if len(f.called("holdinvoicecancel")) != 1 {
		t.Fatalf("the untrusted hold was not cancelled: calls %v", f.order())
	}
}

func heldIn(network string, asset interface{}) func(string, map[string]interface{}) (interface{}, *rpcErr) {
	return func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		if m == "holdinvoicewait" || m == "holdinvoicelookup" {
			r := map[string]interface{}{"state": "accepted", "amount_msat": 5000, "received_msat": 5000,
				"cltv_expiry": 300, "blockheight": 100}
			if asset != nil {
				r["asset"] = asset
			}
			return r, nil
		}
		return on(network)(m, p)
	}
}

// A hold accepted in another asset, or naming none on a network with assets, is
// refused: the caller never reaches the settle, so the preimage stays withheld.
func TestWaitHeldRefusesAHoldInAnotherAsset(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asset interface{}
	}{{"in SILV", silv}, {"in no named asset", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			f := startScriptCLN(t, heldIn("sequentia-regtest", tc.asset))
			_, err := NewCLNAssetLNLeg(f.path, gold).WaitHeldInfo(h32, 5*time.Second)
			if !errors.Is(err, ErrLNHoldAsset) {
				t.Fatalf("err = %v, want ErrLNHoldAsset", err)
			}
			if len(f.called("holdinvoicesettle")) != 0 {
				t.Fatal("a hold in the wrong asset was settled")
			}
		})
	}
	f := startScriptCLN(t, heldIn("sequentia-regtest", strings.ToUpper(gold)))
	info, err := NewCLNAssetLNLeg(f.path, gold).WaitHeldInfo(h32, 5*time.Second)
	if err != nil || info.Asset != gold || info.ReceivedMsat != 5000 {
		t.Fatalf("hold in the leg's asset: info %+v, err %v", info, err)
	}
}

// On a Bitcoin Lightning node there are no assets: the hold names none, and a
// hold that claims one is refused.
func TestBitcoinLegHoldsWithoutAsset(t *testing.T) {
	f := startScriptCLN(t, heldIn("testnet4", nil))
	leg := NewCLNLNLeg(f.path)
	if _, err := leg.CreateHoldInvoice(h32, 5000, 0, "l", "d"); err != nil {
		t.Fatalf("CreateHoldInvoice on testnet4: %v", err)
	}
	if _, ok := f.called("holdinvoice")[0]["asset"]; ok {
		t.Fatal("a Bitcoin hold named an asset")
	}
	if _, err := leg.WaitHeldInfo(h32, 5*time.Second); err != nil {
		t.Fatalf("WaitHeldInfo on testnet4: %v", err)
	}
	g := startScriptCLN(t, heldIn("testnet4", gold))
	if _, err := NewCLNLNLeg(g.path).WaitHeldInfo(h32, 5*time.Second); !errors.Is(err, ErrLNHoldAsset) {
		t.Fatalf("a Bitcoin hold reporting an asset: err = %v, want ErrLNHoldAsset", err)
	}
}

func paying(declineKeysend bool) func(string, map[string]interface{}) (interface{}, *rpcErr) {
	return func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		switch m {
		case "preapprovekeysend":
			if declineKeysend {
				return nil, &rpcErr{Code: 214, Message: "keysend was declined"}
			}
			return map[string]interface{}{}, nil
		case "getroute":
			return map[string]interface{}{"route": []map[string]interface{}{{"id": "03bb", "channel": "1x1x0",
				"direction": 0, "amount_msat": 5000, "delay": 18, "style": "tlv"}}}, nil
		case "sendpay":
			return map[string]interface{}{"status": "pending"}, nil
		case "waitsendpay":
			return map[string]interface{}{"status": "complete",
				"payment_preimage": "70" + strings.Repeat("00", 31)}, nil
		}
		return on("sequentia-regtest")(m, p)
	}
}

// A bare-hash payment asks the signer first, with the destination, hash and
// amount it will pay; sendpay follows only an approval.
func TestPayHashPreapprovesBeforeSendpay(t *testing.T) {
	f := startScriptCLN(t, paying(false))
	_, _ = NewCLNAssetLNLeg(f.path, gold).PayHash("03bb", h32, 5000, 18, nil) // the fake's preimage does not hash to h32
	order := strings.Join(f.order(), ",")
	if !strings.Contains(order, "preapprovekeysend,sendpay") {
		t.Fatalf("calls %s: want preapprovekeysend immediately before sendpay", order)
	}
	pa := f.called("preapprovekeysend")[0]
	if pa["destination"] != "03bb" || pa["payment_hash"] != hex.EncodeToString(h32) || pa["amount_msat"].(float64) != 5000 {
		t.Fatalf("preapprovekeysend params %v", pa)
	}
}

// A declined payment offers no HTLC at all, and says so.
func TestPayHashDeclinedSendsNothing(t *testing.T) {
	f := startScriptCLN(t, paying(true))
	_, err := NewCLNAssetLNLeg(f.path, gold).PayHash("03bb", h32, 5000, 18, nil)
	if !errors.Is(err, ErrLNPayDeclined) {
		t.Fatalf("err = %v, want ErrLNPayDeclined", err)
	}
	if n := len(f.called("sendpay")); n != 0 {
		t.Fatalf("sendpay called %d times after a decline", n)
	}
	t.Logf("declined: %v", err)
}

// `pay` declined by the signer is final: no direct-hop fallback (which would only
// be declined again), no wait for a payment that never started.
func TestPayDeclinedDoesNotFallBack(t *testing.T) {
	f := startScriptCLN(t, func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		switch m {
		case "decode":
			return map[string]interface{}{"type": "bolt11 invoice", "valid": true, "payment_hash": hex.EncodeToString(h32),
				"amount_msat": 5000, "payee": "03bb", "payment_secret": strings.Repeat("11", 32)}, nil
		case "pay":
			return nil, &rpcErr{Code: 213, Message: "invoice was declined"}
		}
		return paying(false)(m, p)
	})
	_, err := NewCLNAssetLNLeg(f.path, gold).Pay("lnbc1", h32, 5000)
	if !errors.Is(err, ErrLNPayDeclined) {
		t.Fatalf("err = %v, want ErrLNPayDeclined", err)
	}
	for _, m := range []string{"sendpay", "listpays", "preapprovekeysend"} {
		if n := len(f.called(m)); n != 0 {
			t.Fatalf("%s called %d times after pay was declined", m, n)
		}
	}
}

// The direct-hop fallback never pays over a channel that names another asset,
// even when that channel comes first and has the room.
func TestDirectHopKeepsToTheLegsAsset(t *testing.T) {
	f := startScriptCLN(t, func(m string, p map[string]interface{}) (interface{}, *rpcErr) {
		if m == "listpeerchannels" {
			ch := func(scid, asset string) map[string]interface{} {
				return map[string]interface{}{"peer_id": "03bb", "state": "CHANNELD_NORMAL", "short_channel_id": scid,
					"direction": 0, "spendable_msat": 10000000, "channel_asset": asset}
			}
			return map[string]interface{}{"channels": []map[string]interface{}{ch("1x1x0", gold), ch("2x1x0", silv)}}, nil
		}
		return on("sequentia-regtest")(m, p)
	})
	hop, err := NewCLNAssetLNLeg(f.path, silv).directHop("03bb", 5000, 18)
	if err != nil || !strings.Contains(string(hop), `"2x1x0"`) {
		t.Fatalf("SILV leg direct hop = %s (err %v), want the SILV channel 2x1x0", hop, err)
	}
	if _, err := NewCLNAssetLNLeg(f.path, strings.Repeat("33", 32)).directHop("03bb", 5000, 18); err == nil {
		t.Fatal("a leg in a third asset found a direct hop over another asset's channel")
	}
}
