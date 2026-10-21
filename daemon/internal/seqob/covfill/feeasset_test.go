package covfill

import (
	"strings"
	"testing"
)

const (
	pol = "aa00000000000000000000000000000000000000000000000000000000000000" // the policy asset, no different from any
	usd = "bb00000000000000000000000000000000000000000000000000000000000000"
	eur = "cc00000000000000000000000000000000000000000000000000000000000000"
	gld = "dd00000000000000000000000000000000000000000000000000000000000000" // what the order sells
)

func TestPickFeeAssetFollowsTheWalletRule(t *testing.T) {
	coins := []FeeCoin{{pol, 100_000}, {usd, 5_000_000}, {usd, 9_000_000}, {eur, 20_000_000}, {gld, 1 << 40}}
	all := map[string]uint64{pol: 100_000_000, usd: 100_000_000, eur: 50_000_000, gld: 1 << 30}
	sold := map[string]bool{gld: true}
	cases := []struct {
		name  string
		moved string
		rates map[string]uint64
		want  string
	}{
		{"the asset it pays in, accepted", usd, all, usd},
		{"pays in an asset the node does not take: the largest coin worth most", "ee" + usd[2:], all, eur}, // 20e6*0.5e8 > 9e6*1e8
		{"the policy asset only wins on worth", "ee" + usd[2:], map[string]uint64{pol: 100_000_000_000, usd: 1}, pol},
		{"never the asset an order sells, however much it is worth", gld, all, eur},
		{"a tie goes to the lower id", "ee" + usd[2:], map[string]uint64{usd: 20, eur: 9}, usd}, // 9e6*20 == 20e6*9
	}
	for _, c := range cases {
		got, err := PickFeeAsset(c.moved, c.rates, coins, sold)
		if err != nil || got != c.want {
			t.Errorf("%s: got %s %v, want %s", c.name, got, err, c.want)
		}
	}
	if _, err := PickFeeAsset(usd, map[string]uint64{}, coins, sold); err == nil || !strings.Contains(err.Error(), "no fee asset") ||
		!strings.Contains(err.Error(), "-fee-asset") {
		t.Fatalf("a node accepting nothing: %v", err)
	}
	if _, err := PickFeeAsset(usd, map[string]uint64{gld: 1}, coins, sold); err == nil {
		t.Fatal("the only accepted asset is the one an order sells: it must be refused")
	}
}

// FeeAssetFor reads the node's rates and the wallet's coins, decimal amounts exactly.
func TestFeeAssetForAsksTheNode(t *testing.T) {
	n := &scriptedNode{unspent: []utxoRow{
		{Txid: "t1", Amount: 0.2, Asset: eur, Spendable: true},
		{Txid: "t2", Amount: 0.09, Asset: usd, Spendable: true},
		{Txid: "t3", Amount: 99, Asset: pol, Spendable: false},
	}, rates: map[string]interface{}{usd: 100000000, eur: 50000000, pol: 100000000}}
	got, err := FeeAssetFor(n, "ff"+usd[2:], nil)
	if err != nil || got != eur {
		t.Fatalf("%s %v", got, err)
	}
	if got, _ := FeeAssetFor(n, usd, nil); got != usd {
		t.Fatalf("the asset paid in: %s", got)
	}
}
