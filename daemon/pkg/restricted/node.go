package restricted

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// Node is the Sequentia node and the wallet a party pays fees and pots from.
// The wallet never holds a restricted coin: those sit at enclaves, found by
// scanning the chain, and are signed by the holder's enclave key.
type Node struct {
	Chain *xchain.Chain
}

func (n *Node) rpc() *xchain.RPC { return n.Chain.RPC() }

// Genesis is the chain's genesis hash in internal byte order (the sighash's).
func (n *Node) Genesis() ([32]byte, error) {
	var h string
	if err := n.rpc().Call(&h, "getblockhash", 0); err != nil {
		return [32]byte{}, err
	}
	return ReverseHex(h)
}

// MedianTime is the tip's median time past.
func (n *Node) MedianTime() (int64, error) {
	var info struct {
		MedianTime int64 `json:"mediantime"`
	}
	if err := n.rpc().Call(&info, "getblockchaininfo"); err != nil {
		return 0, err
	}
	return info.MedianTime, nil
}

// Now is the later of this machine's clock and the chain's median time past:
// the time every deadline here is judged by, as the policy server judges it.
func (n *Node) Now() time.Time {
	t := time.Now()
	if m, err := n.MedianTime(); err == nil && m > t.Unix() {
		t = time.Unix(m, 0)
	}
	return t
}

func atoms(v json.Number) (uint64, error) {
	f, err := v.Float64()
	if err != nil {
		return 0, err
	}
	return uint64(math.Round(f * 1e8)), nil
}

// EnclaveCoins lists the unspent explicit coins of an asset (display hex) at an
// enclave script, by scanning the UTXO set.
func (n *Node) EnclaveCoins(spk []byte, asset string) ([]Coin, error) {
	var res struct {
		Unspents []struct {
			Txid   string      `json:"txid"`
			Vout   uint32      `json:"vout"`
			Asset  string      `json:"asset"`
			Amount json.Number `json:"amount"`
		} `json:"unspents"`
	}
	var err error
	for try := 0; try < 20; try++ {
		if err = n.rpc().Call(&res, "scantxoutset", "start", []string{"raw(" + hex.EncodeToString(spk) + ")"}); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "in progress") {
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	a, err := ReverseHex(asset)
	if err != nil {
		return nil, err
	}
	var out []Coin
	for _, u := range res.Unspents {
		if u.Asset != asset {
			continue // a blinded coin, or another asset
		}
		v, err := atoms(u.Amount)
		if err != nil {
			return nil, err
		}
		out = append(out, Coin{Txid: u.Txid, Vout: u.Vout, Asset: a, Value: v, Script: spk})
	}
	return out, nil
}

// WalletCoins selects confirmed explicit wallet coins of an asset worth at
// least need, largest first, and locks them so no other spend takes them.
func (n *Node) WalletCoins(asset string, need uint64) ([]Coin, error) {
	var us []struct {
		Txid         string      `json:"txid"`
		Vout         uint32      `json:"vout"`
		Asset        string      `json:"asset"`
		Amount       json.Number `json:"amount"`
		ScriptPubKey string      `json:"scriptPubKey"`
		Spendable    bool        `json:"spendable"`
		AmountBlind  string      `json:"amountblinder"`
	}
	if err := n.rpc().Call(&us, "listunspent", 1, 9999999, []string{}, false, map[string]interface{}{"asset": asset}); err != nil {
		return nil, err
	}
	a, err := ReverseHex(asset)
	if err != nil {
		return nil, err
	}
	var cands []Coin
	for _, u := range us {
		if u.Asset != asset || !u.Spendable || (u.AmountBlind != "" && strings.Trim(u.AmountBlind, "0") != "") {
			continue
		}
		v, err := atoms(u.Amount)
		if err != nil {
			return nil, err
		}
		spk, err := hex.DecodeString(u.ScriptPubKey)
		if err != nil {
			return nil, err
		}
		cands = append(cands, Coin{Txid: u.Txid, Vout: u.Vout, Asset: a, Value: v, Script: spk})
	}
	for i := range cands {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].Value > cands[i].Value {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	var got []Coin
	var sum uint64
	for _, c := range cands {
		if sum >= need {
			break
		}
		got = append(got, c)
		sum += c.Value
	}
	if sum < need {
		return nil, fmt.Errorf("the wallet holds %d of %s in explicit confirmed coins, short of %d", sum, asset, need)
	}
	return got, n.Lock(got)
}

// Lock keeps coins from the wallet's own spends; Unlock gives them back.
func (n *Node) Lock(cs []Coin) error   { return n.lockunspent(false, cs) }
func (n *Node) Unlock(cs []Coin) error { return n.lockunspent(true, cs) }

func (n *Node) lockunspent(unlock bool, cs []Coin) error {
	if len(cs) == 0 {
		return nil
	}
	var ops []map[string]interface{}
	for _, c := range cs {
		ops = append(ops, map[string]interface{}{"txid": c.Txid, "vout": c.Vout})
	}
	var ok bool
	if unlock {
		return n.rpc().Call(&ok, "lockunspent", true, ops)
	}
	// Persistent, so a restart keeps a refund's reserved fee coin set aside.
	return n.rpc().Call(&ok, "lockunspent", false, ops, true)
}

// NewV1Script is a fresh v1 (taproot) script of the wallet: where a pot's
// refund pays home. Never a v0 key's script, which may be a staking key's.
func (n *Node) NewV1Script() ([]byte, error) {
	var addr string
	if err := n.rpc().Call(&addr, "getnewaddress", "", "bech32m"); err != nil {
		return nil, err
	}
	spk, err := n.Chain.AddressScriptPubKey(addr)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(spk)
	if err != nil {
		return nil, err
	}
	if _, err := ProgramOf(b); err != nil {
		return nil, err
	}
	return b, nil
}

// NewScript is a fresh wallet script for change.
func (n *Node) NewScript() ([]byte, error) { return n.Chain.NewDestScript() }

// SignWallet signs the transaction's wallet inputs (SIGHASH_ALL), leaving the
// others as they are, and returns the transaction hex.
func (n *Node) SignWallet(u *Unsigned) error {
	raw, err := u.Hex()
	if err != nil {
		return err
	}
	var res struct {
		Hex string `json:"hex"`
	}
	if err := n.rpc().Call(&res, "signrawtransactionwithwallet", raw); err != nil {
		return err
	}
	signed, err := txFromHex(res.Hex)
	if err != nil {
		return err
	}
	for i, in := range signed.Inputs {
		if len(u.Tx.Inputs[i].Witness) == 0 && len(in.Witness) > 0 {
			u.Tx.Inputs[i].Witness = in.Witness
		}
	}
	return nil
}

// Broadcast sends a transaction.
func (n *Node) Broadcast(u *Unsigned) (string, error) {
	raw, err := u.Hex()
	if err != nil {
		return "", err
	}
	return n.Chain.Broadcast(raw)
}

// CoinAt reads an unspent explicit output.
func (n *Node) CoinAt(txid string, vout uint32) (*Coin, error) {
	var o *struct {
		Value        json.Number `json:"value"`
		Asset        string      `json:"asset"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := n.rpc().Call(&o, "gettxout", txid, vout, true); err != nil {
		return nil, err
	}
	if o == nil {
		return nil, nil
	}
	if o.Asset == "" {
		return nil, fmt.Errorf("%s:%d is not explicit", txid, vout)
	}
	a, err := ReverseHex(o.Asset)
	if err != nil {
		return nil, err
	}
	v, err := atoms(o.Value)
	if err != nil {
		return nil, err
	}
	spk, err := hex.DecodeString(o.ScriptPubKey.Hex)
	if err != nil {
		return nil, err
	}
	return &Coin{Txid: txid, Vout: vout, Asset: a, Value: v, Script: spk}, nil
}

// Confirmations is how many blocks bury txid; 0 while it is unmined.
func (n *Node) Confirmations(txid string) (int64, error) {
	var tx struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := n.rpc().Call(&tx, "getrawtransaction", txid, true); err != nil {
		return 0, err
	}
	return tx.Confirmations, nil
}

// SpendWitness finds the transaction that spent an outpoint and returns its
// id and that input's witness; "" while the outpoint is unspent.
func (n *Node) SpendWitness(txid string, vout uint32) (string, [][]byte, error) {
	spender, err := n.Chain.SpenderOf(txid, vout)
	if err != nil || spender == "" {
		return "", nil, err
	}
	var raw struct {
		Vin []struct {
			Txid    string   `json:"txid"`
			Vout    uint32   `json:"vout"`
			Witness []string `json:"txinwitness"`
		} `json:"vin"`
	}
	if err := n.rpc().Call(&raw, "getrawtransaction", spender, true); err != nil {
		return spender, nil, err
	}
	for _, in := range raw.Vin {
		if in.Txid == txid && in.Vout == vout {
			var w [][]byte
			for _, h := range in.Witness {
				b, err := hex.DecodeString(h)
				if err != nil {
					return spender, nil, err
				}
				w = append(w, b)
			}
			return spender, w, nil
		}
	}
	return spender, nil, fmt.Errorf("%s does not spend %s:%d", spender, txid, vout)
}

// FeeAtoms sizes a fee of vsize virtual bytes at rfaPerVb reference atoms per
// byte in an asset's own atoms, through the node's published exchange rate
// (atoms = ceil(rfa * 1e8 / rate)). An asset the node publishes no rate for
// pays nothing here: it is refused, never valued 1:1.
func (n *Node) FeeAtoms(asset string, vsize, rfaPerVb uint64) (uint64, error) {
	rate, listed, err := n.Chain.FeeExchangeRateErr(asset)
	if err != nil {
		return 0, err
	}
	if !listed || rate == 0 {
		return 0, fmt.Errorf("the node accepts no fees in %s", asset)
	}
	ref := vsize * rfaPerVb
	fee := (ref*100_000_000 + rate - 1) / rate
	if fee == 0 {
		fee = 1
	}
	return fee, nil
}
