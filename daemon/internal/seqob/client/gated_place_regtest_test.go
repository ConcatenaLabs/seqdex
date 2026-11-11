package client

// A maker's gated order against real nodes (see xdriver_restricted_regtest_test.go
// for the environment): placed through the policy server, filled in part by a
// taker through it, refused in a block when sent home before its expiry, and
// sent home after it from the remainder the fill left.
//
//	RESTRICTED_E2E_ENV=<dir>/env go test ./internal/seqob/client -run TestGatedPlacementRegtest -v -timeout 30m

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

func TestGatedPlacementRegtest(t *testing.T) {
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
	e.api(t, "POST", "/v1/issuer/assets", map[string]interface{}{"name": "Gated Bond", "ticker": "GTB",
		"precision": 0, "atoms": 1000, "clawback": true, "holder_aid": makerAID, "issuer_aid": issuerAID, "supply_cap": 1000}, &issued)
	asset := issued.Asset
	e.rec(t, "gated_asset", asset)
	maker := e.side(t, "gmaker", makerAID, makerKey, asset)
	taker := e.side(t, "gtaker", takerAID, takerKey, asset)
	for i := 0; i < 3; i++ {
		e.mine(t)
	}
	e.miner(t, ctx)

	// -- the maker places 600 at 2 fee-asset atoms each, lots of 100 ----------
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
	if oc == nil || oc.Value != 600 || !bytes.Equal(oc.Script, tree.ScriptPubKey) {
		t.Fatalf("the placement %s did not put 600 at the order's script: %+v", pl.Txid, oc)
	}
	e.rec(t, "gated_placement", map[string]interface{}{"txid": pl.Txid, "expiry": expiry})

	// A placement whose expiry is outside the server's window is refused before
	// anything is signed.
	if _, err := maker.party.PlaceGated(restricted.GatedTerms{AssetB: e.feeAsset, Value: 100, RateNum: 2, RateDen: 1,
		MinLot: 100, Expiry: uint32(time.Now().Add(10 * time.Minute).Unix()), FeeAsset: e.feeAsset}, 2); err == nil {
		t.Fatal("a placement expiring in 10 minutes was made")
	}

	// -- a taker fills 200 through the policy server --------------------------
	// The fill covenant at input 0: the maker's credit at output 0, the
	// remainder at output 1.
	plan := struct{ RequiredB, Remainder uint64 }{covenant.CeilPrice(200, 2, 1), 400}
	takerEnclave, _, _, err := taker.party.Enclave()
	must(t, err)
	fee, err := taker.party.Node.FeeAtoms(e.feeAsset, 1500, 2)
	must(t, err)
	pay, err := taker.party.Node.WalletCoins(e.feeAsset, plan.RequiredB+fee)
	must(t, err)
	var have uint64
	for _, c := range pay {
		have += c.Value
	}
	change, err := taker.party.Node.NewScript()
	must(t, err)
	fa, _ := restricted.ReverseHex(e.feeAsset)
	makerCredit := append([]byte{0x51, 0x20}, pl.Order.MakerProg...)
	u, err := restricted.Build(append([]restricted.Coin{*oc}, pay...), []restricted.Out{
		{Asset: fa, Value: plan.RequiredB, Script: makerCredit},
		{Asset: pl.Order.AssetA, Value: plan.Remainder, Script: tree.ScriptPubKey},
		{Asset: pl.Order.AssetA, Value: 200, Script: takerEnclave},
		{Asset: fa, Value: have - plan.RequiredB - fee, Script: change},
		{Asset: fa, Value: fee},
	}, 0)
	must(t, err)
	must(t, taker.party.Node.SignWallet(u))
	raw, _ := u.Hex()
	resp, err := taker.party.Policy.Cosign(restricted.CosignRequest{Tx: raw, Asset: asset, SenderAID: makerAID,
		OrderInputs: []restricted.OrderInput{{Input: 0}}, Broadcast: true})
	must(t, err)
	if resp.Txid == "" {
		t.Fatal("the policy server broadcast no fill")
	}
	if got := taker.holdingReaches(t, 200); got != 200 {
		t.Fatalf("the taker's enclave holds %d after the fill, want 200", got)
	}
	e.rec(t, "gated_fill_200", resp.Txid)

	// -- sent home before the expiry: refused in a block ----------------------
	rem, err := maker.party.Node.CoinAt(resp.Txid, 1)
	must(t, err)
	if rem == nil || rem.Value != 400 {
		t.Fatalf("the fill left %+v at the order, want 400", rem)
	}
	early, err := restricted.BuildRefund(restricted.RefundPlan{Coin: *rem, Home: append([]byte{0x51, 0x20}, pl.Order.MakerEnclave[:]...),
		Leaf: covenant.TapLeaf{Script: tree.RefundLeaf, Control: tree.RefundControl}, FeeCoin: pl.Reserve, TSeq: expiry})
	must(t, err)
	must(t, maker.party.Node.SignWallet(early))
	eraw, _ := early.Hex()
	var addr string
	must(t, e.seq.WithWallet("w").Call(&addr, "getnewaddress"))
	var gb interface{}
	gerr := e.seq.WithWallet("w").Call(&gb, "generateblock", addr, []string{eraw})
	if gerr == nil || !strings.Contains(gerr.Error(), "non-final") {
		t.Fatalf("a refund before the expiry was not refused in a block for its lock time: %v", gerr)
	}
	e.rec(t, "gated_refund_before_expiry_in_a_block", gerr.Error())
	if _, err := maker.party.RefundGated(pl); err == nil || !strings.Contains(err.Error(), "opens at the expiry") {
		t.Fatalf("RefundGated before the expiry: %v", err)
	}

	// -- after the expiry, the remainder goes home ---------------------------
	before := maker.holding(t)
	e.mineMu.Lock()
	var ok interface{}
	must(t, e.seq.Call(&ok, "setmocktime", int64(expiry)+600))
	e.mineMu.Unlock()
	var home string
	for i := 0; i < 60 && home == ""; i++ {
		home, err = maker.party.RefundGated(pl)
		if err != nil {
			t.Logf("refund: %v", err)
			home = ""
		}
		time.Sleep(time.Second)
	}
	if home == "" {
		t.Fatal("the remainder was not sent home after the expiry")
	}
	if got := maker.holdingReaches(t, before+400); got != before+400 {
		t.Fatalf("the maker's enclave holds %d after the refund, want %d", got, before+400)
	}
	// Once home, nothing is owed.
	if again, err := maker.party.RefundGated(pl); err != nil || again != "" {
		t.Fatalf("a second refund: %q %v", again, err)
	}
	e.rec(t, "gated_refund_after_expiry", home)
}
