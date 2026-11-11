package client

// A gated order's fill more than 50 blocks old: the maker's -resume still
// finds it and sends the remainder home after the expiry (proof review PV7,
// N8), against real nodes (see xdriver_restricted_regtest_test.go for the
// environment, one fresh run per test):
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run '^TestHorizonGatedRemainderHome$' -v -timeout 40m

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

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
