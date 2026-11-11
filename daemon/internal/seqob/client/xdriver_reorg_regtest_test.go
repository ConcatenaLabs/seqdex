package client

// A Bitcoin reorganization against the on-chain cross-chain swap (version 1:
// a BTC hash lock on bitcoind, an asset hash lock on Sequentia), driven by the
// real drivers (RunTakerForward/RunMakerForward, RunTakerReverse/RunMakerReverse)
// over an in-memory courier, against a real bitcoind and an anchored sequentiad
// (-validateanchor=1 -par=1) from pkg/xchain/testdata/start-regtest-bitcoind.sh.
// It carries over the proof review's runs 18 and 20 (the seller's attack).
//
//	NAMEDLOCK_DIR=/dev/shm/$USER-reorg bash ../../../pkg/xchain/testdata/start-regtest-bitcoind.sh up
//	SEQDEX_NAMEDLOCK_ENV=/dev/shm/$USER-reorg/env go test ./internal/seqob/client -run '^TestXchainSellerReorg' -v
//
// The seller's attack: after its counterparty's claim revealed the secret and it
// claimed the bitcoin, the asset funder (forward: the maker; reverse: the taker)
// waits for a Bitcoin reorganization of only the block its lock anchors to (the
// bitcoin payment stays) and spends its returned lock's inputs elsewhere. With a
// lock that signals replaceability that respend wins every honest mempool; the
// lock is sent final, so it is refused, and the buyer's claim confirms again.
import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type reorgEnv struct {
	btc, seq     *xchain.RPC
	asset        string
	mu           sync.Mutex
	pauseSeq     atomic.Bool
	pauseBtc     atomic.Bool
	stop         chan struct{}
	seqEvery     time.Duration
	btcEvery     time.Duration
	seqMinerAddr string
	btcMinerAddr string
}

func reorgRPC(t *testing.T, s string) *xchain.RPC {
	p := strings.SplitN(s, ":", 4)
	port, _ := strconv.Atoi(p[1])
	return xchain.NewRPC(p[0], port, p[2], p[3])
}

func reorgLoad(t *testing.T) *reorgEnv {
	path := os.Getenv("SEQDEX_NAMEDLOCK_ENV")
	if path == "" {
		t.Skip("SEQDEX_NAMEDLOCK_ENV not set")
	}
	raw, err := os.ReadFile(path)
	reorgMust(t, err)
	kv := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = v
		}
	}
	e := &reorgEnv{btc: reorgRPC(t, kv["NAMEDLOCK_BTC_RPC"]), seq: reorgRPC(t, kv["NAMEDLOCK_SEQ_RPC"]), asset: kv["NAMEDLOCK_ASSET"],
		stop: make(chan struct{}), seqEvery: 500 * time.Millisecond, btcEvery: 5 * time.Second}
	reorgMust(t, e.btc.WithWallet("w").Call(&e.btcMinerAddr, "getnewaddress"))
	reorgMust(t, e.seq.WithWallet("w").Call(&e.seqMinerAddr, "getnewaddress"))
	return e
}

func reorgMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *reorgEnv) mineSeq(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var h []string
	_ = e.seq.Call(&h, "generatetoaddress", n, e.seqMinerAddr)
	return h
}

func (e *reorgEnv) mineBtc(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var h []string
	_ = e.btc.Call(&h, "generatetoaddress", n, e.btcMinerAddr)
	return h
}

// miners: Sequentia every 0.5 s, Bitcoin every 5 s (the 60 s : 600 s ratio of the
// real chains, scaled), each pausable.
func (e *reorgEnv) startMiners() {
	go func() {
		tk := time.NewTicker(e.seqEvery)
		defer tk.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-tk.C:
				if !e.pauseSeq.Load() {
					e.mineSeq(1)
				}
			}
		}
	}()
	go func() {
		tk := time.NewTicker(e.btcEvery)
		defer tk.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-tk.C:
				if !e.pauseBtc.Load() {
					e.mineBtc(1)
				}
			}
		}
	}()
}

type reorgHdr struct {
	Height        int64  `json:"height"`
	AnchorHeight  int64  `json:"anchorheight"`
	Confirmations int64  `json:"confirmations"`
	Hash          string `json:"hash"`
}

func (e *reorgEnv) seqHdr(t *testing.T, h string) reorgHdr {
	var x reorgHdr
	reorgMust(t, e.seq.Call(&x, "getblockheader", h))
	return x
}

type reorgTx struct {
	Txid          string `json:"txid"`
	BlockHash     string `json:"blockhash"`
	Confirmations int64  `json:"confirmations"`
	Hex           string `json:"hex"`
	Vin           []struct {
		Txid string `json:"txid"`
		Vout uint32 `json:"vout"`
	} `json:"vin"`
	Vout []struct {
		Value        json.Number `json:"value"`
		Asset        string      `json:"asset"`
		N            uint32      `json:"n"`
		ScriptPubKey struct {
			Hex     string `json:"hex"`
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

func (e *reorgEnv) seqTx(txid string) (*reorgTx, error) {
	var x reorgTx
	err := e.seq.Call(&x, "getrawtransaction", txid, true)
	return &x, err
}

func (e *reorgEnv) inSeqMempool(txid string) (bool, string) {
	var m map[string]interface{}
	if err := e.seq.Call(&m, "getmempoolentry", txid); err != nil {
		return false, ""
	}
	return true, fmt.Sprint(m["bip125-replaceable"])
}

func (e *reorgEnv) where(t *testing.T, label, txid string) {
	tx, err := e.seqTx(txid)
	if err != nil {
		t.Logf("  %s %s: %v", label, txid, err)
		return
	}
	in, rbf := e.inSeqMempool(txid)
	anchor := int64(-1)
	if tx.BlockHash != "" {
		anchor = e.seqHdr(t, tx.BlockHash).AnchorHeight
	}
	t.Logf("  %s %s: block=%s confs=%d anchor=%d mempool=%v bip125-replaceable=%s", label, txid, tx.BlockHash, tx.Confirmations, anchor, in, rbf)
}

func (e *reorgEnv) assetBal(t *testing.T, wallet string) string {
	var b struct {
		Mine struct {
			Trusted map[string]json.Number `json:"trusted"`
		} `json:"mine"`
	}
	reorgMust(t, e.seq.WithWallet(wallet).Call(&b, "getbalances"))
	return string(b.Mine.Trusted[e.asset])
}

func (e *reorgEnv) btcTip(t *testing.T) int64 {
	var n int64
	reorgMust(t, e.btc.Call(&n, "getblockcount"))
	return n
}

// reorgBtc replaces parent blocks fork..tip with a longer branch; if dsWallet is set
// it first double-spends dsTxid's inputs from that wallet (full RBF, Core 28).
func (e *reorgEnv) reorgBtc(t *testing.T, fork int64, dsWallet, dsTxid string) (int64, string) {
	e.mu.Lock()
	tip := e.btcTip(t)
	var h string
	reorgMust(t, e.btc.Call(&h, "getblockhash", fork))
	reorgMust(t, e.btc.Call(nil, "invalidateblock", h))
	ds := ""
	if dsWallet != "" {
		var d struct {
			Vin []struct {
				Txid string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		}
		reorgMust(t, e.btc.Call(&d, "getrawtransaction", dsTxid, true))
		var ins []map[string]interface{}
		var tot float64
		for _, v := range d.Vin {
			var p struct {
				Vout []struct {
					Value float64 `json:"value"`
				} `json:"vout"`
			}
			reorgMust(t, e.btc.Call(&p, "getrawtransaction", v.Txid, true))
			tot += p.Vout[v.Vout].Value
			ins = append(ins, map[string]interface{}{"txid": v.Txid, "vout": v.Vout})
		}
		var addr, raw string
		reorgMust(t, e.btc.WithWallet(dsWallet).Call(&addr, "getnewaddress"))
		reorgMust(t, e.btc.Call(&raw, "createrawtransaction", ins, []map[string]interface{}{{addr: fmt.Sprintf("%.8f", tot-0.001)}}, 0, true))
		var s struct {
			Hex      string `json:"hex"`
			Complete bool   `json:"complete"`
		}
		reorgMust(t, e.btc.WithWallet(dsWallet).Call(&s, "signrawtransactionwithwallet", raw))
		if err := e.btc.Call(&ds, "sendrawtransaction", s.Hex); err != nil {
			var me interface{}
			merr := e.btc.Call(&me, "getmempoolentry", dsTxid)
			t.Logf("double-spend refused: %v; leg in mempool: %v %v; complete=%v", err, me, merr, s.Complete)
			for _, v := range d.Vin {
				var o interface{}
				_ = e.btc.Call(&o, "gettxout", v.Txid, v.Vout, true)
				var pt map[string]interface{}
				_ = e.btc.Call(&pt, "getrawtransaction", v.Txid, true)
				t.Logf("  input %s:%d gettxout=%v source-confs=%v", v.Txid, v.Vout, o, pt["confirmations"])
			}
			var mp []string
			_ = e.btc.Call(&mp, "getrawmempool")
			t.Logf("  mempool: %v", mp)
			t.FailNow()
		}
	}
	replaced := tip - fork + 1
	var nb []string
	reorgMust(t, e.btc.Call(&nb, "generatetoaddress", replaced+1, e.btcMinerAddr))
	e.mu.Unlock()
	t.Logf("BITCOIN REORG: replaced %d block(s) %d..%d (first %s) with %d new; double-spend=%s", replaced, fork, tip, h, len(nb), ds)
	return replaced, ds
}

func (e *reorgEnv) waitGone(t *testing.T, block string) {
	for i := 0; i < 240; i++ {
		if e.seqHdr(t, block).Confirmations == -1 {
			var st struct {
				AnchorHeight int64 `json:"anchorheight"`
			}
			_ = e.seq.Call(&st, "getanchorstatus")
			t.Logf("Sequentia block %s left the active chain; node anchor now %d", block, st.AnchorHeight)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("block %s did not leave the active chain", block)
}

// respend: the lock's funder spends the lock's own inputs home, the HTLC output
// redirected to a fresh address of its own and 20000 atoms more fee in the asset.
func (e *reorgEnv) respend(t *testing.T, wallet, lockTxid string, htlcVout uint32) (string, error) {
	tx, err := e.seqTx(lockTxid)
	if err != nil {
		return "", err
	}
	var home string
	reorgMust(t, e.seq.WithWallet(wallet).Call(&home, "getnewaddress"))
	var ins []map[string]interface{}
	for _, v := range tx.Vin {
		ins = append(ins, map[string]interface{}{"txid": v.Txid, "vout": v.Vout, "sequence": 0xfffffffd})
	}
	const bump = 0.0002
	bumped := false
	var outs []map[string]interface{}
	for _, o := range tx.Vout {
		v, _ := strconv.ParseFloat(string(o.Value), 64)
		switch {
		case o.ScriptPubKey.Type == "fee":
			outs = append(outs, map[string]interface{}{"fee": fmt.Sprintf("%.8f", v+bump), "fee_asset": o.Asset})
		case o.N == htlcVout:
			outs = append(outs, map[string]interface{}{home: fmt.Sprintf("%.8f", v), "asset": o.Asset})
		default:
			if !bumped && o.Asset == e.asset && v > bump {
				v -= bump
				bumped = true
			}
			outs = append(outs, map[string]interface{}{o.ScriptPubKey.Address: fmt.Sprintf("%.8f", v), "asset": o.Asset})
		}
	}
	var raw string
	if err := e.seq.Call(&raw, "createrawtransaction", ins, outs, 0, true); err != nil {
		return "", err
	}
	var s struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := e.seq.WithWallet(wallet).Call(&s, "signrawtransactionwithwallet", raw); err != nil {
		return "", err
	}
	var txid string
	err = e.seq.Call(&txid, "sendrawtransaction", s.Hex)
	return txid, err
}

func reorgSecret(t *testing.T) []byte {
	s := make([]byte, 32)
	_, err := rand.Read(s)
	reorgMust(t, err)
	return s
}

func TestXchainSellerReorg(t *testing.T) {
	for _, dir := range []string{"forward", "reverse"} {
		t.Run(dir, func(t *testing.T) { reorgSellerRun(t, reorgLoad(t), dir) })
	}
}

func reorgSellerRun(t *testing.T, e *reorgEnv, dir string) {
	attack, defense, minConf := "seller", "", 1
	t.Logf("direction=%s attack=%s defense=%s min-btc-conf=%d asset=%s", dir, attack, defense, minConf, e.asset)
	e.startMiners()
	defer close(e.stop)

	btcT := xchain.NewBitcoinChain(e.btc, "taker", &chaincfg.RegressionNetParams)
	btcM := xchain.NewBitcoinChain(e.btc, "maker", &chaincfg.RegressionNetParams)
	seqT := xchain.NewChain(e.seq, "taker")
	seqM := xchain.NewChain(e.seq, "maker")
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	timing := XcTiming{Poll: 200 * time.Millisecond, TermsWait: time.Minute, BtcConfWait: 5 * time.Minute,
		SeqLockWait: 5 * time.Minute, AnchorWait: 2 * time.Minute, TermsReqWait: time.Minute, BtcFundWait: 5 * time.Minute}
	logf := func(who string) func(string, ...interface{}) {
		return func(f string, a ...interface{}) {
			t.Logf("%s[%s]: %s", who, time.Now().UTC().Format("15:04:05"), fmt.Sprintf(f, a...))
		}
	}
	const seqAtoms, btcSats = uint64(500_00000000), uint64(1_000_000)

	var (
		payerWallet, funderWallet      string // BTC payer (buyer of the asset) / asset funder (seller)
		btcLegTxid                     string
		btcLegHeight                   int64
		lockTxid, lockBlock, claimTxid string
		lockVout                       uint32
		funderBtcClaim                 string
		funderDone                     = make(chan struct{})
	)
	before := map[string]string{"taker": e.assetBal(t, "taker"), "maker": e.assetBal(t, "maker")}
	t.Logf("asset before: taker %s maker %s", before["taker"], before["maker"])

	switch dir {
	case "forward": // taker pays BTC and holds the secret; maker funds the asset
		payerWallet, funderWallet = "taker", "maker"
		secret := reorgSecret(t)
		top := &LiveXcOps{Swap: xchain.NewSwapBitcoin(btcT, seqT, xchain.NewHashLock(secret)), BTC: btcT, SEQ: seqT}
		mp := MakerForwardParams{
			NewOps: func(h []byte) (XcOps, error) {
				return &LiveXcOps{Swap: xchain.NewSwapBitcoin(btcM, seqM, xchain.NewHashLockFromHash(h)), BTC: btcM, SEQ: seqM}, nil
			},
			Crypter: mc, BtcTip: btcM.BlockCount, SeqTip: seqM.BlockCount,
			AssetHex: e.asset, SeqAmount: seqAtoms, BtcAmount: btcSats, MinBTCConf: minConf, Timing: timing, Log: logf("maker"),
		}
		go func() {
			defer close(funderDone)
			res, err := RunMakerForward(mp, net.toMaker, net.makerSend)
			if res != nil {
				funderBtcClaim = res.BtcClaimTxid
			}
			t.Logf("maker driver returned: err=%v settled=%v btc_claim=%s", err, res != nil && res.Settled, funderBtcClaim)
		}()
		tp := TakerForwardParams{Ops: top, Crypter: tc, Secret: secret, BtcRefundKey: mustKey(t), SeqClaimKey: mustKey(t),
			ExpectAsset: e.asset, ExpectSeqAmount: seqAtoms, ExpectBtcAmount: btcSats, MinBTCConf: minConf, Timing: timing, Log: logf("taker")}
		res, err := RunTakerForward(tp, net.takerSend, net.takerRecv)
		if err != nil {
			t.Fatalf("taker: %v", err)
		}
		btcLegTxid, btcLegHeight = res.BtcLeg.Funded.TxID, res.BtcLegHeight
		lockTxid, lockVout, claimTxid = res.SeqLeg.Funded.TxID, res.SeqLeg.Funded.Vout, res.SeqClaimTxid
	case "reverse": // maker pays BTC and holds the secret; taker funds the asset
		payerWallet, funderWallet = "maker", "taker"
		var ta, x string
		reorgMust(t, e.seq.WithWallet("taker").Call(&ta, "getnewaddress"))
		reorgMust(t, e.seq.WithWallet("maker").CallNamed(&x, "sendtoaddress", map[string]interface{}{"address": ta, "amount": "1000", "assetlabel": e.asset, "fee_asset_label": e.asset}))
		e.mineSeq(1)
		t.Logf("seeded the taker with 1000 of the asset: %s", x)
		mp := MakerReverseParams{
			NewOps: func(secret []byte) (XcOps, error) {
				return &LiveXcOps{Swap: xchain.NewSwapBitcoin(btcM, seqM, xchain.NewHashLock(secret)), BTC: btcM, SEQ: seqM}, nil
			},
			Crypter: mc, BtcTip: btcM.BlockCount, SeqTip: seqM.BlockCount,
			AssetHex: e.asset, SeqAmount: seqAtoms, BtcAmount: btcSats, MinBTCConf: minConf, Timing: timing, Log: logf("maker"),
		}
		type mr struct {
			r   *MakerReverseResult
			err error
		}
		mch := make(chan mr, 1)
		go func() { r, err := RunMakerReverse(mp, net.toMaker, net.makerSend); mch <- mr{r, err} }()
		tp := TakerReverseParams{
			NewOps: func(h []byte) (XcOps, error) {
				return &LiveXcOps{Swap: xchain.NewSwapBitcoin(btcT, seqT, xchain.NewHashLockFromHash(h)), BTC: btcT, SEQ: seqT}, nil
			},
			Crypter: tc, BtcClaimKey: mustKey(t), SeqRefundKey: mustKey(t),
			ExpectAsset: e.asset, ExpectSeqAmount: seqAtoms, ExpectBtcAmount: btcSats, MinBTCConf: minConf, Timing: timing, Log: logf("taker"),
		}
		go func() {
			defer close(funderDone)
			res, err := RunTakerReverse(tp, net.takerSend, net.takerRecv)
			if res != nil {
				funderBtcClaim = res.BtcClaimTxid
			}
			t.Logf("taker driver returned: err=%v btc_claim=%s", err, funderBtcClaim)
		}()
		m := <-mch
		if m.err != nil {
			t.Fatalf("maker: %v", m.err)
		}
		btcLegTxid, btcLegHeight = m.r.BtcLeg.Funded.TxID, m.r.BtcLegHeight
		lockTxid, lockVout, claimTxid = m.r.SeqLeg.Funded.TxID, m.r.SeqLeg.Funded.Vout, m.r.SeqClaimTxid
	default:
		t.Fatalf("direction %q", dir)
	}

	lt, err := e.seqTx(lockTxid)
	reorgMust(t, err)
	lockBlock = lt.BlockHash
	lh := e.seqHdr(t, lockBlock)
	// Let the claim confirm.
	for i := 0; i < 40; i++ {
		if c, _ := e.seqTx(claimTxid); c != nil && c.BlockHash != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	ct, _ := e.seqTx(claimTxid)
	t.Logf("BTC leg %s confirmed at parent height %d", btcLegTxid, btcLegHeight)
	t.Logf("asset lock %s:%d in Sequentia block %s (height %d, anchor %d)", lockTxid, lockVout, lockBlock, lh.Height, lh.AnchorHeight)
	t.Logf("asset claim (reveals the secret) %s in block %s (anchor %d)", claimTxid, ct.BlockHash, e.seqHdr(t, ct.BlockHash).AnchorHeight)

	if attack == "seller" {
		// The funder has the secret: wait for its BTC claim to be broadcast and confirm.
		select {
		case <-funderDone:
		case <-time.After(3 * time.Minute):
			t.Fatal("the asset funder's driver did not finish its BTC claim")
		}
		for i := 0; i < 60; i++ {
			if c, _ := btcM.Confirmations(funderBtcClaim); c >= 1 || funderWallet == "taker" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	e.pauseSeq.Store(true)
	e.pauseBtc.Store(true)
	time.Sleep(time.Second)
	tip := e.btcTip(t)
	t.Logf("before the reorg: Bitcoin tip %d, BTC leg depth %d, lock anchor %d", tip, tip-btcLegHeight+1, lh.AnchorHeight)

	switch attack {
	case "buyer":
		e.reorgBtc(t, btcLegHeight, payerWallet, btcLegTxid)
	case "seller":
		if lh.AnchorHeight <= btcLegHeight {
			t.Logf("lock anchors at the BTC leg's own height; a reorg of it takes the leg too: no seller attack on this run")
			return
		}
		e.reorgBtc(t, lh.AnchorHeight, "", "")
	}
	e.waitGone(t, lockBlock)
	e.where(t, "lock ", lockTxid)
	e.where(t, "claim", claimTxid)
	if attack == "seller" || defense == "respend" {
		rs, err := e.respend(t, funderWallet, lockTxid, lockVout)
		t.Logf("asset funder (%s) respends its lock: txid=%s err=%v", funderWallet, rs, err)
		if err == nil {
			t.Fatalf("STOLEN: the honest mempool took the asset funder's respend %s of its returned lock", rs)
		}
		if !strings.Contains(err.Error(), "txn-mempool-conflict") {
			t.Fatalf("the respend was refused for another reason: %v", err)
		}
		e.where(t, "lock ", lockTxid)
		e.where(t, "claim", claimTxid)
		var x string
		err = e.seq.Call(&x, "sendrawtransaction", lt.Hex)
		t.Logf("counterparty rebroadcasts the lock: %v", err)
	}
	e.pauseSeq.Store(false)
	e.pauseBtc.Store(false)
	time.Sleep(8 * time.Second)
	e.pauseSeq.Store(true)
	e.pauseBtc.Store(true)
	e.where(t, "lock ", lockTxid)
	e.where(t, "claim", claimTxid)
	var legConfs struct {
		Confirmations int64 `json:"confirmations"`
	}
	_ = e.btc.WithWallet(payerWallet).Call(&legConfs, "gettransaction", btcLegTxid)
	t.Logf("BTC leg confirmations now %d (negative: double-spent)", legConfs.Confirmations)
	if funderBtcClaim != "" {
		c, err := btcM.Confirmations(funderBtcClaim)
		if funderWallet == "taker" {
			c, err = btcT.Confirmations(funderBtcClaim)
		}
		t.Logf("asset funder's BTC claim %s confirmations %d err=%v", funderBtcClaim, c, err)
	}
	t.Logf("asset after: taker %s maker %s (before: taker %s maker %s)", e.assetBal(t, "taker"), e.assetBal(t, "maker"), before["taker"], before["maker"])
	if c, err := e.seqTx(claimTxid); err != nil || c.Confirmations < 1 {
		t.Fatalf("the buyer's claim %s is not confirmed after the reorganization (err %v)", claimTxid, err)
	}
	t.Logf("RESULT: the asset funder could not withdraw its lock; the buyer's claim %s is confirmed again", claimTxid)
}
