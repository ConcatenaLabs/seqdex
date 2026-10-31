package covfill

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// The fee asset of a fill the taker's wallet funds, when the taker names none.
//
// Sequentia's fee market is open: a transaction pays its fee in exactly one
// asset, which each node values at broadcast through its own exchange-rate
// table (getfeeexchangerates), and no asset is the default or a fallback. The
// rule is the one every wallet applies (SeqPal's seqpald and openampd do too):
//
//  1. the asset the transaction moves (for a fill, the asset the taker pays
//     in), when the node accepts it for fees;
//  2. otherwise, among the assets the node accepts, the one whose largest
//     spendable coin in the taker's wallet covers the most fees: the coin's
//     atoms valued at the node's rate. No asset is preferred for what it is,
//     the policy asset included; a tie goes to the lower asset id.
//
// An asset a taken order sells is never the fee asset: the taker never funds
// it (it comes from the order's own input), and the tapscript leaf would read
// a fee output in it at the gap slot. A node that accepts none of the wallet's
// assets for fees gets a refusal, never a guess.

// FeeCoin is one spendable coin of the taker's wallet.
type FeeCoin struct {
	Asset string // display hex
	Atoms uint64
}

// PickFeeAsset applies the rule to a node's rates (display asset id -> rate,
// 1e8 being one reference unit per whole coin) and a wallet's coins. moved is
// the asset the fill pays in; exclude holds the assets taken orders sell.
func PickFeeAsset(moved string, rates map[string]uint64, coins []FeeCoin, exclude map[string]bool) (string, error) {
	moved = strings.ToLower(moved)
	accepted := func(a string) bool { return rates[a] > 0 && !exclude[a] }
	if moved != "" && accepted(moved) {
		return moved, nil
	}
	largest := map[string]uint64{}
	for _, c := range coins {
		a := strings.ToLower(c.Asset)
		if accepted(a) && c.Atoms > largest[a] {
			largest[a] = c.Atoms
		}
	}
	if len(largest) == 0 {
		var acc []string
		for a, r := range rates {
			if r > 0 && !exclude[a] {
				acc = append(acc, a)
			}
		}
		sort.Strings(acc)
		list := "none"
		if len(acc) > 0 {
			list = strings.Join(acc, ", ")
		}
		return "", fmt.Errorf("no fee asset: the asset the fill pays in (%s) is not one the node accepts for fees, and the "+
			"wallet holds no coin of an asset it does accept (accepted here: %s); name one with -fee-asset", moved, list)
	}
	assets := make([]string, 0, len(largest))
	for a := range largest {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	best, bestWorth := "", new(big.Int)
	for _, a := range assets {
		worth := new(big.Int).Mul(new(big.Int).SetUint64(largest[a]), new(big.Int).SetUint64(rates[a]))
		if best == "" || worth.Cmp(bestWorth) > 0 {
			best, bestWorth = a, worth
		}
	}
	return best, nil
}

// FeeAssetFor asks the node for its fee rates and the taker wallet for its
// spendable coins, and applies PickFeeAsset.
func FeeAssetFor(node Node, moved string, exclude map[string]bool) (string, error) {
	var rawRates map[string]json.Number
	if err := node.Call(&rawRates, "getfeeexchangerates"); err != nil {
		return "", fmt.Errorf("getfeeexchangerates: %w", err)
	}
	// The node names an asset it has a label for by that label ("gold"), every other one by
	// its id; the wallet's coins name assets by id. Labels are read back to ids, or no labelled
	// asset would ever be found accepted.
	var labels map[string]string
	_ = node.Call(&labels, "dumpassetlabels")
	ids := map[string]string{}
	for l, id := range labels {
		ids[strings.ToLower(l)] = strings.ToLower(id)
	}
	rates := map[string]uint64{}
	for a, r := range rawRates {
		n, ok := new(big.Int).SetString(r.String(), 10)
		if ok && n.Sign() > 0 && n.IsUint64() {
			k := strings.ToLower(a)
			if id, ok := ids[k]; ok {
				k = id
			}
			rates[k] = n.Uint64()
		}
	}
	var utxos []struct {
		Asset     string      `json:"asset"`
		Amount    json.Number `json:"amount"`
		Spendable bool        `json:"spendable"`
	}
	if err := node.Call(&utxos, "listunspent", 1); err != nil {
		return "", fmt.Errorf("listunspent: %w", err)
	}
	coins := make([]FeeCoin, 0, len(utxos))
	for _, u := range utxos {
		if u.Spendable {
			coins = append(coins, FeeCoin{Asset: u.Asset, Atoms: atomsOf(u.Amount)})
		}
	}
	return PickFeeAsset(moved, rates, coins, exclude)
}

// atomsOf converts the node's decimal coins to atoms exactly.
func atomsOf(v json.Number) uint64 {
	r, ok := new(big.Rat).SetString(v.String())
	if !ok || r.Sign() <= 0 {
		return 0
	}
	r.Mul(r, big.NewRat(100_000_000, 1))
	if !r.IsInt() || !r.Num().IsUint64() {
		return 0
	}
	return r.Num().Uint64()
}
