package watcher

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// rpcChain is a ChainView backed by an Elements/Sequentia node's JSON-RPC. It
// uses only node-level, wallet-independent calls (getbestblockhash,
// getblockcount, gettxout, getrawtransaction, getrawmempool, getblockhash,
// getblock), so it needs no wallet and holds no keys. It reads the LIVE tip on
// every Inspect (gettxout hits the current UTXO set), never a cache — the node's
// own Bitcoin-anchor reorg-following (first principle) is what makes a vanished
// funding UTXO observable here.
type rpcChain struct {
	url  string
	user string
	pass string
	http *http.Client
	// scanWindow bounds how far back from the tip Inspect walks to find a
	// confirmed spender (regtest fills confirm within a few blocks).
	scanWindow int64
}

// NewRPCChain builds a ChainView for host:port with cookie basic-auth.
func NewRPCChain(host string, port int, user, pass string) ChainView {
	return &rpcChain{
		url:        fmt.Sprintf("http://%s:%d", host, port),
		user:       user,
		pass:       pass,
		http:       &http.Client{Timeout: 30 * time.Second},
		scanWindow: 100,
	}
}

func (c *rpcChain) call(out interface{}, method string, params ...interface{}) error {
	if params == nil {
		params = []interface{}{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0", "id": "seqob-watcher", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: decode (http %d): %w", method, resp.StatusCode, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: rpc %d: %s", method, r.Error.Code, r.Error.Message)
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("%s: unmarshal: %w", method, err)
		}
	}
	return nil
}

func (c *rpcChain) Tip() (string, int64, error) {
	var hash string
	if err := c.call(&hash, "getbestblockhash"); err != nil {
		return "", 0, err
	}
	var height int64
	if err := c.call(&height, "getblockcount"); err != nil {
		return "", 0, err
	}
	return hash, height, nil
}

type txout struct {
	Confirmations int         `json:"confirmations"`
	Value         json.Number `json:"value"`
	Asset         string      `json:"asset"`
	ScriptPubKey  struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
}

type rawtx struct {
	Confirmations int    `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
	Vin           []struct {
		TxID        string   `json:"txid"`
		Vout        uint32   `json:"vout"`
		TxInWitness []string `json:"txinwitness"`
	} `json:"vin"`
	Vout []struct {
		N            uint32      `json:"n"`
		Value        json.Number `json:"value"`
		Asset        string      `json:"asset"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

// Inspect reads the on-chain reality of a covenant outpoint at the current tip.
func (c *rpcChain) Inspect(txid string, vout uint32, orderSPKHex, assetADisplay string) (CovState, error) {
	var st CovState

	// 1) Is the outpoint in the current UTXO set (mempool-aware)? gettxout
	// returns null once spent or if the outpoint does not exist.
	var utxo *txout
	if err := c.call(&utxo, "gettxout", txid, vout, true); err != nil {
		return st, fmt.Errorf("gettxout %s:%d: %w", txid, vout, err)
	}
	if utxo != nil {
		st.Unspent = true
		st.Value = coinsToAtoms(utxo.Value)
		st.AssetDisplay = utxo.Asset
		st.SPKHex = utxo.ScriptPubKey.Hex
		st.ConfirmedUTXO = utxo.Confirmations >= 1
		return st, nil
	}

	// 2) Spent or absent. Find the spender (mempool first, then a bounded window
	// of recent blocks). No spender AND not a UTXO => the outpoint does not exist
	// at the current tip (reorg / never confirmed): GHOST.
	spender, spenderTx, err := c.findSpender(txid, vout)
	if err != nil {
		return st, err
	}
	if spender == "" {
		return st, nil // st.SpenderTxid == "" => classifier decides GHOST
	}
	st.SpenderTxid = spender
	st.SpenderConfirmed = spenderTx.Confirmations >= 1

	// 3) A remainder covenant lands at index 2k+1 where k is the covenant input's
	// index in the spender. Find k, then match the candidate output against the
	// covenant program (spk == orderSPK, asset == asset_a).
	k, found := spentInputIndex(spenderTx, txid, vout)
	if found {
		ri := 2*k + 1
		for _, o := range spenderTx.Vout {
			if o.N == ri &&
				strings.EqualFold(o.ScriptPubKey.Hex, orderSPKHex) &&
				strings.EqualFold(o.Asset, assetADisplay) {
				st.Remainder = RemainderOut{Present: true, Vout: o.N, Value: coinsToAtoms(o.Value)}
				break
			}
		}
		// What the record of the fill is read from: the spender's outputs as the
		// order's leaf reads them, the witness of the input that spent the order,
		// and what the order's outpoint held.
		sp := &SpenderTx{Txid: spender, Input: k}
		for _, w := range spenderTx.Vin[k].TxInWitness {
			b, err := hex.DecodeString(w)
			if err != nil {
				return st, fmt.Errorf("spender %s input %d witness: %w", spender, k, err)
			}
			sp.Witness = append(sp.Witness, b)
		}
		for _, o := range spenderTx.Vout {
			sp.Outputs = append(sp.Outputs, outputOf(o.Asset, o.Value, o.ScriptPubKey.Hex))
		}
		var prev rawtx
		if err := c.call(&prev, "getrawtransaction", txid, true); err != nil {
			return st, fmt.Errorf("getrawtransaction %s: %w", txid, err)
		}
		for _, o := range prev.Vout {
			if o.N == vout {
				sp.PrevValue = coinsToAtoms(o.Value)
			}
		}
		st.Spender = sp
	}
	return st, nil
}

// outputOf reads a decoded output as a FILL leaf reads it: an asset and value
// the node shows are explicit, a missing one is a commitment.
func outputOf(assetDisplay string, value json.Number, spkHex string) covenant.Output {
	var out covenant.Output
	if b, err := hex.DecodeString(assetDisplay); err == nil && len(b) == 32 {
		for i := 0; i < 32; i++ {
			out.Asset[i] = b[31-i]
		}
		out.AssetExplicit = true
	}
	if strings.TrimSpace(value.String()) != "" {
		out.Value = coinsToAtoms(value)
		out.ValueExplicit = true
	}
	out.Script, _ = hex.DecodeString(spkHex)
	return out
}

// TxStatus reports where txid stands at the live tip: in a block of the active
// chain (with that block's height, anchor and certification), in the mempool,
// or neither. A node without -txindex cannot tell a confirmed transaction from
// an absent one; that is reported as an error, never as an absence.
func (c *rpcChain) TxStatus(txid string) (TxStatus, error) {
	var ts TxStatus
	var tx rawtx
	if err := c.call(&tx, "getrawtransaction", txid, true); err != nil {
		if strings.Contains(err.Error(), "txindex") {
			return ts, err
		}
		if strings.Contains(err.Error(), "No such mempool or blockchain transaction") {
			return ts, nil // neither in the chain nor in the mempool
		}
		return ts, err
	}
	if tx.BlockHash != "" {
		var hdr struct {
			Confirmations int64  `json:"confirmations"`
			Height        int64  `json:"height"`
			AnchorHeight  *int64 `json:"anchorheight"`
			PosCertified  *bool  `json:"poscertified"`
		}
		if err := c.call(&hdr, "getblockheader", tx.BlockHash, true); err != nil {
			return ts, err
		}
		if hdr.Confirmations > 0 {
			ts.InChain = true
			ts.Block, ts.Height, ts.Confirmations = tx.BlockHash, hdr.Height, hdr.Confirmations
			if hdr.AnchorHeight != nil {
				ts.BlockAnchor = *hdr.AnchorHeight
			}
			if hdr.PosCertified != nil {
				ts.CertPresent = true
				ts.Certified = *hdr.PosCertified
				if !ts.Certified {
					ts.Certified = c.certifiedAbove(hdr.Height, hdr.Confirmations)
				}
			}
			return ts, nil
		}
		// Its block left the active chain: it is in the mempool again, or gone.
	}
	var entry json.RawMessage
	if err := c.call(&entry, "getmempoolentry", txid); err == nil {
		ts.InMempool = true
	}
	return ts, nil
}

// certifiedAbove reports whether a block above height (on the active chain, up
// to the tip and at most certLookback blocks) is quorum-certified: a certified
// block cannot be left without leaving its ancestors.
func (c *rpcChain) certifiedAbove(height, confirmations int64) bool {
	top := height + confirmations - 1
	if top > height+certLookback {
		top = height + certLookback
	}
	for h := height + 1; h <= top; h++ {
		var bh string
		if err := c.call(&bh, "getblockhash", h); err != nil {
			return false
		}
		var hdr struct {
			PosCertified *bool `json:"poscertified"`
		}
		if err := c.call(&hdr, "getblockheader", bh, true); err != nil {
			return false
		}
		if hdr.PosCertified != nil && *hdr.PosCertified {
			return true
		}
	}
	return false
}

// certLookback bounds how far above a block TxStatus looks for a certificate.
const certLookback = 144

// Anchor reads the tip's Bitcoin anchor (getanchorstatus). A chain without
// Bitcoin anchoring answers Anchored false.
func (c *rpcChain) Anchor() (AnchorTip, error) {
	var a struct {
		AnchorHeight int64  `json:"anchorheight"`
		AnchorStatus string `json:"anchorstatus"`
	}
	if err := c.call(&a, "getanchorstatus"); err != nil {
		if strings.Contains(err.Error(), "not enabled") {
			return AnchorTip{}, nil
		}
		return AnchorTip{}, err
	}
	return AnchorTip{Anchored: true, Height: a.AnchorHeight, Status: a.AnchorStatus}, nil
}

// findSpender returns the txid + decoded tx that spends (txid,vout), searching
// the mempool then recent blocks. Returns ("", nil, nil) if none is found.
func (c *rpcChain) findSpender(txid string, vout uint32) (string, *rawtx, error) {
	spends := func(cand string) (*rawtx, bool) {
		var tx rawtx
		if err := c.call(&tx, "getrawtransaction", cand, true); err != nil {
			return nil, false // ignore txs the node cannot decode
		}
		for _, in := range tx.Vin {
			if strings.EqualFold(in.TxID, txid) && in.Vout == vout {
				return &tx, true
			}
		}
		return nil, false
	}

	// Mempool.
	var mempool []string
	if err := c.call(&mempool, "getrawmempool"); err == nil {
		for _, mt := range mempool {
			if tx, ok := spends(mt); ok {
				return mt, tx, nil
			}
		}
	}

	// Recent blocks, tip-first.
	var height int64
	if err := c.call(&height, "getblockcount"); err != nil {
		return "", nil, err
	}
	for h := height; h >= 0 && h > height-c.scanWindow; h-- {
		var bh string
		if err := c.call(&bh, "getblockhash", h); err != nil {
			break
		}
		var blk struct {
			Tx []string `json:"tx"`
		}
		if err := c.call(&blk, "getblock", bh); err != nil {
			continue
		}
		for _, bt := range blk.Tx {
			if tx, ok := spends(bt); ok {
				return bt, tx, nil
			}
		}
	}
	return "", nil, nil
}

// spentInputIndex returns the index k of the spender input that spends
// (txid,vout).
func spentInputIndex(tx *rawtx, txid string, vout uint32) (uint32, bool) {
	for i, in := range tx.Vin {
		if strings.EqualFold(in.TxID, txid) && in.Vout == vout {
			return uint32(i), true
		}
	}
	return 0, false
}

// coinsToAtoms converts the node's decimal-coins value to integer atoms (1e8 per
// coin) EXACTLY, from the decimal text. Going through float64 is exact only below
// 2^53 atoms (about 9e7 units at 8 decimals); a large issuance read that way
// mis-sizes the live covenant by whole atoms, and the watcher then "reconciles" the
// order to a wrong size. A malformed or negative value reads as 0 (the classifier
// treats that as an unfundable covenant, the safe direction).
func coinsToAtoms(v json.Number) uint64 {
	s := strings.TrimSpace(v.String())
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "eE") {
		return 0
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseUint(whole, 10, 64)
	if err != nil || w > math.MaxUint64/100_000_000 {
		return 0
	}
	f, err := strconv.ParseUint(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*1e8 + f
}

// GenesisHash is the genesis block hash (display hex) of the chain a view
// built by NewRPCChain reads.
func GenesisHash(v ChainView) (string, error) {
	c, ok := v.(*rpcChain)
	if !ok {
		return "", fmt.Errorf("not a node RPC view")
	}
	var h string
	if err := c.call(&h, "getblockhash", 0); err != nil {
		return "", err
	}
	return h, nil
}
