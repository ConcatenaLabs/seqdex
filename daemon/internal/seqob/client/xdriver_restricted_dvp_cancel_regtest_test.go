package client

// A trade the chain un-mined and that something then cancelled, followed by
// a second lift in which the taker adds the paying maker's coin from the
// cancelled trade (proof review PV8, B1). While the cancelled trade sits in
// the maker's wallet unmined, that coin is unspent on the chain and listed
// nowhere, so the maker's own check cannot know it as its own: the maker's
// wallet must still refuse to sign it.
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run TestDvPAttackCancelledTrade -v

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

func TestDvPAttackCancelledTrade(t *testing.T) {
	e := loadRtEnv(t)
	w := dvpxSetup(t, e, "C", 1000)
	var paused atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !paused.Load() {
				e.mine(t)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	defer paused.Store(false)
	maker, taker := w.maker, w.taker
	makerEnclave, _, _, err := maker.party.Enclave()
	must(t, err)
	takerEnclave, _, _, err := taker.party.Enclave()
	must(t, err)
	for i := 0; i < 3; i++ { // spare fee coins for the taker
		var addr, txid string
		must(t, taker.seq.RPC().Call(&addr, "getnewaddress"))
		must(t, e.seq.WithWallet("w").CallNamed(&txid, "sendtoaddress", map[string]interface{}{
			"address": addr, "amount": 50, "assetlabel": e.feeAsset, "fee_asset_label": "bitcoin"}))
	}
	// The taker buys 300 first, so it holds the asset to sell to the maker.
	{
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, _ := maker.party.Node.NewScript()
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, 300, 50000000), true, recv, mIn, mSend)
			done <- err
		}()
		_, err := RunTakerRestrictedDvP(w.dvp(taker, tc, 300, 50000000), w.makerAID, true, recv, tSend, tRecv)
		must(t, err)
		must(t, <-done)
		taker.holdingReaches(t, 300)
	}
	listed := func(s *rtSide) map[string]bool {
		var us []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		}
		must(t, s.seq.RPC().Call(&us, "listunspent", 0))
		m := map[string]bool{}
		for _, u := range us {
			m[fmt.Sprintf("%s:%d", u.Txid, u.Vout)] = true
		}
		return m
	}
	inChain := func(txid string) bool {
		bh, err := maker.seq.BlockHashOfTx(txid)
		return err == nil && bh != ""
	}

	// honestThenReorg: the maker buys 100 for 0.2 B, paying from its coin X;
	// the trade is mined and the maker returns. Then the trade's anchor is
	// invalidated, so the trade leaves the chain and returns to the mempool.
	// It returns the trade, X, and the taker's fee coin in it.
	honestThenReorg := func(t *testing.T, tag string) (string, restricted.Outpoint, restricted.Outpoint) {
		time.Sleep(3 * time.Second)
		before, takerBefore := listed(maker), listed(taker)
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, 100, 20000000), false, nil, mIn, mSend)
			done <- err
		}()
		t1, err := RunTakerRestrictedDvP(w.dvp(taker, tc, 100, 20000000), w.makerAID, false, nil, tSend, tRecv)
		if err != nil {
			select {
			case merr := <-done:
				t.Fatalf("%s: the honest trade failed: taker %v; maker %v", tag, err, merr)
			case <-time.After(time.Minute):
				t.Fatalf("%s: the honest trade failed: taker %v; the maker is still waiting", tag, err)
			}
		}
		must(t, <-done)
		b1 := w.minedIn(t, t1)
		paused.Store(true)
		time.Sleep(3 * time.Second)
		var tx struct {
			Vin []struct {
				Txid string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		}
		must(t, e.seq.Call(&tx, "getrawtransaction", t1, true))
		var x, fee *restricted.Outpoint
		for _, in := range tx.Vin {
			k := fmt.Sprintf("%s:%d", in.Txid, in.Vout)
			if before[k] {
				x = &restricted.Outpoint{Txid: in.Txid, Vout: in.Vout}
			}
			if takerBefore[k] {
				fee = &restricted.Outpoint{Txid: in.Txid, Vout: in.Vout}
			}
		}
		if x == nil || fee == nil {
			t.Fatalf("the trade's coins: maker %v taker %v", x, fee)
		}
		var hdr struct {
			AnchorHash string `json:"anchorhash"`
		}
		must(t, e.seq.Call(&hdr, "getblockheader", b1))
		var nothing interface{}
		must(t, e.btc.Call(&nothing, "invalidateblock", hdr.AnchorHash))
		for i := 0; i < 60 && inChain(t1); i++ {
			time.Sleep(time.Second)
		}
		if inChain(t1) {
			t.Fatalf("%s: the trade %s is still in the chain after its anchor was invalidated", tag, t1)
		}
		if !maker.party.Node.InMempool(t1) {
			t.Fatalf("%s: the un-mined trade %s did not return to the mempool", tag, t1)
		}
		e.rec(t, tag+"_reorg", map[string]interface{}{"t1": t1, "t1_block": b1, "anchor_invalidated": hdr.AnchorHash,
			"x": fmt.Sprintf("%s:%d", x.Txid, x.Vout), "taker_fee_coin": fmt.Sprintf("%s:%d", fee.Txid, fee.Vout)})
		return t1, *x, *fee
	}

	// relift: the taker lifts the maker's next buy quote and adds X to the
	// trade, paid to itself whole. It returns the maker's error, and the
	// broadcast's txid if the maker signed.
	relift := func(t *testing.T, x restricted.Outpoint) (error, string) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, 100, 20000000), false, nil, mIn, mSend)
			done <- err
		}()
		must(t, sendXc(&XcMsg{Type: XcDvpRequest, TakerAID: w.takerAID, SeqAmount: 100}, tc, tSend))
		m, err := recvXcType(tRecv, tc, XcDvpCoins, time.Minute)
		must(t, err)
		offered, err := coinsAt(taker.party.Node, m.DvpCoins)
		must(t, err)
		mchange, _ := hex.DecodeString(m.DvpChange)
		var oSum uint64
		for _, c := range offered {
			oSum += c.Value
		}
		for _, c := range offered {
			if c.Txid == x.Txid && c.Vout == x.Vout {
				t.Fatalf("the maker offered X itself; the test needs it to pay from another coin")
			}
		}
		xc, err := coinsAt(taker.party.Node, []restricted.Outpoint{x})
		must(t, err)
		enc, err := taker.party.Node.EnclaveCoins(takerEnclave, w.asset)
		must(t, err)
		var mine *restricted.Coin
		for i := range enc {
			if enc[i].Value >= 100 {
				mine = &enc[i]
				break
			}
		}
		if mine == nil {
			t.Fatalf("the taker's enclave holds no coin of 100: %+v", enc)
		}
		fees, err := taker.party.Node.WalletCoins(e.feeAsset, 20000)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(fees) }()
		var fSum uint64
		for _, c := range fees {
			fSum += c.Value
		}
		recvB, _ := taker.party.Node.NewScript()
		coins := append(append(append([]restricted.Coin{*mine}, offered...), xc[0]), fees...)
		outs := []restricted.Out{{Asset: w.a, Value: 100, Script: makerEnclave}}
		if mine.Value > 100 {
			outs = append(outs, restricted.Out{Asset: w.a, Value: mine.Value - 100, Script: takerEnclave})
		}
		outs = append(outs, restricted.Out{Asset: w.b, Value: 20000000, Script: recvB})
		if oSum > 20000000 {
			outs = append(outs, restricted.Out{Asset: w.b, Value: oSum - 20000000, Script: mchange})
		}
		outs = append(outs, restricted.Out{Asset: xc[0].Asset, Value: xc[0].Value, Script: recvB}) // X, whole
		outs = append(outs, restricted.Out{Asset: w.fa, Value: fSum - 20000, Script: recvB}, restricted.Out{Asset: w.fa, Value: 20000})
		u, err := restricted.Build(coins, outs, 0)
		must(t, err)
		for _, in := range u.Tx.Inputs {
			in.Sequence = restricted.Sequence
		}
		raw, _ := u.Hex()
		must(t, sendXc(&XcMsg{Type: XcDvpTx, DvpTx: raw}, tc, tSend))
		sm, err := recvXcType(tRecv, tc, XcDvpSigned, time.Minute)
		if err != nil {
			return <-done, ""
		}
		signed, err := restricted.ParseTx(sm.DvpTx)
		must(t, err)
		for i := range u.Tx.Inputs {
			if len(signed.Tx.Inputs[i].Witness) > 0 {
				u.Tx.Inputs[i].Witness = signed.Tx.Inputs[i].Witness
			}
		}
		must(t, taker.party.Node.SignWallet(u))
		exp := taker.party.Node.Now().Add(5 * time.Minute).Unix()
		sigs, es, err := taker.party.SellerSigs(u, []int{0}, exp)
		must(t, err)
		txid, berr := taker.party.Policy.BroadcastDvP(u, w.asset, w.takerAID, []int{0}, sigs, exp, es)
		if berr != nil {
			t.Fatalf("the maker signed X and the broadcast failed: %v", berr)
		}
		paused.Store(false)
		w.minedIn(t, txid)
		return <-done, txid
	}

	// -- B1 without a replacement: the node stops accepting the fee asset ------
	// The trade back in the mempool pays its fee in an asset that the node
	// then drops from its whitelist (principle 4: no asset stays accepted for
	// sure), which evicts it. No attacker is needed for this state.
	t.Run("evicted-trade-relift", func(t *testing.T) {
		t1, x, _ := honestThenReorg(t, "evicted")
		var rates map[string]int64
		must(t, e.seq.Call(&rates, "getfeeexchangerates"))
		fewer := map[string]int64{}
		for k, v := range rates {
			if k != e.feeAsset {
				fewer[k] = v
			}
		}
		var nothing interface{}
		must(t, e.seq.Call(&nothing, "setfeeexchangerates", fewer, false))
		evicted := !maker.party.Node.InMempool(t1)
		must(t, e.seq.Call(&nothing, "setfeeexchangerates", rates, false))
		if !evicted {
			t.Fatalf("delisting the fee asset did not evict the trade %s", t1)
		}
		xOut, err := maker.party.Node.CoinAt(x.Txid, x.Vout)
		must(t, err)
		owns, err := maker.party.Node.WalletOutpoints()
		must(t, err)
		xk := fmt.Sprintf("%s:%d", x.Txid, x.Vout)
		e.rec(t, "evicted_window", map[string]interface{}{"t1_evicted": evicted, "x": xk,
			"x_unspent_per_gettxout": xOut != nil, "x_in_maker_walletOwns": owns[xk]})
		if xOut == nil || owns[xk] {
			t.Fatalf("no window: X unspent %v, listed %v", xOut != nil, owns[xk])
		}
		merr, t2 := relift(t, x)
		e.rec(t, "evicted_relift", map[string]interface{}{"maker_err": fmt.Sprint(merr), "t2": t2})
		if t2 != "" {
			t.Fatalf("B1: the maker's wallet signed X, which it never offered, and %s was mined", t2)
		}
		if merr == nil || !strings.Contains(merr.Error(), "a coin of ours not offered") || !strings.Contains(merr.Error(), xk) {
			t.Fatalf("the maker refused for another reason: %v", merr)
		}
		paused.Store(false)
	})

	// -- PV8's route: a replacement, refused by relay, mined by a producer -------
	// The trade signals no replacement, so the taker's conflicting spend is
	// refused by the mempool; a producer may still mine it (consensus allows
	// it). Once a block holds the conflict, the maker's wallet lists X again,
	// and the maker's check refuses X before any signature.
	t.Run("reorg-replacement-refused-then-mined", func(t *testing.T) {
		t1, x, fee := honestThenReorg(t, "replaced")
		fc, err := taker.party.Node.CoinAt(fee.Txid, fee.Vout)
		must(t, err)
		if fc == nil { // spent in the mempool by the trade: read it from the chain
			var o struct {
				Value        float64 `json:"value"`
				Asset        string  `json:"asset"`
				ScriptPubKey struct {
					Hex string `json:"hex"`
				} `json:"scriptPubKey"`
			}
			must(t, e.seq.Call(&o, "gettxout", fee.Txid, fee.Vout, false))
			a, _ := restricted.ReverseHex(o.Asset)
			spk, _ := hex.DecodeString(o.ScriptPubKey.Hex)
			fc = &restricted.Coin{Txid: fee.Txid, Vout: fee.Vout, Asset: a, Value: uint64(o.Value*1e8 + 0.5), Script: spk}
		}
		dest, err := taker.party.Node.NewScript()
		must(t, err)
		r, err := restricted.Build([]restricted.Coin{*fc}, []restricted.Out{{Asset: fc.Asset, Value: fc.Value - 100000, Script: dest}, {Asset: fc.Asset, Value: 100000}}, 0)
		must(t, err)
		must(t, taker.party.Node.SignWallet(r))
		_, rerr := taker.party.Node.Broadcast(r)
		if rerr == nil || !strings.Contains(rerr.Error(), "txn-mempool-conflict") {
			t.Fatalf("the trade's replacement was not refused for its conflict: %v", rerr)
		}
		if !maker.party.Node.InMempool(t1) {
			t.Fatalf("the trade %s left the mempool", t1)
		}
		raw, _ := r.Hex()
		var addr string
		must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
		var gb struct {
			Hash string `json:"hash"`
		}
		must(t, e.seq.WithWallet("w").Call(&gb, "generateblock", addr, []string{raw}))
		time.Sleep(2 * time.Second)
		xk := fmt.Sprintf("%s:%d", x.Txid, x.Vout)
		owns, err := maker.party.Node.WalletOutpoints()
		must(t, err)
		e.rec(t, "replaced_in_a_block", map[string]interface{}{"replacement_relay_err": rerr.Error(), "replacement_block": gb.Hash,
			"t1_in_mempool_after": maker.party.Node.InMempool(t1), "x_in_maker_walletOwns": owns[xk]})
		if !owns[xk] {
			t.Fatalf("the maker's wallet does not list X once a block cancelled its trade")
		}
		// A coin larger than X, so the maker pays from it and not from X.
		var addr2, ftx string
		must(t, maker.seq.RPC().Call(&addr2, "getnewaddress"))
		must(t, e.seq.WithWallet("w").CallNamed(&ftx, "sendtoaddress", map[string]interface{}{
			"address": addr2, "amount": 8, "assetlabel": w.assetB, "fee_asset_label": "bitcoin"}))
		e.mine(t)
		e.mine(t)
		merr, t2 := relift(t, x)
		e.rec(t, "replaced_relift", map[string]interface{}{"maker_err": fmt.Sprint(merr), "t2": t2})
		if t2 != "" {
			t.Fatalf("the maker's wallet signed X, which it never offered, and %s was mined", t2)
		}
		if merr == nil || !strings.Contains(merr.Error(), "the trade spends "+xk+", a coin of ours not offered") {
			t.Fatalf("the maker refused for another reason: %v", merr)
		}
		paused.Store(false)
	})
}
