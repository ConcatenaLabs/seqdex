package client

// A lying counterparty against the honest RunMakerRestrictedDvP and
// RunTakerRestrictedDvP, on real nodes (the regtest harness of
// xdriver_restricted_regtest_test.go). Each lie, written by the proof review
// of the asset-for-asset trade, is a valid transaction that a block takes:
// the honest side must refuse to sign it.
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run TestDvPAttack -v

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

type dvpxWorld struct {
	e                  *rtEnv
	asset, assetB      string
	a, b, fa           [32]byte
	maker, taker       *rtSide
	makerAID, takerAID string
}

func dvpxSetup(t *testing.T, e *rtEnv, tag string, atoms uint64) *dvpxWorld {
	e.mine(t)
	makerAID, makerKey := e.account(t)
	takerAID, takerKey := e.account(t)
	issuerAID, _ := e.account(t)
	var issued struct {
		Asset string `json:"asset"`
	}
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "DvPAttack " + tag, "ticker": "PV" + tag,
		"precision": 0, "atoms": atoms, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": atoms}, &issued)
	var bi struct {
		Asset string `json:"asset"`
	}
	must(t, e.seq.WithWallet("w").CallNamed(&bi, "issueasset", map[string]interface{}{"assetamount": 1000, "tokenamount": 0,
		"blind": false, "fee_asset": "bitcoin"}))
	w := &dvpxWorld{e: e, asset: issued.Asset, assetB: bi.Asset, makerAID: makerAID, takerAID: takerAID}
	w.maker = e.side(t, "dvpxm"+tag, makerAID, makerKey, w.asset)
	w.taker = e.side(t, "dvpxt"+tag, takerAID, takerKey, w.asset)
	for _, s := range []*rtSide{w.maker, w.taker} {
		var addr, txid string
		must(t, s.seq.RPC().Call(&addr, "getnewaddress"))
		must(t, e.seq.WithWallet("w").CallNamed(&txid, "sendtoaddress", map[string]interface{}{
			"address": addr, "amount": 10, "assetlabel": w.assetB, "fee_asset_label": "bitcoin"}))
	}
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	w.a, _ = restricted.ReverseHex(w.asset)
	w.b, _ = restricted.ReverseHex(w.assetB)
	w.fa, _ = restricted.ReverseHex(e.feeAsset)
	e.rec(t, "assets_"+tag, map[string]string{"restricted": w.asset, "b": w.assetB, "maker": makerAID, "taker": takerAID})
	return w
}

func (w *dvpxWorld) dvp(s *rtSide, c *Crypter, value, price uint64) *RestrictedDvP {
	return &RestrictedDvP{Party: s.party, Crypter: c, Value: value, Price: price, AssetB: w.assetB, FeeAsset: w.e.feeAsset,
		RfaPerVb: 2, Wait: 90 * time.Second, Timing: XcTiming{Poll: time.Second},
		Log: func(f string, a ...interface{}) { fmt.Printf("    "+f+"\n", a...) }}
}

func (w *dvpxWorld) bHeld(t *testing.T, s *rtSide) uint64 {
	var bal map[string]float64
	must(t, s.seq.RPC().Call(&bal, "getbalance"))
	return uint64(bal[w.assetB]*1e8 + 0.5)
}

func (w *dvpxWorld) minedIn(t *testing.T, txid string) string {
	for i := 0; i < 90; i++ {
		if bh, err := w.maker.seq.BlockHashOfTx(txid); err == nil && bh != "" {
			return bh
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s was not mined", txid)
	return ""
}

// TestDvPAttackDvPLiars: A1 the taker adds a maker enclave coin it was not offered;
// A2 a buying maker names the taker's own wallet coin as its payment.
func TestDvPAttackDvPLiars(t *testing.T) {
	e := loadRtEnv(t)
	w := dvpxSetup(t, e, "L", 1000)
	_ = w
	ctxStop := make(chan struct{})
	defer close(ctxStop)
	go func() {
		for {
			select {
			case <-ctxStop:
				return
			default:
			}
			e.mine(t)
			time.Sleep(500 * time.Millisecond)
		}
	}()
	maker, taker := w.maker, w.taker
	makerEnclave, _, _, err := maker.party.Enclave()
	must(t, err)
	takerEnclave, _, _, err := taker.party.Enclave()
	must(t, err)

	// -- honest trades first, so the maker holds two enclave coins and has seen
	// the taker's change ------------------------------------------------------
	var s1tx string
	var s1recv []byte
	t.Run("honest-sell-300", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, err := maker.party.Node.NewScript()
		must(t, err)
		s1recv = recv
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, 300, 50000000), true, recv, mIn, mSend)
			done <- err
		}()
		txid, err := RunTakerRestrictedDvP(w.dvp(taker, tc, 300, 50000000), w.makerAID, true, recv, tSend, tRecv)
		must(t, err)
		must(t, <-done)
		s1tx = txid
		e.rec(t, "L_honest_sell_300", txid)
	})
	t.Run("honest-buy-100", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, 100, 20000000), false, nil, mIn, mSend)
			done <- err
		}()
		txid, err := RunTakerRestrictedDvP(w.dvp(taker, tc, 100, 20000000), w.makerAID, false, nil, tSend, tRecv)
		must(t, err)
		must(t, <-done)
		e.rec(t, "L_honest_buy_100", txid)
	})
	if got := maker.holdingReaches(t, 800); got != 800 {
		t.Fatalf("maker holds %d, want 800", got)
	}

	// -- A2: a buying maker names the taker's own coin as its payment --------
	t.Run("A2-maker-names-taker-coin", func(t *testing.T) {
		// The taker's change of B from the honest sell, which the maker saw.
		var tx struct {
			Vout []struct {
				N            uint32  `json:"n"`
				Asset        string  `json:"asset"`
				Value        float64 `json:"value"`
				ScriptPubKey struct {
					Hex string `json:"hex"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		}
		must(t, e.seq.Call(&tx, "getrawtransaction", s1tx, true))
		var victim *restricted.Outpoint
		for _, o := range tx.Vout {
			if o.Asset == w.assetB && o.ScriptPubKey.Hex != hex.EncodeToString(s1recv) {
				victim = &restricted.Outpoint{Txid: s1tx, Vout: o.N}
			}
		}
		if victim == nil {
			t.Fatal("no taker change of B in the honest sell")
		}
		vc, err := maker.party.Node.CoinAt(victim.Txid, victim.Vout)
		must(t, err)
		if vc == nil {
			t.Fatalf("the taker's change %v is spent", victim)
		}
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		mRecv := chanRecv(mIn)
		makerBefore, takerBefore := maker.holding(t), taker.holding(t)
		tbBefore, mbBefore := w.bHeld(t, taker), w.bHeld(t, maker)
		const value, price = 100, 20000000
		mchange, err := maker.party.Node.NewScript()
		must(t, err)
		attack := make(chan error, 1)
		go func() {
			if _, err := recvXcType(mRecv, mc, XcDvpRequest, time.Minute); err != nil {
				attack <- err
				return
			}
			if err := sendXc(&XcMsg{Type: XcDvpCoins, DvpCoins: []restricted.Outpoint{*victim}, DvpChange: hex.EncodeToString(mchange)}, mc, mSend); err != nil {
				attack <- err
				return
			}
			m, err := recvXcType(mRecv, mc, XcDvpTx, time.Minute)
			if err != nil {
				attack <- err
				return
			}
			attack <- sendXc(&XcMsg{Type: XcDvpSigned, DvpTx: m.DvpTx}, mc, mSend) // signs nothing
		}()
		txid, err := RunTakerRestrictedDvP(w.dvp(taker, tc, value, price), w.makerAID, false, nil, tSend, tRecv)
		aerr := <-attack // the attacker meets the taker's refusal
		if err != nil {
			e.rec(t, "A2_maker_names_taker_coin", map[string]interface{}{"taker_refused": err.Error()})
			if !strings.Contains(err.Error(), "a coin of ours") {
				t.Fatalf("A2: the taker refused for another reason: %v", err)
			}
			return
		}
		must(t, aerr)
		bh := w.minedIn(t, txid)
		mAfter := maker.holdingReaches(t, makerBefore+value)
		var res struct {
			Vout []struct {
				Asset        string  `json:"asset"`
				Value        float64 `json:"value"`
				ScriptPubKey struct {
					Hex string `json:"hex"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
			Vin []struct {
				Txid string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		}
		must(t, e.seq.Call(&res, "getrawtransaction", txid, true))
		var toMakerChange float64
		for _, o := range res.Vout {
			if o.ScriptPubKey.Hex == hex.EncodeToString(mchange) {
				toMakerChange += o.Value
			}
		}
		e.rec(t, "A2_maker_names_taker_coin", map[string]interface{}{"txid": txid, "block": bh,
			"victim_coin":          fmt.Sprintf("%s:%d (%d of B, the taker's own)", victim.Txid, victim.Vout, vc.Value),
			"maker_enclave_before": makerBefore, "maker_enclave_after": mAfter,
			"taker_enclave_before": takerBefore, "taker_enclave_after": taker.holding(t),
			"taker_B_before": tbBefore, "taker_B_after": w.bHeld(t, taker), "maker_B_before": mbBefore, "maker_B_after": w.bHeld(t, maker),
			"paid_to_maker_change_script": toMakerChange, "inputs": res.Vin})
		t.Errorf("A2: the honest side signed a lie, and a block took it")
	})
	// -- A1: the taker spends a maker enclave coin it was not offered ---------
	t.Run("A1-extra-enclave-coin", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, err := maker.party.Node.NewScript()
		must(t, err)
		makerBefore, takerBefore := maker.holding(t), taker.holding(t)
		bBefore := w.bHeld(t, maker)
		const value, price = 100, 10000000
		done := make(chan error, 1)
		var mtx string
		go func() {
			var err error
			mtx, err = RunMakerRestrictedDvP(w.dvp(maker, mc, value, price), true, recv, mIn, mSend)
			done <- err
		}()
		must(t, sendXc(&XcMsg{Type: XcDvpRequest, TakerAID: w.takerAID, SeqAmount: value}, tc, tSend))
		m, err := recvXcType(tRecv, tc, XcDvpCoins, time.Minute)
		must(t, err)
		offered, err := coinsAt(taker.party.Node, m.DvpCoins)
		must(t, err)
		all, err := taker.party.Node.EnclaveCoins(makerEnclave, w.asset) // public: the enclave is on the chain
		must(t, err)
		off := map[string]bool{}
		var offSum uint64
		for _, c := range offered {
			off[c.Txid+fmt.Sprint(c.Vout)] = true
			offSum += c.Value
		}
		var extra []restricted.Coin
		var extraSum uint64
		for _, c := range all {
			if !off[c.Txid+fmt.Sprint(c.Vout)] {
				extra = append(extra, c)
				extraSum += c.Value
			}
		}
		if len(extra) == 0 {
			t.Fatalf("no unoffered maker coin: %+v", all)
		}
		fee, err := taker.party.Node.FeeAtoms(e.feeAsset, 1500, 2)
		must(t, err)
		pay, err := taker.party.Node.WalletCoins(w.assetB, price)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(pay) }()
		fees, err := taker.party.Node.WalletCoins(e.feeAsset, fee)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(fees) }()
		var paySum, feeSum uint64
		for _, c := range pay {
			paySum += c.Value
		}
		for _, c := range fees {
			feeSum += c.Value
		}
		ch, err := taker.party.Node.NewScript()
		must(t, err)
		outs := []restricted.Out{{Asset: w.a, Value: value + extraSum, Script: takerEnclave}, {Asset: w.b, Value: price, Script: recv}}
		if offSum > value {
			outs = append(outs, restricted.Out{Asset: w.a, Value: offSum - value, Script: makerEnclave})
		}
		if paySum > price {
			outs = append(outs, restricted.Out{Asset: w.b, Value: paySum - price, Script: ch})
		}
		if feeSum > fee {
			outs = append(outs, restricted.Out{Asset: w.fa, Value: feeSum - fee, Script: ch})
		}
		outs = append(outs, restricted.Out{Asset: w.fa, Value: fee})
		coins := append(append(append(append([]restricted.Coin{}, offered...), extra...), pay...), fees...)
		u, err := restricted.Build(coins, outs, 0)
		must(t, err)
		var sellerIns []int
		for i := range offered {
			sellerIns = append(sellerIns, i)
		}
		for i := range extra {
			sellerIns = append(sellerIns, len(offered)+i)
		}
		raw, _ := u.Hex()
		must(t, sendXc(&XcMsg{Type: XcDvpTx, DvpTx: raw}, tc, tSend))
		sm, err := recvXcType(tRecv, tc, XcDvpSigned, time.Minute)
		if err != nil {
			merr := <-done
			e.rec(t, "A1_extra_enclave_coin", map[string]interface{}{"maker_refused": fmt.Sprint(merr)})
			if merr == nil || !strings.Contains(merr.Error(), "not offered") {
				t.Fatalf("A1: the maker refused for another reason: %v", merr)
			}
			return
		}
		must(t, taker.party.Node.SignWallet(u))
		txid, err := taker.party.Policy.BroadcastDvP(u, w.asset, w.makerAID, sellerIns, sm.UserSigs, sm.ExpiresAt, sm.ExpirySig)
		if err != nil {
			t.Fatalf("A1: the policy server refused: %v", err)
		}
		_ = sendXc(&XcMsg{Type: XcDvpDone, SettleTxid: txid}, tc, tSend)
		bh := w.minedIn(t, txid)
		merr := <-done
		mAfter := maker.holdingReaches(t, makerBefore-value-extraSum)
		tAfter := taker.holding(t)
		e.rec(t, "A1_extra_enclave_coin", map[string]interface{}{"txid": txid, "block": bh, "maker_returned": mtx, "maker_err": fmt.Sprint(merr),
			"offered": offSum, "taken_unoffered": extraSum, "agreed_value": value, "price_paid": price,
			"maker_enclave_before": makerBefore, "maker_enclave_after": mAfter, "taker_enclave_before": takerBefore, "taker_enclave_after": tAfter,
			"maker_B_gain": int64(w.bHeld(t, maker)) - int64(bBefore), "user_sigs_from_maker": len(sm.UserSigs)})
		t.Errorf("A1: the honest side signed a lie, and a block took it")
	})

}

// TestDvPAttackGatedCollision: the maker's expired gated order's keyless refund sits
// in the trade, and its output (home to the maker's enclave) is counted as the
// maker's change (A3, the maker sells) or as the asset delivered (A4, the maker
// buys).
func TestDvPAttackGatedCollision(t *testing.T) {
	e := loadRtEnv(t)
	w := dvpxSetup(t, e, "G", 1000)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.mine(t)
			time.Sleep(500 * time.Millisecond)
		}
	}()
	maker, taker := w.maker, w.taker
	makerEnclave, _, _, err := maker.party.Enclave()
	must(t, err)
	takerEnclave, _, _, err := taker.party.Enclave()
	must(t, err)
	// The taker needs enclave coins for nothing here; the maker places two
	// gated orders of 300 and keeps 400.
	expiry := uint32(maker.party.Node.Now().Add(2 * time.Hour).Unix())
	var placed []*restricted.Placed
	for i := 0; i < 2; i++ {
		pl, err := maker.party.PlaceGated(restricted.GatedTerms{AssetB: e.feeAsset, Value: 300, RateNum: 2, RateDen: 1,
			MinLot: 100, Expiry: expiry, FeeAsset: e.feeAsset}, 2)
		must(t, err)
		w.minedIn(t, pl.Txid)
		placed = append(placed, pl)
	}
	if got := maker.holdingReaches(t, 400); got != 400 {
		t.Fatalf("maker enclave %d, want 400", got)
	}
	e.rec(t, "G_placed", map[string]interface{}{"o1": placed[0].Txid, "o2": placed[1].Txid, "expiry": expiry})
	// Past the expiry (the refunds open; nobody has sent them home yet).
	e.mineMu.Lock()
	var ok interface{}
	must(t, e.seq.Call(&ok, "setmocktime", int64(expiry)+600))
	e.mineMu.Unlock()
	for i := 0; i < 120; i++ {
		if mt, _ := maker.party.Node.MedianTime(); mt > int64(expiry)+60 {
			break
		}
		time.Sleep(time.Second)
	}
	refundIn := func(pl *restricted.Placed) restricted.Coin {
		c, err := maker.party.Node.CoinAt(pl.Txid, pl.Vout)
		must(t, err)
		if c == nil {
			t.Fatalf("order %s:%d is spent", pl.Txid, pl.Vout)
		}
		return *c
	}
	refundWitness := func(pl *restricted.Placed) [][]byte {
		tree, err := pl.Order.Tree()
		must(t, err)
		return [][]byte{tree.RefundLeaf, tree.RefundControl}
	}
	home := func(pl *restricted.Placed) []byte { return append([]byte{0x51, 0x20}, pl.Order.MakerEnclave[:]...) }
	if string(home(placed[0])) != string(makerEnclave) {
		t.Fatalf("the refund's home %x is not the maker's enclave %x", home(placed[0]), makerEnclave)
	}

	// -- A3: the maker sells 100; its refund's output is counted as its change
	t.Run("A3-refund-counted-as-change", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, err := maker.party.Node.NewScript()
		must(t, err)
		pl := placed[0]
		gc := refundIn(pl)
		makerBefore, takerBefore := maker.holding(t), taker.holding(t)
		const value, price = 100, 10000000
		done := make(chan error, 1)
		go func() {
			_, err := RunMakerRestrictedDvP(w.dvp(maker, mc, value, price), true, recv, mIn, mSend)
			done <- err
		}()
		must(t, sendXc(&XcMsg{Type: XcDvpRequest, TakerAID: w.takerAID, SeqAmount: value}, tc, tSend))
		m, err := recvXcType(tRecv, tc, XcDvpCoins, time.Minute)
		must(t, err)
		offered, err := coinsAt(taker.party.Node, m.DvpCoins)
		must(t, err)
		var offSum uint64
		for _, c := range offered {
			offSum += c.Value
		}
		fee, err := taker.party.Node.FeeAtoms(e.feeAsset, 1500, 2)
		must(t, err)
		pay, err := taker.party.Node.WalletCoins(w.assetB, price)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(pay) }()
		fees, err := taker.party.Node.WalletCoins(e.feeAsset, fee)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(fees) }()
		var paySum, feeSum uint64
		for _, c := range pay {
			paySum += c.Value
		}
		for _, c := range fees {
			feeSum += c.Value
		}
		ch, err := taker.party.Node.NewScript()
		must(t, err)
		outs := []restricted.Out{
			{Asset: w.a, Value: gc.Value, Script: makerEnclave}, // the refund home, whole: also "the maker's change"
			{Asset: w.b, Value: price, Script: recv},
			{Asset: w.a, Value: offSum, Script: takerEnclave}, // everything offered, not 100
			{Asset: w.b, Value: paySum - price, Script: ch},
		}
		if feeSum > fee {
			outs = append(outs, restricted.Out{Asset: w.fa, Value: feeSum - fee, Script: ch})
		}
		outs = append(outs, restricted.Out{Asset: w.fa, Value: fee})
		coins := append(append(append([]restricted.Coin{gc}, offered...), pay...), fees...)
		u, err := restricted.Build(coins, outs, expiry)
		must(t, err)
		u.Tx.Inputs[0].Witness = refundWitness(pl)
		var sellerIns []int
		for i := range offered {
			sellerIns = append(sellerIns, 1+i)
		}
		raw, _ := u.Hex()
		must(t, sendXc(&XcMsg{Type: XcDvpTx, DvpTx: raw}, tc, tSend))
		sm, err := recvXcType(tRecv, tc, XcDvpSigned, time.Minute)
		if err != nil {
			merr := <-done
			e.rec(t, "A3_refund_counted_as_change", map[string]interface{}{"maker_refused": fmt.Sprint(merr)})
			if merr == nil || !strings.Contains(merr.Error(), "lock time") {
				t.Fatalf("the maker refused for another reason: %v", merr)
			}
			return
		}
		must(t, taker.party.Node.SignWallet(u))
		txid, err := taker.party.Policy.BroadcastDvP(u, w.asset, w.makerAID, sellerIns, sm.UserSigs, sm.ExpiresAt, sm.ExpirySig)
		if err != nil {
			e.rec(t, "A3_refund_counted_as_change", map[string]interface{}{"server_refused": err.Error()})
			return
		}
		_ = sendXc(&XcMsg{Type: XcDvpDone, SettleTxid: txid}, tc, tSend)
		bh := w.minedIn(t, txid)
		merr := <-done
		e.rec(t, "A3_refund_counted_as_change", map[string]interface{}{"txid": txid, "block": bh, "maker_err": fmt.Sprint(merr),
			"refund_value": gc.Value, "offered": offSum, "agreed_value": value, "taker_received": offSum,
			"maker_enclave_before_excl_order": makerBefore, "maker_enclave_after": maker.holdingReaches(t, makerBefore-offSum+gc.Value),
			"taker_enclave_before": takerBefore, "taker_enclave_after": taker.holding(t)})
		t.Errorf("A3: the honest side signed a lie, and a block took it")
	})

	// -- A4: the maker buys 100; its own refund is "the asset delivered" ------
	t.Run("A4-refund-counted-as-delivery", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		pl := placed[1]
		gc := refundIn(pl)
		makerBefore, takerBefore := maker.holding(t), taker.holding(t)
		tbBefore, mbBefore := w.bHeld(t, taker), w.bHeld(t, maker)
		const value, price = 100, 20000000
		done := make(chan error, 1)
		var mtx string
		go func() {
			var err error
			mtx, err = RunMakerRestrictedDvP(w.dvp(maker, mc, value, price), false, nil, mIn, mSend)
			done <- err
		}()
		must(t, sendXc(&XcMsg{Type: XcDvpRequest, TakerAID: w.takerAID, SeqAmount: value}, tc, tSend))
		m, err := recvXcType(tRecv, tc, XcDvpCoins, time.Minute)
		must(t, err)
		theirs, err := coinsAt(taker.party.Node, m.DvpCoins)
		must(t, err)
		mchange, _ := hex.DecodeString(m.DvpChange)
		var bSum uint64
		for _, c := range theirs {
			bSum += c.Value
		}
		fee, err := taker.party.Node.FeeAtoms(e.feeAsset, 1500, 2)
		must(t, err)
		fees, err := taker.party.Node.WalletCoins(e.feeAsset, fee)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(fees) }()
		var feeSum uint64
		for _, c := range fees {
			feeSum += c.Value
		}
		myRecv, err := taker.party.Node.NewScript()
		must(t, err)
		outs := []restricted.Out{
			{Asset: w.a, Value: gc.Value, Script: makerEnclave}, // the maker's own refund, as "delivery"
			{Asset: w.b, Value: price, Script: myRecv},
		}
		if bSum > price {
			outs = append(outs, restricted.Out{Asset: w.b, Value: bSum - price, Script: mchange})
		}
		if feeSum > fee {
			outs = append(outs, restricted.Out{Asset: w.fa, Value: feeSum - fee, Script: myRecv})
		}
		outs = append(outs, restricted.Out{Asset: w.fa, Value: fee})
		coins := append(append([]restricted.Coin{gc}, theirs...), fees...)
		u, err := restricted.Build(coins, outs, expiry)
		must(t, err)
		u.Tx.Inputs[0].Witness = refundWitness(pl)
		raw, _ := u.Hex()
		must(t, sendXc(&XcMsg{Type: XcDvpTx, DvpTx: raw}, tc, tSend))
		sm, err := recvXcType(tRecv, tc, XcDvpSigned, time.Minute)
		if err != nil {
			merr := <-done
			e.rec(t, "A4_refund_counted_as_delivery", map[string]interface{}{"maker_refused": fmt.Sprint(merr)})
			if merr == nil || !strings.Contains(merr.Error(), "lock time") {
				t.Fatalf("the maker refused for another reason: %v", merr)
			}
			return
		}
		signed, err := restricted.ParseTx(sm.DvpTx)
		must(t, err)
		for i := range u.Tx.Inputs {
			if len(signed.Tx.Inputs[i].Witness) > 0 {
				u.Tx.Inputs[i].Witness = signed.Tx.Inputs[i].Witness
			}
		}
		must(t, taker.party.Node.SignWallet(u))
		// No policy server: the taker broadcasts it itself.
		txid, err := taker.party.Node.Broadcast(u)
		if err != nil {
			e.rec(t, "A4_refund_counted_as_delivery", map[string]interface{}{"node_refused": err.Error()})
			t.Fatalf("A4 broadcast: %v", err)
		}
		_ = sendXc(&XcMsg{Type: XcDvpDone, SettleTxid: txid}, tc, tSend)
		bh := w.minedIn(t, txid)
		merr := <-done
		e.rec(t, "A4_refund_counted_as_delivery", map[string]interface{}{"txid": txid, "block": bh, "maker_returned": mtx, "maker_err": fmt.Sprint(merr),
			"refund_value": gc.Value, "agreed_value": value, "price": price,
			"maker_enclave_before_excl_order": makerBefore, "maker_enclave_after": maker.holdingReaches(t, makerBefore+gc.Value),
			"taker_enclave_before": takerBefore, "taker_enclave_after": taker.holding(t),
			"taker_B_before": tbBefore, "taker_B_after": w.bHeld(t, taker), "maker_B_before": mbBefore, "maker_B_after": w.bHeld(t, maker)})
		t.Errorf("A4: the honest side signed a lie, and a block took it")
	})
	// -- the lock-time rule's ground: no time-gated refund can sit in a
	// transaction of lock time 0. Forced into a block, the refund alone is
	// refused for its lock time, so a trade CheckTrade accepts never holds one.
	t.Run("refund-at-lock-time-0-in-a-block", func(t *testing.T) {
		pl := placed[1]
		gc := refundIn(pl)
		fee, err := maker.party.Node.FeeAtoms(e.feeAsset, 600, 2)
		must(t, err)
		fc, err := maker.party.Node.WalletCoins(e.feeAsset, fee)
		must(t, err)
		defer func() { _ = maker.party.Node.Unlock(fc) }()
		defer func() { _ = maker.party.Node.Unlock(fc) }()
		var sum uint64
		for _, c := range fc {
			sum += c.Value
		}
		ch, err := maker.party.Node.NewScript()
		must(t, err)
		outs := []restricted.Out{{Asset: gc.Asset, Value: gc.Value, Script: home(pl)}}
		if sum > fee {
			outs = append(outs, restricted.Out{Asset: w.fa, Value: sum - fee, Script: ch})
		}
		outs = append(outs, restricted.Out{Asset: w.fa, Value: fee})
		u, err := restricted.Build(append([]restricted.Coin{gc}, fc...), outs, 0)
		must(t, err)
		for _, in := range u.Tx.Inputs {
			in.Sequence = restricted.Sequence
		}
		u.Tx.Inputs[0].Witness = refundWitness(pl)
		must(t, maker.party.Node.SignWallet(u))
		raw, _ := u.Hex()
		var addr string
		must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
		var gb interface{}
		gerr := e.seq.WithWallet("w").Call(&gb, "generateblock", addr, []string{raw})
		e.rec(t, "refund_at_lock_time_0_in_a_block", fmt.Sprint(gerr))
		if gerr == nil || !strings.Contains(gerr.Error(), "Locktime requirement not satisfied") {
			t.Fatalf("a gated refund at lock time 0 was not refused in a block for its lock time: %v", gerr)
		}
	})
}

// TestDvPAttackHeldAndReorg: R an anchor reorg un-mines a trade; H a trade broadcast
// and left unmined in the mempool (producers skip it: modelled with
// prioritisetransaction) blocks the paying maker's respend and lands after the
// seller's expiry.
func TestDvPAttackHeldAndReorg(t *testing.T) {
	e := loadRtEnv(t)
	w := dvpxSetup(t, e, "H", 1000)
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
	maker, taker := w.maker, w.taker
	inBlock := func(txid string) string {
		bh, _ := maker.seq.BlockHashOfTx(txid)
		return bh
	}
	btcHeight := func() int64 {
		var h int64
		must(t, e.btc.Call(&h, "getblockcount"))
		return h
	}

	// -- R: anchor reorg ---------------------------------------------------------
	t.Run("R-anchor-reorg", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, err := maker.party.Node.NewScript()
		must(t, err)
		h0 := btcHeight()
		done := make(chan error, 1)
		var mtx string
		go func() {
			var err error
			mtx, err = RunMakerRestrictedDvP(w.dvp(maker, mc, 300, 50000000), true, recv, mIn, mSend)
			done <- err
		}()
		txid, err := RunTakerRestrictedDvP(w.dvp(taker, tc, 300, 50000000), w.makerAID, true, recv, tSend, tRecv)
		must(t, err)
		must(t, <-done)
		b1 := w.minedIn(t, txid)
		paused.Store(true)
		time.Sleep(3 * time.Second)
		var hdr struct {
			AnchorHeight int64  `json:"anchorheight"`
			AnchorHash   string `json:"anchorhash"`
		}
		must(t, e.seq.Call(&hdr, "getblockheader", b1))
		bhash := hdr.AnchorHash
		_ = h0
		var nothing interface{}
		must(t, e.btc.Call(&nothing, "invalidateblock", bhash))
		var after string
		for i := 0; i < 60; i++ {
			if after = inBlock(txid); after == "" {
				break
			}
			time.Sleep(time.Second)
		}
		var inPool bool
		var mp []string
		must(t, e.seq.Call(&mp, "getrawmempool"))
		for _, x := range mp {
			inPool = inPool || x == txid
		}
		takerMid, makerMid := taker.holding(t), maker.holding(t)
		paused.Store(false)
		b2 := w.minedIn(t, txid)
		e.rec(t, "R_anchor_reorg", map[string]interface{}{"txid": txid, "maker_returned": mtx, "first_block": b1,
			"btc_invalidated": fmt.Sprintf("the trade block's anchor %d %s", hdr.AnchorHeight, bhash), "block_after_invalidate": after, "back_in_mempool": inPool,
			"taker_enclave_unmined": takerMid, "maker_enclave_unmined": makerMid, "remined_block": b2,
			"taker_enclave_final": taker.holdingReaches(t, 300), "maker_enclave_final": maker.holding(t)})
	})

	// -- H1: the paying maker's respend is blocked by its trade in the mempool --
	t.Run("H1-buy-respend-blocked", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		md := w.dvp(maker, mc, 100, 20000000)
		md.Wait = 15 * time.Second
		done := make(chan error, 1)
		go func() { _, err := RunMakerRestrictedDvP(md, false, nil, mIn, mSend); done <- err }()
		td := w.dvp(taker, tc, 100, 20000000)
		td.BeforeBroadcast = func() { paused.Store(true); time.Sleep(4 * time.Second) }
		txid, err := RunTakerRestrictedDvP(td, w.makerAID, false, nil, tSend, tRecv)
		must(t, err)
		var ok bool
		must(t, e.seq.Call(&ok, "prioritisetransaction", txid, 0, -100000000))
		paused.Store(false)
		merr := <-done
		mErrAt := time.Now().UTC()
		time.Sleep(5 * time.Second)
		still := inBlock(txid)
		_ = e.seq.Call(&ok, "prioritisetransaction", txid, 0, 100000000)
		if merr != nil && strings.Contains(merr.Error(), "replaces it") {
			// The respend replaced the trade: it must never be mined now.
			for i := 0; i < 15; i++ {
				if inBlock(txid) != "" {
					t.Fatalf("H1: the maker reported the trade replaced, and it was mined: %v", merr)
				}
				time.Sleep(time.Second)
			}
			e.rec(t, "H1_buy_respend_blocked", map[string]interface{}{"txid": txid, "maker_err": merr.Error(), "replaced": true})
			return
		}
		if !errors.Is(merr, ErrDvPPending) {
			t.Fatalf("H1: the maker reported a failure while its signed payment could still be mined: %v", merr)
		}
		bh := w.minedIn(t, txid)
		e.rec(t, "H1_buy_respend_blocked", map[string]interface{}{"txid": txid, "maker_err": fmt.Sprint(merr), "maker_gave_up_at": mErrAt,
			"block_5s_after_maker_gave_up": still, "landed_in": bh, "landed_at": time.Now().UTC()})
	})

	// -- H2: a selling maker's trade lands after the seller's expiry ------------
	t.Run("H2-sell-lands-after-expiry", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv, err := maker.party.Node.NewScript()
		must(t, err)
		md := w.dvp(maker, mc, 100, 10000000)
		md.Expiry = 10 * time.Second
		md.Wait = 20 * time.Second
		done := make(chan error, 1)
		go func() { _, err := RunMakerRestrictedDvP(md, true, recv, mIn, mSend); done <- err }()
		td := w.dvp(taker, tc, 100, 10000000)
		var bAt int64
		td.BeforeBroadcast = func() { paused.Store(true); time.Sleep(4 * time.Second); bAt = maker.party.Node.Now().Unix() }
		txid, err := RunTakerRestrictedDvP(td, w.makerAID, true, recv, tSend, tRecv)
		must(t, err)
		var ok bool
		must(t, e.seq.Call(&ok, "prioritisetransaction", txid, 0, -100000000))
		paused.Store(false)
		merr := <-done
		if merr != nil && !errors.Is(merr, ErrDvPPending) {
			t.Errorf("H2: the maker reported a failure while its trade could still be mined: %v", merr)
		}
		time.Sleep(10 * time.Second)
		must(t, e.seq.Call(&ok, "prioritisetransaction", txid, 0, 100000000))
		bh := w.minedIn(t, txid)
		var blk struct {
			Time       int64 `json:"time"`
			MedianTime int64 `json:"mediantime"`
		}
		must(t, e.seq.Call(&blk, "getblock", bh))
		e.rec(t, "H2_sell_lands_after_expiry", map[string]interface{}{"txid": txid, "maker_err": fmt.Sprint(merr),
			"landed_in": bh, "block_time": blk.Time, "block_mtp": blk.MedianTime, "broadcast_at_node_now": bAt,
			"seller_expiry_at_most": bAt + 10, "lands_after_expiry_s": blk.MedianTime - (bAt + 10)})
	})
}
