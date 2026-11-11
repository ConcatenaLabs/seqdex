package client

// A swap's or an order's coin spent long ago, or spent in a block after the
// refunds were broadcast, against real nodes (see
// xdriver_restricted_regtest_test.go for the environment, one fresh run per
// test):
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run '^TestHorizon' -v -timeout 40m
//
// The seller is always the driver; a buyer that holds a claim back is played
// by hand. Each test moves the chain more than 50 blocks past a spend, or
// forces a held-back claim into a block with generateblock, and asserts that
// the driver still follows it.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// sellerRun is the driver selling 300 of an asset for 100,000 sats, and the
// buyer's side of it played by hand.
type sellerRun struct {
	e            *rtEnv
	maker, taker *rtSide
	makerAID     string
	takerAID     string
	logs         chan string
	mres         *RestrictedResult
	merr         error
	done         chan struct{}
	stMu         sync.Mutex
	state        RestrictedResult // the seller's last saved state
	tSend        XcSend
	tRecv        XcRecv
	tc           *Crypter
	secret       []byte
	hash         [32]byte
	refund       *xchain.Key
	terms        *XcMsg
	script       []byte
	lockTxid     string
	lockVout     uint32
}

func startSeller(t *testing.T, e *rtEnv, poll time.Duration) *sellerRun {
	e.mine(t)
	makerAID, makerKey := e.account(t)
	takerAID, takerKey := e.account(t)
	issuerAID, _ := e.account(t)
	var issued struct {
		Asset string `json:"asset"`
	}
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "Horizon Bond", "ticker": "HZB",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	e.rec(t, "asset", issued.Asset)
	s := &sellerRun{e: e, makerAID: makerAID, takerAID: takerAID, logs: make(chan string, 1024), done: make(chan struct{})}
	s.maker = e.side(t, "hzmaker", makerAID, makerKey, issued.Asset)
	s.taker = e.side(t, "hztaker", takerAID, takerKey, issued.Asset)
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
	s.tSend, s.tRecv, s.tc = tSend, tRecv, tc
	ms := s.maker.swap(e, mc, 300, 100000, func(r *RestrictedResult) {
		s.stMu.Lock()
		s.state = *r
		s.state.Refunds = append([]string(nil), r.Refunds...)
		s.stMu.Unlock()
	})
	ms.Timing.Poll = poll
	ms.Log = func(f string, a ...interface{}) {
		line := fmt.Sprintf(f, a...)
		fmt.Printf("    maker[%s]: %s\n", time.Now().UTC().Format("15:04:05"), line)
		select {
		case s.logs <- line:
		default:
		}
	}
	go func() { s.mres, s.merr = RunMakerRestrictedSell(ms, s.maker.newOps(), mIn, mSend); close(s.done) }()
	s.secret = make([]byte, 32)
	_, _ = rand.Read(s.secret)
	s.hash = sha256.Sum256(s.secret)
	s.refund, _ = xchain.NewKey()
	return s
}

func (s *sellerRun) saved() RestrictedResult {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.state
}

// termsAndBtcLock: the taker's request and the maker's terms; then the
// taker's BTC lock, broadcast.
func (s *sellerRun) termsAndBtcLock(t *testing.T) {
	must(t, sendXc(&XcMsg{Type: XcTermsRequest, TakerAID: s.takerAID}, s.tc, s.tSend))
	terms, err := recvXcType(s.tRecv, s.tc, XcTerms, time.Minute)
	must(t, err)
	s.terms = terms
	claimPub, err := hex.DecodeString(terms.MakerBtcClaimPub)
	must(t, err)
	s.script, err = xchain.NewHashLock(s.secret).LockScript(claimPub, s.refund.PubKey(), terms.BtcLocktime)
	must(t, err)
	a, err := btcutil.NewAddressScriptHash(s.script, &chaincfg.RegressionNetParams)
	must(t, err)
	addr := a.EncodeAddress()
	must(t, s.taker.btc.RPC().CallNamed(&s.lockTxid, "sendtoaddress", map[string]interface{}{"address": addr, "amount": "0.00100000"}))
	var dec struct {
		Vout []struct {
			N            uint32 `json:"n"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	must(t, s.e.btc.Call(&dec, "getrawtransaction", s.lockTxid, true))
	for _, o := range dec.Vout {
		if o.ScriptPubKey.Address == addr {
			s.lockVout = o.N
		}
	}
	s.e.rec(t, "btc_lock", map[string]interface{}{"txid": s.lockTxid, "vout": s.lockVout, "t_btc": terms.BtcLocktime})
}

// waitBtcLegHeight waits for the BTC lock's first confirmation; its height.
func (s *sellerRun) waitBtcLegHeight(t *testing.T) int64 {
	for i := 0; i < 90; i++ {
		var c struct {
			Confirmations int `json:"confirmations"`
		}
		if err := s.e.btc.Call(&c, "getrawtransaction", s.lockTxid, true); err == nil && c.Confirmations >= 1 {
			var n int64
			must(t, s.e.btc.Call(&n, "getblockcount"))
			return n - int64(c.Confirmations) + 1
		}
		time.Sleep(time.Second)
	}
	t.Fatal("the BTC lock did not confirm")
	return 0
}

func (s *sellerRun) btcLegFunded(t *testing.T, height int64) {
	must(t, sendXc(&XcMsg{Type: XcBtcLegFunded, HashH: hex.EncodeToString(s.hash[:]), TakerBtcRefundPub: hex.EncodeToString(s.refund.PubKey()),
		SeqAmount: 300, BtcAmount: 100000,
		Leg: &XcLeg{Txid: s.lockTxid, Vout: s.lockVout, Amount: 100000, RedeemScript: hex.EncodeToString(s.script),
			Locktime: s.terms.BtcLocktime, Height: height}}, s.tc, s.tSend))
}

// seqLocked waits for the seller's lock; the swap's terms and its two coins.
func (s *sellerRun) seqLocked(t *testing.T) (restricted.SwapTerms, restricted.Outpoint, restricted.Outpoint) {
	locked, err := recvXcType(s.tRecv, s.tc, XcSeqLegLocked, 10*time.Minute)
	must(t, err)
	payout, err := hex.DecodeString(locked.Payout)
	must(t, err)
	st := restricted.SwapTerms{Hash: s.hash, SellerAID: s.makerAID, BuyerAID: s.takerAID, TSeq: locked.Leg.Locktime,
		Value: 300, PotAsset: s.e.feeAsset, PotValue: 20000, Payout: payout}
	ha := restricted.Outpoint{Txid: locked.Leg.Txid, Vout: locked.Leg.Vout}
	pa := restricted.Outpoint{Txid: locked.Leg.Txid, Vout: *locked.PotVout}
	s.e.rec(t, "seq_lock", map[string]interface{}{"txid": locked.Leg.Txid, "block": locked.Leg.BlockHash, "anchor_height": locked.Leg.AnchorHeight, "t_seq": st.TSeq})
	return st, ha, pa
}

// lockToClaim runs the swap from the terms to a mined Sequentia lock: an
// honest BTC lock, confirmed and announced.
func (s *sellerRun) lockToClaim(t *testing.T) (restricted.SwapTerms, restricted.Outpoint, restricted.Outpoint) {
	s.termsAndBtcLock(t)
	s.btcLegFunded(t, s.waitBtcLegHeight(t))
	terms, ha, pa := s.seqLocked(t)
	s.waitConfirmed(t, ha.Txid)
	return terms, ha, pa
}

// buildClaim is Party.Claim without the broadcast: the policy server's
// co-sign, checked, and the buyer's finished claim, held by the buyer.
func buildClaim(t *testing.T, p *restricted.Party, st restricted.SwapTerms, htlcAt, potAt restricted.Outpoint, secret []byte) *restricted.Unsigned {
	sw, err := p.Rebuild(st)
	must(t, err)
	htlc, _, err := sw.Trees()
	must(t, err)
	hc, err := p.Node.CoinAt(htlcAt.Txid, htlcAt.Vout)
	must(t, err)
	pc, err := p.Node.CoinAt(potAt.Txid, potAt.Vout)
	must(t, err)
	enc, _, _, err := p.Enclave()
	must(t, err)
	change, err := p.Node.NewScript()
	must(t, err)
	fee, err := p.Node.FeeAtoms(st.PotAsset, 500, 2)
	must(t, err)
	u, err := restricted.BuildClaim(restricted.ClaimPlan{Swap: sw, HTLC: *hc, Pot: *pc, BuyerEnclave: enc, PotChange: change, Fee: fee})
	must(t, err)
	g, err := p.Node.Genesis()
	must(t, err)
	raw, _ := u.Hex()
	resp, err := p.Policy.Cosign(restricted.CosignRequest{Tx: raw, Asset: p.Asset, SenderAID: st.SellerAID, HTLCInputs: []restricted.HTLCInput{{Input: 0}}})
	must(t, err)
	cl := htlc.Leaves["claim"]
	psig, _, err := u.CheckedPolicySig(resp, 0, cl.Script, cl.Control, sw.PolicyX, g)
	must(t, err)
	must(t, restricted.FinishClaim(u, sw, secret, psig, p.Key, g))
	return u
}

func (s *sellerRun) waitConfirmed(t *testing.T, txid string) string {
	for i := 0; i < 120; i++ {
		if bh, err := s.taker.seq.BlockHashOfTx(txid); err == nil && bh != "" {
			return bh
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s not confirmed", txid)
	return ""
}

// btcClaimMined mines a block on each chain and checks that the seller's BTC
// claim spent the leg in it.
func (s *sellerRun) btcClaimMined(t *testing.T, claim string) {
	s.e.mine(t)
	var c struct {
		Confirmations int    `json:"confirmations"`
		BlockHash     string `json:"blockhash"`
	}
	must(t, s.e.btc.Call(&c, "getrawtransaction", claim, true))
	var txo interface{}
	must(t, s.e.btc.Call(&txo, "gettxout", s.lockTxid, s.lockVout, true))
	s.e.rec(t, "btc_claim_mined", map[string]interface{}{"txid": claim, "block": c.BlockHash, "confirmations": c.Confirmations, "btc_htlc_unspent": txo != nil})
	if c.Confirmations < 1 || txo != nil {
		t.Fatalf("the seller's BTC claim %s is not mined over the leg (confirmations %d, leg unspent %v)", claim, c.Confirmations, txo != nil)
	}
}

// generateBlock makes the next Sequentia block from exactly txs (raw
// transactions or mempool txids), nothing else from the mempool.
func (e *rtEnv) generateBlock(t *testing.T, txs []string) (string, error) {
	e.mineMu.Lock()
	defer e.mineMu.Unlock()
	var addr string
	must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
	var gb struct {
		Hash string `json:"hash"`
	}
	err := e.seq.WithWallet("w").Call(&gb, "generateblock", addr, txs)
	return gb.Hash, err
}

// seqBlocks mines n Sequentia blocks (from the mempool) at once.
func (e *rtEnv) seqBlocks(t *testing.T, n int) {
	e.mineMu.Lock()
	defer e.mineMu.Unlock()
	var addr string
	must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
	var hs []string
	must(t, e.seq.WithWallet("w").Call(&hs, "generatetoaddress", n, addr))
}

func (e *rtEnv) seqMock(t *testing.T, at int64) {
	e.mineMu.Lock()
	defer e.mineMu.Unlock()
	var ok interface{}
	must(t, e.seq.Call(&ok, "setmocktime", at))
}

func (e *rtEnv) btcMock(t *testing.T, at int64) {
	e.mineMu.Lock()
	defer e.mineMu.Unlock()
	var ok interface{}
	must(t, e.btc.Call(&ok, "setmocktime", at))
}

func blockHas(t *testing.T, e *rtEnv, block, txid string) bool {
	var b struct {
		Tx []string `json:"tx"`
	}
	must(t, e.seq.Call(&b, "getblock", block))
	for _, x := range b.Tx {
		if x == txid {
			return true
		}
	}
	return false
}

// A seller stalled for 51 blocks after the buyer's claim reads the secret
// from that claim when it looks again, past T_seq, and claims the BTC: it
// never writes the swap ended, and the buyer's BTC refund at T_btc is refused.
func TestHorizonSellerReadsAnOldClaim(t *testing.T) {
	e := loadRtEnv(t)
	s := startSeller(t, e, 45*time.Second)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	e.miner(t, ctx)
	terms, ha, pa := s.lockToClaim(t)
	time.Sleep(3 * time.Second) // the seller's first look at the pot has passed
	u := buildClaim(t, s.taker.party, terms, ha, pa, s.secret)
	raw, _ := u.Hex()
	stop()
	// The claim, then 51 blocks, all before the seller's next look.
	block, err := e.generateBlock(t, []string{raw})
	must(t, err)
	if !blockHas(t, e, block, u.Txid()) {
		t.Fatalf("block %s does not hold the claim %s", block, u.Txid())
	}
	e.seqBlocks(t, 51)
	e.rec(t, "claim_mined", map[string]interface{}{"txid": u.Txid(), "block": block, "blocks_after": 51})
	if got := s.taker.holdingReaches(t, 300); got != 300 {
		t.Fatalf("the buyer's enclave holds %d after its claim", got)
	}
	// The seller's next look comes within its 45 s poll; whatever it does,
	// T_seq then passes.
	var seen []string
	deadline := time.After(2 * time.Minute)
wait:
	for {
		select {
		case l := <-s.logs:
			seen = append(seen, l)
			if strings.Contains(l, "watching the pot") || strings.Contains(l, "claimed the BTC") {
				break wait
			}
		case <-s.done:
			break wait
		case <-deadline:
			break wait
		}
	}
	e.rec(t, "seller_after_stall", seen)
	e.seqMock(t, int64(terms.TSeq)+600)
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	e.miner(t, ctx2)
	select {
	case <-s.done:
	case <-time.After(5 * time.Minute):
		t.Fatal("the seller did not finish after T_seq")
	}
	stop2()
	e.rec(t, "seller_result", map[string]interface{}{"err": fmt.Sprint(s.merr), "settled": s.mres.Settled, "ended": s.mres.Ended,
		"refunds": s.mres.Refunds, "seq_claim": s.mres.SeqClaimTxid, "btc_claim": s.mres.BtcClaimTxid, "secret_read": s.mres.Secret == hex.EncodeToString(s.secret)})
	if s.mres.Ended != "" {
		t.Fatalf("the seller wrote the swap ended (%q, %v) with the buyer's claim %s on the chain", s.mres.Ended, s.merr, u.Txid())
	}
	if s.merr != nil || !s.mres.Settled || s.mres.BtcClaimTxid == "" || s.mres.Secret != hex.EncodeToString(s.secret) || s.mres.SeqClaimTxid != u.Txid() {
		t.Fatalf("the seller did not read s from the claim and claim the BTC: %v %+v", s.merr, s.mres)
	}
	if len(s.mres.Refunds) != 0 {
		t.Fatalf("the seller broadcast refunds %v of coins the claim spent", s.mres.Refunds)
	}
	s.btcClaimMined(t, s.mres.BtcClaimTxid)
	// The buyer's refund of its BTC at T_btc finds the leg claimed.
	e.btcMock(t, int64(s.terms.BtcLocktime)+600)
	for i := 0; i < 12; i++ {
		e.mine(t)
	}
	rs := &RestrictedSwap{Ops: s.taker.ops(s.secret, true), BtcMedianTime: btcMTP(e.btc), BtcFee: 2000}
	leg := &xchain.LegLock{Script: s.script, Funded: &xchain.FundedHTLC{TxID: s.lockTxid, Vout: s.lockVout, Amount: 100000}, Locktime: s.terms.BtcLocktime}
	btxid, rerr := RefundRestrictedBTC(rs, leg, s.refund, s.terms.BtcLocktime, false)
	e.rec(t, "buyer_btc_refund", map[string]interface{}{"txid": btxid, "err": fmt.Sprint(rerr)})
	if rerr == nil {
		t.Fatalf("the buyer refunded the BTC the seller claimed: %s", btxid)
	}
}

// The buyer holds back a claim co-signed before the cut-off. After T_seq the
// seller broadcasts both refunds, and every block for a while is made with
// none of them; the seller does not write the swap ended. Then a block takes
// the claim in their place: the seller reads s from it and claims the BTC.
func TestHorizonWithheldClaimAfterRefunds(t *testing.T) {
	e := loadRtEnv(t)
	s := startSeller(t, e, time.Second)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	e.miner(t, ctx)
	terms, ha, pa := s.lockToClaim(t)
	u := buildClaim(t, s.taker.party, terms, ha, pa, s.secret)
	raw, _ := u.Hex()
	e.rec(t, "withheld_claim", u.Txid())
	stop() // from here every Sequentia block is made by hand, with no mempool transaction
	time.Sleep(2 * time.Second)
	e.seqMock(t, int64(terms.TSeq)+600)
	// Empty blocks until both refunds are in the mempool, then 10 more: more
	// than RefundDepth blocks the refunds are not in.
	var refunds []string
	extra := -1
	for i := 0; i < 90 && extra < 10; i++ {
		select {
		case <-s.done:
			e.rec(t, "seller_returned_early", map[string]interface{}{"err": fmt.Sprint(s.merr), "ended": s.mres.Ended, "refunds": s.mres.Refunds})
			t.Fatalf("the seller's driver returned before any refund was mined: %v (ended %q, refunds %v)", s.merr, s.mres.Ended, s.mres.Refunds)
		default:
		}
		_, err := e.generateBlock(t, []string{})
		must(t, err)
		if extra >= 0 {
			extra++
		} else if st := s.saved(); len(st.Refunds) >= 2 {
			refunds = st.Refunds
			extra = 0
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if extra < 10 {
		t.Fatalf("the seller broadcast no two refunds after T_seq: %+v", s.saved().Refunds)
	}
	var mp []string
	must(t, e.seq.Call(&mp, "getrawmempool"))
	inPool := 0
	for _, r := range refunds[:2] {
		for _, m := range mp {
			if m == r {
				inPool++
			}
		}
	}
	var htlcOut interface{}
	must(t, e.seq.Call(&htlcOut, "gettxout", ha.Txid, ha.Vout, false))
	st := s.saved()
	e.rec(t, "refunds_unmined", map[string]interface{}{"refunds": refunds, "in_mempool": inPool, "empty_blocks_after": extra,
		"htlc_unspent_in_chain": htlcOut != nil, "seller_ended": st.Ended})
	if inPool != 2 || htlcOut == nil {
		t.Fatalf("the refunds are not both waiting in the mempool over an unspent HTLC: %d in the pool, HTLC unspent %v", inPool, htlcOut != nil)
	}
	select {
	case <-s.done:
		t.Fatalf("the seller's driver returned with its refunds unmined: %v (ended %q)", s.merr, s.mres.Ended)
	default:
	}
	if st.Ended != "" {
		t.Fatalf("the seller wrote the swap ended with its refunds unmined: %q", st.Ended)
	}
	// A producer that had the claim first mines it in the refunds' place.
	block, gerr := e.generateBlock(t, []string{raw})
	e.rec(t, "claim_in_a_block", map[string]interface{}{"block": block, "err": fmt.Sprint(gerr)})
	if gerr != nil {
		t.Fatalf("the withheld claim was refused: %v", gerr)
	}
	if !blockHas(t, e, block, u.Txid()) {
		t.Fatalf("block %s does not hold the claim", block)
	}
	must(t, e.seq.Call(&mp, "getrawmempool"))
	for _, m := range mp {
		for _, r := range refunds[:2] {
			if m == r {
				t.Fatalf("the refund %s is still in the mempool beside the claim", r)
			}
		}
	}
	if got := s.taker.holdingReaches(t, 300); got != 300 {
		t.Fatalf("the buyer's enclave holds %d after its claim", got)
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Minute):
		t.Fatal("the seller did not follow the claim within 3 minutes")
	}
	e.rec(t, "seller_result", map[string]interface{}{"err": fmt.Sprint(s.merr), "settled": s.mres.Settled, "ended": s.mres.Ended,
		"refunds": s.mres.Refunds, "seq_claim": s.mres.SeqClaimTxid, "btc_claim": s.mres.BtcClaimTxid, "secret_read": s.mres.Secret == hex.EncodeToString(s.secret)})
	if s.merr != nil || s.mres.Ended != "" || !s.mres.Settled || s.mres.BtcClaimTxid == "" || s.mres.SeqClaimTxid != u.Txid() || s.mres.Secret != hex.EncodeToString(s.secret) {
		t.Fatalf("the seller did not read s from the claim mined in place of its refunds: %v %+v", s.merr, s.mres)
	}
	s.btcClaimMined(t, s.mres.BtcClaimTxid)
}

// The maker buys (holds s) and stops before its claim; the taker-seller
// sends its HTLC home at T_seq. The maker's resume, run 55 blocks after that
// refund and past T_btc, follows the HTLC's spend to the refund and sends its
// BTC home.
func TestHorizonBuyerResumeAfterRefund(t *testing.T) {
	e := loadRtEnv(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	e.mine(t)
	makerAID, makerKey := e.account(t)
	takerAID, takerKey := e.account(t)
	issuerAID, _ := e.account(t)
	var issued struct {
		Asset string `json:"asset"`
	}
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "Horizon Bond", "ticker": "HZB",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": takerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	maker := e.side(t, "hzmaker", makerAID, makerKey, issued.Asset)
	taker := e.side(t, "hztaker", takerAID, takerKey, issued.Asset)
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	mctx0, mstop := context.WithCancel(ctx)
	e.miner(t, mctx0)
	tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
	ts := taker.swap(e, tc, 100, 30000, nil)
	ts.Ops = taker.ops(make([]byte, 32), false)
	mctx, crash := context.WithCancel(ctx)
	var saved []byte
	ms := maker.swap(e, mc, 100, 30000, func(r *RestrictedResult) {
		saved, _ = json.Marshal(r)
		if r.HTLCAt != nil {
			crash()
		}
	})
	ms.Ctx = mctx
	done := make(chan struct{})
	go func() { _, _ = RunMakerRestrictedBuy(ms, maker.newOps(), mIn, mSend); close(done) }()
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
	var tres *RestrictedResult
	var terr error
	tdone := make(chan struct{})
	go func() { tres, terr = RunTakerRestrictedSell(ts, makerAID, tSend, rebind); close(tdone) }()
	<-done
	var st RestrictedResult
	must(t, json.Unmarshal(saved, &st))
	if st.HTLCAt == nil || st.SeqClaimTxid != "" || st.BtcLeg == nil {
		t.Fatalf("the saved state is not an unclaimed buyer's: %+v", st)
	}
	e.rec(t, "maker_state", map[string]interface{}{"role": st.Role, "htlc": st.HTLCAt, "btc_leg": st.BtcLeg.Funded.TxID, "t_seq": st.TSeq, "t_btc": st.TBtc})
	// T_seq passes: the taker-seller sends its HTLC and pot home.
	e.seqMock(t, int64(st.TSeq)+600)
	<-tdone
	e.rec(t, "taker_seller", map[string]interface{}{"err": fmt.Sprint(terr), "refunds": tres.Refunds, "ended": tres.Ended})
	if len(tres.Refunds) == 0 {
		t.Fatalf("the taker-seller sent nothing home: %v", terr)
	}
	mstop()
	time.Sleep(2 * time.Second)
	// 55 blocks, and T_btc on Bitcoin.
	e.seqBlocks(t, 55)
	e.btcMock(t, int64(st.TBtc)+600)
	for i := 0; i < 12; i++ {
		e.mine(t)
	}
	var txo interface{}
	must(t, e.btc.Call(&txo, "gettxout", st.BtcLeg.Funded.TxID, st.BtcLeg.Funded.Vout, true))
	if txo == nil {
		t.Fatal("the BTC HTLC is spent before the resume")
	}
	rs := maker.swap(e, nil, 0, 0, nil)
	out, rerr := ResumeRestricted(rs, &st, maker.newOps())
	e.rec(t, "maker_resume", map[string]interface{}{"err": fmt.Sprint(rerr), "ended": out.Ended, "settled": out.Settled, "refunds": out.Refunds})
	if rerr != nil || out.Settled || !strings.HasPrefix(out.Ended, "the BTC went home at T_btc: ") || len(out.Refunds) == 0 {
		t.Fatalf("the resume did not send the BTC home: %v (ended %q)", rerr, out.Ended)
	}
	refund := out.Refunds[len(out.Refunds)-1]
	e.mine(t)
	var c struct {
		Confirmations int    `json:"confirmations"`
		BlockHash     string `json:"blockhash"`
		Vin           []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	must(t, e.btc.Call(&c, "getrawtransaction", refund, true))
	must(t, e.btc.Call(&txo, "gettxout", st.BtcLeg.Funded.TxID, st.BtcLeg.Funded.Vout, true))
	spends := false
	for _, in := range c.Vin {
		if in.Txid == st.BtcLeg.Funded.TxID && in.Vout == st.BtcLeg.Funded.Vout {
			spends = true
		}
	}
	e.rec(t, "btc_refund_mined", map[string]interface{}{"txid": refund, "block": c.BlockHash, "confirmations": c.Confirmations, "spends_htlc": spends, "btc_htlc_unspent": txo != nil})
	if c.Confirmations < 1 || !spends || txo != nil {
		t.Fatalf("the BTC refund %s is not mined over the HTLC", refund)
	}
}

// A gated order filled in part, then left 67 blocks and past its expiry:
// RefundGated follows the fill to the remainder and sends it home.
func TestHorizonGatedRemainderHome(t *testing.T) {
	e := loadRtEnv(t)
	e.mine(t)
	makerAID, makerKey := e.account(t)
	takerAID, takerKey := e.account(t)
	issuerAID, _ := e.account(t)
	var issued struct {
		Asset string `json:"asset"`
	}
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "Horizon Gated", "ticker": "HZG",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	asset := issued.Asset
	maker := e.side(t, "hzgmaker", makerAID, makerKey, asset)
	taker := e.side(t, "hzgtaker", takerAID, takerKey, asset)
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	e.miner(t, ctx)
	expiry := uint32(maker.party.Node.Now().Add(2 * time.Hour).Unix())
	pl, err := maker.party.PlaceGated(restricted.GatedTerms{AssetB: e.feeAsset, Value: 600, RateNum: 2, RateDen: 1,
		MinLot: 100, Expiry: expiry, FeeAsset: e.feeAsset}, 2)
	must(t, err)
	tree, err := pl.Order.Tree()
	must(t, err)
	var oc *restricted.Coin
	for i := 0; i < 30 && oc == nil; i++ {
		if bh, _ := maker.seq.BlockHashOfTx(pl.Txid); bh != "" {
			oc, err = maker.party.Node.CoinAt(pl.Txid, pl.Vout)
			must(t, err)
		}
		time.Sleep(time.Second)
	}
	if oc == nil {
		t.Fatal("not placed")
	}
	// The taker fills 200 through the policy server: the remainder, 400, at
	// output 1.
	takerEnclave, _, _, err := taker.party.Enclave()
	must(t, err)
	fee, err := taker.party.Node.FeeAtoms(e.feeAsset, 1500, 2)
	must(t, err)
	reqB := covenant.CeilPrice(200, 2, 1)
	pay, err := taker.party.Node.WalletCoins(e.feeAsset, reqB+fee)
	must(t, err)
	var have uint64
	for _, c := range pay {
		have += c.Value
	}
	change, err := taker.party.Node.NewScript()
	must(t, err)
	fa, _ := restricted.ReverseHex(e.feeAsset)
	u, err := restricted.Build(append([]restricted.Coin{*oc}, pay...), []restricted.Out{
		{Asset: fa, Value: reqB, Script: append([]byte{0x51, 0x20}, pl.Order.MakerProg...)},
		{Asset: pl.Order.AssetA, Value: 400, Script: tree.ScriptPubKey},
		{Asset: pl.Order.AssetA, Value: 200, Script: takerEnclave},
		{Asset: fa, Value: have - reqB - fee, Script: change},
		{Asset: fa, Value: fee},
	}, 0)
	must(t, err)
	must(t, taker.party.Node.SignWallet(u))
	raw, _ := u.Hex()
	resp, err := taker.party.Policy.Cosign(restricted.CosignRequest{Tx: raw, Asset: asset, SenderAID: makerAID,
		OrderInputs: []restricted.OrderInput{{Input: 0}}, Broadcast: true})
	must(t, err)
	fb := ""
	for i := 0; i < 60 && fb == ""; i++ {
		fb, _ = taker.seq.BlockHashOfTx(resp.Txid)
		time.Sleep(time.Second)
	}
	if fb == "" {
		t.Fatalf("the fill %s was not mined", resp.Txid)
	}
	stop()
	time.Sleep(2 * time.Second)
	// 55 blocks pass (55 minutes at the testnet's 60 s), then the expiry and 12 more.
	e.seqBlocks(t, 55)
	e.seqMock(t, int64(expiry)+600)
	e.seqBlocks(t, 12)
	e.rec(t, "fill", map[string]interface{}{"txid": resp.Txid, "block": fb, "blocks_after": 67})
	before := maker.holding(t)
	rtx, rerr := maker.party.RefundGated(pl)
	e.rec(t, "refund_gated", map[string]interface{}{"txid": rtx, "err": fmt.Sprint(rerr)})
	if rerr != nil || rtx == "" {
		t.Fatalf("RefundGated did not follow the fill to the remainder: %v", rerr)
	}
	// Forced into the next block, alone.
	block, gerr := e.generateBlock(t, []string{rtx})
	e.rec(t, "refund_in_a_block", map[string]interface{}{"block": block, "err": fmt.Sprint(gerr)})
	if gerr != nil || !blockHas(t, e, block, rtx) {
		t.Fatalf("the remainder's refund %s was not mined: %v", rtx, gerr)
	}
	var dec struct {
		Vin []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	must(t, e.seq.Call(&dec, "getrawtransaction", rtx, true))
	if len(dec.Vin) == 0 {
		t.Fatalf("the refund %s has no input", rtx)
	}
	rem, err := maker.party.Node.CoinAt(resp.Txid, 1)
	must(t, err)
	after := maker.holdingReaches(t, before+400)
	e.rec(t, "remainder_home", map[string]interface{}{"refund_input0": fmt.Sprintf("%s:%d", dec.Vin[0].Txid, dec.Vin[0].Vout),
		"remainder_unspent": rem != nil, "maker_enclave_before": before, "maker_enclave_after": after})
	if dec.Vin[0].Txid != resp.Txid || dec.Vin[0].Vout != 1 || rem != nil || after != before+400 {
		t.Fatalf("the remainder %s:1 did not go home: refund input %+v, unspent %v, enclave %d -> %d", resp.Txid, dec.Vin, rem != nil, before, after)
	}
}
