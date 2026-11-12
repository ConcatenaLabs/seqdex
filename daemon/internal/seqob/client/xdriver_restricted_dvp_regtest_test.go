package client

// Delivery against payment for a restricted asset, over the courier, against
// real nodes (see xdriver_restricted_regtest_test.go for the environment).
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run TestRestrictedDvPRegtest -v -timeout 30m

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

func TestRestrictedDvPRegtest(t *testing.T) {
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
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "DvP Bond", "ticker": "DVB",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	asset := issued.Asset
	// B: a plain asset, neither the restricted one nor the fee asset.
	var bi struct {
		Asset string `json:"asset"`
	}
	must(t, e.seq.WithWallet("w").CallNamed(&bi, "issueasset", map[string]interface{}{"assetamount": 1000, "tokenamount": 0,
		"blind": false, "fee_asset": "bitcoin"}))
	assetB := bi.Asset
	e.rec(t, "dvp_assets", map[string]string{"restricted": asset, "b": assetB})
	maker := e.side(t, "dmaker", makerAID, makerKey, asset)
	taker := e.side(t, "dtaker", takerAID, takerKey, asset)
	for _, s := range []*rtSide{maker, taker} {
		var addr, txid string
		must(t, s.seq.RPC().Call(&addr, "getnewaddress"))
		must(t, e.seq.WithWallet("w").CallNamed(&txid, "sendtoaddress", map[string]interface{}{
			"address": addr, "amount": 10, "assetlabel": assetB, "fee_asset_label": "bitcoin"}))
	}
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	e.miner(t, ctx)

	dvp := func(s *rtSide, c *Crypter, value, price uint64) *RestrictedDvP {
		return &RestrictedDvP{Party: s.party, Crypter: c, Value: value, Price: price, AssetB: assetB, FeeAsset: e.feeAsset,
			RfaPerVb: 2, Wait: 90 * time.Second, Timing: XcTiming{Poll: time.Second},
			Log: func(f string, a ...interface{}) { t.Logf(f, a...) }}
	}
	recvScript := func(s *rtSide) []byte {
		spk, err := s.party.Node.NewScript()
		must(t, err)
		return spk
	}
	bHeld := func(s *rtSide) uint64 {
		var bal map[string]float64
		must(t, s.seq.RPC().Call(&bal, "getbalance"))
		return uint64(bal[assetB]*1e8 + 0.5)
	}

	// -- 1. the maker sells 300 for 0.5 B; the taker pays ---------------------
	t.Run("maker-sells", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		recv := recvScript(maker)
		bBefore := bHeld(maker)
		var mtx string
		var merr error
		done := make(chan struct{})
		go func() {
			mtx, merr = RunMakerRestrictedDvP(dvp(maker, mc, 300, 50000000), true, recv, mIn, mSend)
			close(done)
		}()
		txid, err := RunTakerRestrictedDvP(dvp(taker, tc, 300, 50000000), makerAID, true, recv, tSend, tRecv)
		if err != nil {
			t.Fatalf("taker: %v", err)
		}
		<-done
		if merr != nil || mtx != txid {
			t.Fatalf("maker: %v (%s vs %s)", merr, mtx, txid)
		}
		if got := taker.holdingReaches(t, 300); got != 300 {
			t.Fatalf("the taker's enclave holds %d, want 300", got)
		}
		if got := bHeld(maker); got != bBefore+50000000 {
			t.Fatalf("the maker holds %d B, want %d", got, bBefore+50000000)
		}
		e.rec(t, "dvp_maker_sells", txid)
	})

	// -- 2. the maker buys 100 back for 0.2 B; the taker sells -----------------
	t.Run("maker-buys", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		before := maker.holding(t)
		var mtx string
		var merr error
		done := make(chan struct{})
		go func() {
			mtx, merr = RunMakerRestrictedDvP(dvp(maker, mc, 100, 20000000), false, nil, mIn, mSend)
			close(done)
		}()
		txid, err := RunTakerRestrictedDvP(dvp(taker, tc, 100, 20000000), makerAID, false, nil, tSend, tRecv)
		if err != nil {
			t.Fatalf("taker: %v", err)
		}
		<-done
		if merr != nil || mtx != txid {
			t.Fatalf("maker: %v (%s vs %s)", merr, mtx, txid)
		}
		if got := maker.holdingReaches(t, before+100); got != before+100 {
			t.Fatalf("the maker's enclave holds %d, want %d", got, before+100)
		}
		e.rec(t, "dvp_maker_buys", txid)
	})

	// -- 3. a paying maker whose signed trade is held respends its coin --------
	t.Run("held-trade-coin-respent", func(t *testing.T) {
		tSend, tRecv, mIn, mSend, tc, mc := pipe(t)
		md := dvp(maker, mc, 100, 20000000)
		md.Wait = 5 * time.Second
		var merr error
		done := make(chan struct{})
		go func() { _, merr = RunMakerRestrictedDvP(md, false, nil, mIn, mSend); close(done) }()
		td := dvp(taker, tc, 100, 20000000)
		td.BeforeBroadcast = func() { <-done }
		_, err := RunTakerRestrictedDvP(td, makerAID, false, nil, tSend, tRecv)
		if merr == nil || errors.Is(merr, ErrDvPPending) || !strings.Contains(merr.Error(), "respent home") {
			t.Fatalf("the maker did not respend its coin: %v", merr)
		}
		if err == nil {
			t.Fatal("a held trade was broadcast after the payer respent its coin")
		}
		e.rec(t, "dvp_held_trade", map[string]string{"maker": merr.Error(), "taker_broadcast": err.Error()})
	})

	// -- 4. what a maker refuses to sign ---------------------------------------
	t.Run("maker-refuses", func(t *testing.T) {
		enclave, _, _, err := maker.party.Enclave()
		must(t, err)
		takerEnclave, _, _, err := taker.party.Enclave()
		must(t, err)
		all, err := maker.party.Node.EnclaveCoins(enclave, asset)
		must(t, err)
		var coins []restricted.Coin
		for _, c := range all {
			if c.Value >= 200 {
				coins = []restricted.Coin{c}
				break
			}
		}
		if coins == nil {
			t.Fatalf("no enclave coin of 200 or more: %+v", all)
		}
		pay, err := taker.party.Node.WalletCoins(assetB, 10000000)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(pay) }()
		fees, err := taker.party.Node.WalletCoins(e.feeAsset, 100000)
		must(t, err)
		defer func() { _ = taker.party.Node.Unlock(fees) }()
		a, _ := restricted.ReverseHex(asset)
		b, _ := restricted.ReverseHex(assetB)
		fa, _ := restricted.ReverseHex(e.feeAsset)
		recv := recvScript(maker)
		ch := recvScript(taker)
		plan := restricted.DvP{Asset: a, Value: 100, AssetB: b, Price: 10000000, SellerEnclave: enclave, BuyerEnclave: takerEnclave,
			SellerRecv: recv, SellerCoins: coins[:1], BuyerCoins: pay, BuyerChange: ch, FeeCoins: fees, FeeAsset: fa, Fee: 3000, FeeChange: ch}
		owns, err := maker.party.Node.WalletOutpoints()
		must(t, err)
		check := func(p restricted.DvP, price uint64) error {
			u, err := restricted.BuildDvP(p)
			must(t, err)
			return restricted.CheckTrade(u, restricted.TradeSide{Offered: coins[:1], WalletOwns: owns, Ours: [][]byte{enclave, recv},
				Gives: map[[32]byte]uint64{a: 100}, Gets: []restricted.Out{{Asset: b, Value: price, Script: recv}}})
		}
		if err := check(plan, 10000000); err != nil {
			t.Fatalf("the agreed trade was refused: %v", err)
		}
		under := plan
		under.Price = 9999999
		uerr := check(under, 10000000)
		if uerr == nil || !strings.Contains(uerr.Error(), "does not pay") {
			t.Fatalf("an underpaying trade was not refused: %v", uerr)
		}
		more := plan
		more.Value = 101
		merr := check(more, 10000000)
		if merr == nil || !strings.Contains(merr.Error(), "more than the") {
			t.Fatalf("a trade taking more of the asset was not refused: %v", merr)
		}
		// A coin of the maker's wallet slipped in beside the taker's fee coins.
		var mw []restricted.Coin
		for i := 0; i < 30; i++ { // the respend above leaves its change unconfirmed for a block
			if mw, err = maker.party.Node.WalletCoins(e.feeAsset, 1000); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		must(t, err)
		defer func() { _ = maker.party.Node.Unlock(mw) }()
		owns, err = maker.party.Node.WalletOutpoints()
		must(t, err)
		sneak := plan
		sneak.FeeCoins = append(append([]restricted.Coin{}, fees...), mw...)
		serr := check(sneak, 10000000)
		if serr == nil || !strings.Contains(serr.Error(), "not offered") {
			t.Fatalf("a trade spending a coin of the maker's wallet was not refused: %v", serr)
		}
		b2, _ := json.Marshal(map[string]string{"underpay": uerr.Error(), "more_asset": merr.Error(), "wallet_coin": serr.Error()})
		e.rec(t, "dvp_refusals", string(b2))
	})
}
