// hedge.go — a crossing's second leg, checked before the first, and completed by
// other offers when it fails anyway.
//
// A crossing is two takes on two rails, and only each take is atomic: a second
// leg that fails after the first settled leaves the crosser holding what the
// first leg bought (INVENTORY DRIFT). Two things keep that rare and small:
//
//   - Preflight: before leg 1 runs, leg 2 is checked as far as the crosser can
//     check it without the maker. A covenant leg's coin must still be unspent at
//     the node (a coin someone else just took or the maker reclaimed fails its
//     fill), and a pure-Lightning leg needs the taker node to hold a usable
//     channel balance in the asset it pays, at least what it pays (a route that
//     cannot carry the payment fails at getroute). A leg that fails its preflight
//     cancels the crossing with nothing executed.
//   - Completion: when leg 2 fails after leg 1 settled, the crosser reads the
//     book again and takes the best other offers on leg 2's side, on any rail it
//     can execute, until the position is flat, as long as each is priced within
//     -hedge-max-bps of the failed leg's price (a worse price would turn a small
//     arbitrage into a large loss; what is left then is booked as drift, as
//     before, and reported).
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Preflight is the check of leg `n` for a take of `take` base atoms; "" when it
// can run, else why not.
func (e *Executor) Preflight(n *NormOrder, take uint64) string {
	// A leg that spends from the crosser's node wallet needs that wallet loaded: a node
	// restart drops a wallet that is not loaded on startup, and the leg then fails with
	// "Requested wallet does not exist or is not loaded" after the other leg settled.
	switch n.Family {
	case FamCovenant, FamCross, FamSubmarine, FamSubAsset:
		if e.Caps.SeqRPC != "" && e.Caps.SeqWallet != "" {
			if loaded, err := e.walletLoaded(e.Caps.SeqWallet); err == nil && !loaded {
				return fmt.Sprintf("the node wallet %s is not loaded", e.Caps.SeqWallet)
			}
		}
	}
	switch n.Family {
	case FamCovenant:
		cov := n.Offer.GetCovenant()
		if cov == nil || e.Caps.SeqRPC == "" {
			return ""
		}
		unspent, err := e.txOutUnspent(cov.GetCovenantTxid(), cov.GetCovenantVout())
		if err != nil {
			return "" // the node cannot say: the leg runs and reports for itself
		}
		if !unspent {
			return fmt.Sprintf("its covenant coin %s:%d is spent", short(cov.GetCovenantTxid()), cov.GetCovenantVout())
		}
	case FamPureLN:
		// The crosser pays the quote to buy from an ask and the base to sell into a
		// bid; BTC rides the bitcoin node, an asset the asset node.
		asset, amount := n.Quote, n.CostFor(take)
		if n.Side == SideBid {
			asset, amount = n.Base, take
		}
		sock, assetID := e.Caps.AssetLNSocket, asset
		if asset == "BTC" {
			sock, assetID = e.Caps.BtcLNSocket, ""
		}
		room, err := e.spendable(sock, assetID)
		if err != nil {
			return ""
		}
		if room < amount {
			return fmt.Sprintf("the taker node can send %d of %s over its channels, the leg pays %d", room, short(asset), amount)
		}
	}
	return ""
}

// walletLoaded asks the node whether `wallet` is loaded.
func (e *Executor) walletLoaded(wallet string) (bool, error) {
	res, err := e.nodeCall("listwallets", "[]")
	if err != nil {
		return false, err
	}
	var names []string
	if err := json.Unmarshal(res, &names); err != nil {
		return false, err
	}
	for _, w := range names {
		if w == wallet {
			return true, nil
		}
	}
	return false, nil
}

// nodeCall is one JSON-RPC call to the crosser's node (no wallet), its result raw.
func (e *Executor) nodeCall(method, params string) (json.RawMessage, error) {
	body := fmt.Sprintf(`{"jsonrpc":"1.0","id":"pre","method":%q,"params":%s}`, method, params)
	req, err := http.NewRequest("POST", e.Caps.SeqRPC, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage        `json:"result"`
		Error  map[string]interface{} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %v", method, r.Error["message"])
	}
	return r.Result, nil
}

// txOutUnspent asks the node whether txid:vout is unspent, mempool included.
func (e *Executor) txOutUnspent(txid string, vout uint32) (bool, error) {
	res, err := e.nodeCall("gettxout", fmt.Sprintf(`[%q,%d,true]`, txid, vout))
	if err != nil {
		return false, err
	}
	return string(res) != "null" && len(res) > 0, nil
}

// spendable sums what the node at `sock` can send now over its usable channels
// in `asset` ("" for bitcoin), in atoms.
func (e *Executor) spendable(sock, asset string) (uint64, error) {
	if sock == "" {
		return 0, fmt.Errorf("no socket")
	}
	cli := e.Caps.LNCli
	if cli == "" {
		cli = "lightning-cli"
	}
	out, err := exec.Command(cli, "--rpc-file="+sock, "listpeerchannels").Output()
	if err != nil {
		return 0, err
	}
	return spendableIn(out, asset)
}

// spendableIn is spendable's parse of listpeerchannels, apart for its tests.
func spendableIn(listpeerchannels []byte, asset string) (uint64, error) {
	var r struct {
		Channels []struct {
			State     string      `json:"state"`
			Asset     string      `json:"channel_asset"`
			Spendable interface{} `json:"spendable_msat"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(listpeerchannels, &r); err != nil {
		return 0, err
	}
	var sum uint64
	for _, c := range r.Channels {
		if c.State != "CHANNELD_NORMAL" || !strings.EqualFold(c.Asset, asset) {
			continue
		}
		var msat uint64
		switch v := c.Spendable.(type) {
		case float64:
			msat = uint64(v)
		case string:
			fmt.Sscan(strings.TrimSuffix(v, "msat"), &msat)
		}
		sum += msat / 1000
	}
	return sum, nil
}

// priceWithin says whether `n`'s price is no worse for the crosser than
// `failed`'s by more than maxBps: a buy pays at most that much more, a sell
// gets at most that much less.
func priceWithin(n, failed *NormOrder, maxBps uint64) bool {
	// n.price vs failed.price × (1 ± maxBps), cross-multiplied in big.Int
	lhs := new(big.Int).Mul(new(big.Int).SetUint64(n.QuoteNum), new(big.Int).SetUint64(failed.BaseDen))
	lhs.Mul(lhs, big.NewInt(10000))
	rhs := new(big.Int).Mul(new(big.Int).SetUint64(failed.QuoteNum), new(big.Int).SetUint64(n.BaseDen))
	if failed.Side == SideAsk {
		rhs.Mul(rhs, new(big.Int).SetUint64(10000+maxBps))
		return lhs.Cmp(rhs) <= 0
	}
	if maxBps >= 10000 {
		return true
	}
	rhs.Mul(rhs, new(big.Int).SetUint64(10000-maxBps))
	return lhs.Cmp(rhs) >= 0
}

// CompleteLeg takes, from `orders` (the pair's book read again), the best other
// offers on the failed leg's side until `take` base atoms are done, each priced
// within maxBps of the failed leg and passing its preflight. It returns what it
// filled and a line a leg.
func (e *Executor) CompleteLeg(orders []*NormOrder, failed *NormOrder, take uint64, maxBps uint64, canExec Executability) (uint64, []string) {
	return completeLegWith(orders, failed, take, maxBps, canExec, e.Preflight, e.RunLeg)
}

// completeLegWith is CompleteLeg with its preflight and its leg runner given, for its tests.
func completeLegWith(orders []*NormOrder, failed *NormOrder, take uint64, maxBps uint64, canExec Executability,
	preflight func(*NormOrder, uint64) string, runLeg func(*NormOrder, uint64) LegResult) (uint64, []string) {
	var side []*NormOrder
	for _, n := range orders {
		if n.Side != failed.Side || n.ID() == failed.ID() {
			continue
		}
		if ok, _ := canExec(n); !ok || !priceWithin(n, failed, maxBps) {
			continue
		}
		side = append(side, n)
	}
	SortBook(side)
	var done uint64
	var lines []string
	for _, n := range side {
		if done >= take {
			break
		}
		left := take - done
		t, why := SizeOne(n, left)
		if t == 0 || t > left {
			// a take larger than what is left would overshoot into a new position
			if t > left && n.Sliceable && n.AllowPartial && left >= n.MinFill {
				t = left
			} else {
				if why == "" {
					why = fmt.Sprintf("its legal size %d is over the %d left", t, left)
				}
				lines = append(lines, fmt.Sprintf("skipped %s %s: %s", n.Family, short(n.ID()), why))
				continue
			}
		}
		if p := preflight(n, t); p != "" {
			lines = append(lines, fmt.Sprintf("skipped %s %s: %s", n.Family, short(n.ID()), p))
			continue
		}
		r := runLeg(n, t)
		if !r.Settled {
			lines = append(lines, fmt.Sprintf("failed %s %s for %d: %s", n.Family, short(n.ID()), t, r.Err))
			continue
		}
		done += t
		lines = append(lines, fmt.Sprintf("completed %d with %s %s", t, n.Family, short(n.ID())))
	}
	return done, lines
}
