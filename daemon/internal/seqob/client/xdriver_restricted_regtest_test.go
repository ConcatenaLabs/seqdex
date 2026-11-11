package client

// The restricted swap's drivers against real nodes: a bitcoind (the BTC leg,
// and the anchor's parent), a Sequentia node anchored to it with -par=1, and
// openampd, the asset's policy server. pkg/restricted/testdata/start-regtest.sh
// brings them up and writes the env file this test reads:
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run TestRestrictedSwapRegtest -v -timeout 30m
//
// Skipped without it.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type rtEnv struct {
	btc, seq      *xchain.RPC
	policy, token string
	feeAsset      string
	evidence      map[string]interface{}
	evMu          sync.Mutex
	mineMu        sync.Mutex
}

func (e *rtEnv) rec(t *testing.T, k string, v interface{}) {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	e.evidence[k] = v
	t.Logf("%s: %v", k, v)
	if p := os.Getenv("RESTRICTED_E2E_EVIDENCE"); p != "" {
		b, _ := json.MarshalIndent(e.evidence, "", " ")
		_ = os.WriteFile(p, b, 0o644)
	}
}

func rpcFrom(t *testing.T, spec string) *xchain.RPC {
	p := strings.SplitN(spec, ":", 4)
	if len(p) != 4 {
		t.Fatalf("rpc spec %q", spec)
	}
	port, _ := strconv.Atoi(p[1])
	return xchain.NewRPC(p[0], port, p[2], p[3])
}

func loadRtEnv(t *testing.T) *rtEnv {
	path := os.Getenv("RESTRICTED_E2E_ENV")
	if path == "" {
		t.Skip("RESTRICTED_E2E_ENV names no regtest run (pkg/restricted/testdata/start-regtest.sh)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[k] = v
		}
	}
	return &rtEnv{btc: rpcFrom(t, kv["RESTRICTED_BTC_RPC"]), seq: rpcFrom(t, kv["RESTRICTED_SEQ_RPC"]),
		policy: kv["RESTRICTED_POLICY"], token: kv["RESTRICTED_ISSUER_TOKEN"], feeAsset: kv["RESTRICTED_FEE_ASSET"],
		evidence: map[string]interface{}{}}
}

func (e *rtEnv) api(t *testing.T, method, path string, body interface{}, out interface{}) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.policy+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1/issuer/") {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *rtEnv) account(t *testing.T) (string, *btcec.PrivateKey) {
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		AID string `json:"aid"`
	}
	x := restricted.XOnly(k)
	e.api(t, "POST", "/v1/users", map[string]interface{}{"pubkeys": []string{hex.EncodeToString(x[:])}}, &r)
	return r.AID, k
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mine one block on each chain: Bitcoin first, so the Sequentia block anchors it.
func (e *rtEnv) mine(t *testing.T) {
	e.mineMu.Lock()
	defer e.mineMu.Unlock()
	var a string
	must(t, e.btc.WithWallet("w").Call(&a, "getnewaddress"))
	var h []string
	must(t, e.btc.WithWallet("w").Call(&h, "generatetoaddress", 1, a))
	time.Sleep(1500 * time.Millisecond) // the anchor poll
	must(t, e.seq.WithWallet("w").Call(&a, "getnewaddress"))
	must(t, e.seq.WithWallet("w").Call(&h, "generatetoaddress", 1, a))
}

func (e *rtEnv) miner(t *testing.T, ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			e.mine(t)
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

func (e *rtEnv) wallet(t *testing.T, rpc *xchain.RPC, name string) {
	var r interface{}
	if err := rpc.CallNamed(&r, "createwallet", map[string]interface{}{"wallet_name": name, "descriptors": true}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
}

// pipe is an in-memory courier between two Crypters.
func pipe(t *testing.T) (tSend XcSend, tRecv XcRecv, mIn chan []byte, mSend XcSend, takerC, makerC *Crypter) {
	tk, _ := btcec.NewPrivateKey()
	mk, _ := btcec.NewPrivateKey()
	takerC, err := NewCrypter(tk, mk.PubKey())
	must(t, err)
	makerC, err = NewCrypter(mk, tk.PubKey())
	must(t, err)
	toMaker, toTaker := make(chan []byte, 16), make(chan []byte, 16)
	tSend = func(b []byte) error { toMaker <- b; return nil }
	tRecv = chanRecv(toTaker)
	mSend = func(b []byte) error { toTaker <- b; return nil }
	return tSend, tRecv, toMaker, mSend, takerC, makerC
}

func btcMTP(rpc *xchain.RPC) func() (int64, error) {
	return func() (int64, error) {
		var i struct {
			MedianTime int64 `json:"mediantime"`
		}
		return i.MedianTime, rpc.Call(&i, "getblockchaininfo")
	}
}

type rtSide struct {
	party *restricted.Party
	btc   *xchain.BitcoinChain
	seq   *xchain.Chain
}

func (e *rtEnv) side(t *testing.T, name, aid string, key *btcec.PrivateKey, asset string) *rtSide {
	name = fmt.Sprintf("ra3-%s-%d", name, time.Now().UnixNano()) // a fresh descriptor wallet each run
	e.wallet(t, e.seq, name)
	e.wallet(t, e.btc, name)
	seq := xchain.NewChain(e.seq, name)
	btc := xchain.NewBitcoinChain(e.btc, name, &chaincfg.RegressionNetParams)
	// The fee asset for pots and fees, and bitcoin for the BTC leg.
	var addr, txid string
	must(t, e.seq.WithWallet(name).Call(&addr, "getnewaddress"))
	must(t, e.seq.WithWallet("w").CallNamed(&txid, "sendtoaddress", map[string]interface{}{
		"address": addr, "amount": 50, "assetlabel": e.feeAsset, "fee_asset_label": "bitcoin"}))
	must(t, e.btc.WithWallet(name).Call(&addr, "getnewaddress"))
	must(t, e.btc.WithWallet("w").Call(&txid, "sendtoaddress", addr, 5))
	return &rtSide{party: &restricted.Party{Node: &restricted.Node{Chain: seq}, Policy: restricted.NewPolicy(e.policy),
		AID: aid, Key: key, Asset: asset}, btc: btc, seq: seq}
}

func (s *rtSide) swap(e *rtEnv, c *Crypter, value, sats uint64, onUpdate func(*RestrictedResult)) *RestrictedSwap {
	return &RestrictedSwap{Party: s.party, Crypter: c, BtcMedianTime: btcMTP(e.btc),
		Value: value, BtcAmount: sats, PotAsset: e.feeAsset, PotAmount: 20000, RfaPerVb: 2, BtcFee: 2000, MinBTCConf: 1,
		Timing: XcTiming{Poll: time.Second}, OnUpdate: onUpdate,
		Log: func(f string, a ...interface{}) { fmt.Printf("    "+f+"\n", a...) }}
}

func (s *rtSide) ops(secretOrHash []byte, isSecret bool) *LiveXcOps {
	hl := xchain.NewHashLockFromHash(secretOrHash)
	if isSecret {
		hl = xchain.NewHashLock(secretOrHash)
	}
	return &LiveXcOps{Swap: xchain.NewSwapBitcoin(s.btc, s.seq, hl), BTC: s.btc, SEQ: s.seq}
}

func (s *rtSide) newOps() func([]byte) (XcOps, error) {
	return func(h []byte) (XcOps, error) { return s.ops(h, false), nil }
}

// holdingReaches waits (up to a minute of blocks) for the enclave to hold want.
func (s *rtSide) holdingReaches(t *testing.T, want uint64) uint64 {
	var got uint64
	for i := 0; i < 60; i++ {
		if got = s.holding(t); got == want {
			return got
		}
		time.Sleep(time.Second)
	}
	return got
}

func (s *rtSide) holding(t *testing.T) uint64 {
	spk, _, _, err := s.party.Enclave()
	must(t, err)
	cs, err := s.party.Node.EnclaveCoins(spk, s.party.Asset)
	must(t, err)
	var n uint64
	for _, c := range cs {
		n += c.Value
	}
	return n
}

func TestRestrictedSwapRegtest(t *testing.T) {
	e := loadRtEnv(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	e.mine(t) // the policy server's fee coins confirmed
	makerAID, makerKey := e.account(t)
	takerAID, takerKey := e.account(t)
	issuerAID, _ := e.account(t)
	var issued struct {
		Asset string `json:"asset"`
	}
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "Restricted Swap Bond", "ticker": "RSB",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	asset := issued.Asset
	e.rec(t, "asset", asset)
	maker := e.side(t, "maker", makerAID, makerKey, asset)
	taker := e.side(t, "taker", takerAID, takerKey, asset)
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	e.miner(t, ctx)

	// -- 1. the taker buys 300 for 100,000 sats -----------------------------------
	t.Run("buy", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		refund, _ := xchain.NewKey()
		ts := taker.swap(e, tc, 300, 100000, nil)
		ts.Ops = taker.ops(secret, true)
		ms := maker.swap(e, mc, 300, 100000, nil)
		var mres *RestrictedResult
		var merr error
		done := make(chan struct{})
		go func() { mres, merr = RunMakerRestrictedSell(ms, maker.newOps(), mIn, mSend); close(done) }()
		tres, err := RunTakerRestrictedBuy(ts, makerAID, secret, refund, tSend, tRecv)
		if err != nil {
			t.Fatalf("taker: %v", err)
		}
		<-done
		if merr != nil {
			t.Fatalf("maker: %v", merr)
		}
		if got := taker.holdingReaches(t, 300); got != 300 {
			t.Fatalf("the taker's enclave holds %d, want 300", got)
		}
		if mres.BtcClaimTxid == "" || mres.Secret != hex.EncodeToString(secret) {
			t.Fatalf("the maker did not claim the BTC with the taker's secret: %+v", mres)
		}
		e.rec(t, "buy", map[string]interface{}{"lock": mres.Locked.Txid, "t_seq": mres.TSeq, "t_btc": mres.TBtc,
			"t_btc_minus_t_seq_h": float64(int64(mres.TBtc)-int64(mres.TSeq)) / 3600, "claim": tres.SeqClaimTxid,
			"btc_claim": mres.BtcClaimTxid, "taker_holds": 300})
	})

	// -- 2. the taker sells 300 back for 90,000 sats ------------------------------
	t.Run("sell", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		before := maker.holding(t)
		ts := taker.swap(e, tc, 300, 90000, nil)
		ts.Ops = taker.ops(make([]byte, 32), false) // rebound below once the hash is known
		ms := maker.swap(e, mc, 300, 90000, nil)
		var mres *RestrictedResult
		var merr error
		done := make(chan struct{})
		go func() { mres, merr = RunMakerRestrictedBuy(ms, maker.newOps(), mIn, mSend); close(done) }()
		// The taker's BTC engine needs the maker's hash: wrap the recv to rebind
		// the ops when btc_leg_locked arrives.
		rebind := func(timeout time.Duration) ([]byte, error) {
			b, err := tRecv(timeout)
			if err == nil {
				if m, oerr := OpenXcMsg(b, tc); oerr == nil && m.Type == XcBtcLegLocked {
					if h, herr := hex.DecodeString(m.HashH); herr == nil {
						ts.Ops = taker.ops(h, false)
					}
				}
			}
			return b, err
		}
		tres, err := RunTakerRestrictedSell(ts, makerAID, tSend, rebind)
		if err != nil {
			t.Fatalf("taker: %v", err)
		}
		<-done
		if merr != nil {
			t.Fatalf("maker: %v", merr)
		}
		if got := maker.holdingReaches(t, before+300); got != before+300 {
			t.Fatalf("the maker's enclave holds %d, want %d", got, before+300)
		}
		if tres.BtcClaimTxid == "" {
			t.Fatalf("the taker did not claim the BTC: %+v", tres)
		}
		e.rec(t, "sell", map[string]interface{}{"lock": tres.Locked.Txid, "t_seq": tres.TSeq, "t_btc": tres.TBtc,
			"claim": mres.SeqClaimTxid, "btc_claim": tres.BtcClaimTxid})
	})

	// -- 3. a claim the policy server refuses: nobody loses a coin --------------
	// The buyer is frozen once the HTLC is locked, so its claim is refused and s
	// is never revealed. A refund before T_seq is refused in a block; after it,
	// the HTLC and the pot go home, and the buyer refunds its BTC at T_btc.
	t.Run("refused-claim-goes-home", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		before := maker.holding(t)
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		refund, _ := xchain.NewKey()
		frozen := false
		ts := taker.swap(e, tc, 100, 40000, func(r *RestrictedResult) {
			if r.TSeq != 0 && !frozen {
				frozen = true
				e.api(t, "POST", "/v1/issuer/freeze", map[string]interface{}{"aid": takerAID, "frozen": true}, nil)
			}
		})
		ts.Ops = taker.ops(secret, true)
		lockedCh := make(chan restricted.Locked, 1)
		ms := maker.swap(e, mc, 100, 40000, func(r *RestrictedResult) {
			if r.Locked != nil && len(lockedCh) == 0 {
				lockedCh <- *r.Locked
			}
		})
		var mres *RestrictedResult
		var merr error
		done := make(chan struct{})
		go func() { mres, merr = RunMakerRestrictedSell(ms, maker.newOps(), mIn, mSend); close(done) }()
		tres, err := RunTakerRestrictedBuy(ts, makerAID, secret, refund, tSend, tRecv)
		if err == nil || !strings.Contains(err.Error(), "frozen") {
			t.Fatalf("a frozen buyer's claim was not refused: %v", err)
		}
		e.rec(t, "frozen_buyer_claim", err.Error())
		l := <-lockedCh

		// A refund before T_seq, forced into a block.
		sw, err := maker.party.Rebuild(l.Terms)
		must(t, err)
		htlc, _, err := sw.Trees()
		must(t, err)
		hc, err := maker.party.Node.CoinAt(l.Txid, l.HTLCVout)
		must(t, err)
		early, err := restricted.BuildRefund(restricted.RefundPlan{Coin: *hc, Home: append([]byte{0x51, 0x20}, sw.SellerEnclave[:]...),
			Leaf: htlc.Leaves["refund"], FeeCoin: l.Reserve[0], TSeq: sw.TSeq})
		must(t, err)
		must(t, maker.party.Node.SignWallet(early))
		raw, _ := early.Hex()
		var addr string
		must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
		var gb interface{}
		gerr := e.seq.WithWallet("w").Call(&gb, "generateblock", addr, []string{raw})
		if gerr == nil || !strings.Contains(gerr.Error(), "non-final") {
			t.Fatalf("a refund before T_seq was not refused in a block for its lock time: %v", gerr)
		}
		e.rec(t, "refund_before_t_seq_in_a_block", gerr.Error())

		// Past T_seq: the maker sends both home with its reserved fee coins.
		e.mineMu.Lock()
		var ok interface{}
		must(t, e.seq.Call(&ok, "setmocktime", int64(sw.TSeq)+600))
		e.mineMu.Unlock()
		<-done
		if merr == nil || !strings.Contains(merr.Error(), "refunded") || len(mres.Refunds) != 2 {
			t.Fatalf("the maker did not send the HTLC and the pot home: %v %+v", merr, mres)
		}
		if got := maker.holdingReaches(t, before); got != before {
			t.Fatalf("the maker's enclave holds %d after the refund, want %d", got, before)
		}
		e.rec(t, "refunds_after_t_seq", mres.Refunds)

		// The buyer's claim after the refund: the HTLC is gone.
		e.api(t, "POST", "/v1/issuer/freeze", map[string]interface{}{"aid": takerAID, "frozen": false}, nil)
		_, cerr := taker.party.Claim(*tres.Terms, restricted.Outpoint{Txid: l.Txid, Vout: l.HTLCVout},
			restricted.Outpoint{Txid: l.Txid, Vout: l.PotVout}, secret, 2)
		if cerr == nil {
			t.Fatal("a claim after the refund was made")
		}
		e.rec(t, "claim_after_refund", cerr.Error())

		// The buyer's BTC comes home at T_btc.
		e.mineMu.Lock()
		must(t, e.btc.Call(&ok, "setmocktime", int64(tres.TBtc)+600))
		e.mineMu.Unlock()
		for i := 0; i < 12; i++ {
			e.mine(t)
		}
		btxid, err := RefundRestrictedBTC(ts, tres.BtcLeg, refund, tres.TBtc, false)
		must(t, err)
		e.rec(t, "btc_refund_at_t_btc", btxid)
	})
}
