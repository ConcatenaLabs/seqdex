package restricted

import (
	"fmt"
	"strings"
	"testing"
)

// A trade laid out as BuildDvP lays it, and each lie PV8 mined against the
// check it replaced: every one is refused here, with its reason.

func tid(n byte) string { return fmt.Sprintf("%064x", n) }

var (
	tA, tB, tF   = [32]byte{0xaa}, [32]byte{0xbb}, [32]byte{0xff}
	sellerEnc    = []byte{0x51, 0x20, 1}
	buyerEnc     = []byte{0x51, 0x20, 2}
	sellerRecv   = []byte{0x00, 0x14, 3}
	buyerChange  = []byte{0x00, 0x14, 4}
	feeChange    = []byte{0x00, 0x14, 5}
	gatedRefund  = []byte{0x51, 0x20, 6}
	sellerCoin   = Coin{Txid: tid(1), Vout: 0, Asset: tA, Value: 400, Script: sellerEnc}
	sellerOther  = Coin{Txid: tid(2), Vout: 0, Asset: tA, Value: 800, Script: sellerEnc}
	buyerCoin    = Coin{Txid: tid(3), Vout: 0, Asset: tB, Value: 1000, Script: buyerChange}
	buyerFeeCoin = Coin{Txid: tid(4), Vout: 0, Asset: tF, Value: 5000, Script: feeChange}
)

func plan() DvP {
	return DvP{Asset: tA, Value: 100, AssetB: tB, Price: 10, SellerEnclave: sellerEnc, BuyerEnclave: buyerEnc,
		SellerRecv: sellerRecv, SellerCoins: []Coin{sellerCoin}, BuyerCoins: []Coin{buyerCoin}, BuyerChange: buyerChange,
		FeeCoins: []Coin{buyerFeeCoin}, FeeAsset: tF, Fee: 300, FeeChange: feeChange}
}

func sellerSide(walletOwns ...Coin) TradeSide {
	owns := map[string]bool{}
	for _, c := range walletOwns {
		owns[outpointKey(c.Txid, c.Vout)] = true
	}
	return TradeSide{Offered: []Coin{sellerCoin}, WalletOwns: owns, Ours: [][]byte{sellerEnc, sellerRecv},
		Gives: map[[32]byte]uint64{tA: 100}, Gets: []Out{{Asset: tB, Value: 10, Script: sellerRecv}}}
}

func buyerSide() TradeSide {
	return TradeSide{Offered: []Coin{buyerCoin, buyerFeeCoin}, WalletOwns: map[string]bool{
		outpointKey(buyerCoin.Txid, 0): true, outpointKey(buyerFeeCoin.Txid, 0): true},
		Ours: [][]byte{buyerEnc, buyerChange, feeChange}, Gives: map[[32]byte]uint64{tB: 10, tF: 300},
		Gets: []Out{{Asset: tA, Value: 100, Script: buyerEnc}}}
}

func build(t *testing.T, coins []Coin, outs []Out) *Unsigned {
	u, err := Build(coins, outs, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range u.Tx.Inputs {
		in.Sequence = Sequence
	}
	return u
}

func wantRefused(t *testing.T, err error, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Fatalf("want a refusal naming %q, got %v", why, err)
	}
}

func TestCheckTradeHonest(t *testing.T) {
	u, err := BuildDvP(plan())
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range u.Tx.Inputs {
		if in.Sequence != Sequence || u.Tx.Locktime != 0 {
			t.Fatalf("input %d: sequence %#x, lock time %d", i, in.Sequence, u.Tx.Locktime)
		}
	}
	if err := CheckTrade(u, sellerSide()); err != nil {
		t.Fatalf("seller: %v", err)
	}
	if err := CheckTrade(u, buyerSide()); err != nil {
		t.Fatalf("buyer: %v", err)
	}
	if got := OfferedInputs(u, []Coin{sellerCoin}); len(got) != 1 || got[0] != 0 {
		t.Fatalf("offered inputs %v", got)
	}
}

// A1: the taker adds the seller's unoffered enclave coin and takes it.
func TestCheckTradeRefusesAnUnofferedCoinAtOurScript(t *testing.T) {
	u := build(t, []Coin{sellerCoin, sellerOther, buyerCoin, buyerFeeCoin}, []Out{
		{tA, 900, buyerEnc}, {tB, 10, sellerRecv}, {tA, 300, sellerEnc}, {tB, 990, buyerChange}, {tF, 4700, feeChange}, {tF, 300, nil}})
	wantRefused(t, CheckTrade(u, sellerSide()), "at a script of ours, not offered")
	if got := OfferedInputs(u, []Coin{sellerCoin}); len(got) != 1 || got[0] != 0 {
		t.Fatalf("the seller would sign inputs %v, not only its offer", got)
	}
}

// A2: the maker names a coin of the taker's own wallet as its payment.
func TestCheckTradeRefusesOurWalletCoinNamedAsTheirs(t *testing.T) {
	takerCoin := Coin{Txid: tid(9), Vout: 1, Asset: tB, Value: 950, Script: []byte{0x00, 0x14, 9}}
	side := TradeSide{Offered: []Coin{sellerCoin, buyerFeeCoin}, WalletOwns: map[string]bool{
		outpointKey(takerCoin.Txid, 1): true, outpointKey(buyerFeeCoin.Txid, 0): true},
		Ours: [][]byte{sellerEnc, sellerRecv, feeChange}, Gives: map[[32]byte]uint64{tA: 100, tF: 300},
		Gets: []Out{{Asset: tB, Value: 20, Script: sellerRecv}}}
	u := build(t, []Coin{sellerCoin, takerCoin, buyerFeeCoin}, []Out{
		{tA, 100, buyerEnc}, {tB, 20, sellerRecv}, {tA, 300, sellerEnc}, {tB, 930, buyerChange}, {tF, 4700, feeChange}, {tF, 300, nil}})
	wantRefused(t, CheckTrade(u, side), "a coin of ours not offered")
}

// A3: an expired gated order's keyless refund (CLTV) pays the seller's
// enclave inside the trade, and would count as the seller's change.
func TestCheckTradeRefusesALockTime(t *testing.T) {
	refund := Coin{Txid: tid(7), Vout: 0, Asset: tA, Value: 300, Script: gatedRefund}
	u := build(t, []Coin{refund, sellerCoin, buyerCoin, buyerFeeCoin}, []Out{
		{tA, 300, sellerEnc}, {tA, 400, buyerEnc}, {tB, 10, sellerRecv}, {tB, 990, buyerChange}, {tF, 4700, feeChange}, {tF, 300, nil}})
	u.Tx.Locktime = 1794440523
	wantRefused(t, CheckTrade(u, sellerSide()), "lock time 1794440523")
}

// A CSV path: an input whose sequence can meet a relative lock.
func TestCheckTradeRefusesARelativeLock(t *testing.T) {
	u, _ := BuildDvP(plan())
	u.Tx.Inputs[1].Sequence = 144
	wantRefused(t, CheckTrade(u, sellerSide()), "relative lock")
}

// A4: the buyer's "delivery" is the refund of its own order, back home.
func TestCheckTradeRefusesTheirRefundAsOurDelivery(t *testing.T) {
	refund := Coin{Txid: tid(7), Vout: 0, Asset: tA, Value: 300, Script: gatedRefund}
	u := build(t, []Coin{refund, buyerCoin, buyerFeeCoin}, []Out{
		{tA, 300, buyerEnc}, {tB, 10, sellerRecv}, {tB, 990, buyerChange}, {tF, 4700, feeChange}, {tF, 300, nil}})
	u.Tx.Locktime = 1794440523
	wantRefused(t, CheckTrade(u, buyerSide()), "lock time")
}

// An output counts once: a seller paying the fee in the price's asset cannot
// have its price output also stand for its fee change.
func TestCheckTradeCountsAnOutputOnce(t *testing.T) {
	feeB := Coin{Txid: tid(5), Vout: 0, Asset: tB, Value: 50, Script: feeChange}
	side := TradeSide{Offered: []Coin{sellerCoin, feeB}, Ours: [][]byte{sellerEnc, sellerRecv, feeChange},
		Gives: map[[32]byte]uint64{tA: 100, tB: 30}, Gets: []Out{{Asset: tB, Value: 10, Script: sellerRecv}}}
	// The fee change (20) is missing: the price (10) is all that comes back.
	u := build(t, []Coin{sellerCoin, feeB, buyerCoin}, []Out{
		{tA, 100, buyerEnc}, {tB, 10, sellerRecv}, {tA, 300, sellerEnc}, {tB, 990, buyerChange}, {tB, 50, nil}})
	wantRefused(t, CheckTrade(u, side), "takes 50 of")
}

func TestCheckTradeFee(t *testing.T) {
	u, _ := BuildDvP(plan())
	s := sellerSide()
	s.FeeFloor = func([32]byte) (uint64, error) { return 301, nil }
	wantRefused(t, CheckTrade(u, s), "below our floor of 301")
	s.FeeFloor = func([32]byte) (uint64, error) { return 0, fmt.Errorf("the node accepts no fees in it") }
	wantRefused(t, CheckTrade(u, s), "accepts no fees")
	u2 := build(t, []Coin{sellerCoin, buyerCoin, buyerFeeCoin}, []Out{
		{tA, 100, buyerEnc}, {tB, 10, sellerRecv}, {tA, 300, sellerEnc}, {tB, 980, buyerChange}, {tF, 4700, feeChange}, {tF, 300, nil}, {tB, 10, nil}})
	wantRefused(t, CheckTrade(u2, sellerSide()), "in 2 assets")
}
