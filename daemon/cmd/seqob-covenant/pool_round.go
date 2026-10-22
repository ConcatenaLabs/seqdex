package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// The pool (pkg/covenant SimPool, template seqdex/pool) and the round order
// (RoundOrder, template seqdex/round-order). Each spec command reads one JSON
// object on stdin; asset ids, keys and scripts are internal-order hex, txids
// display hex, the genesis hash internal order.
//
//	poolderive  < {"pool": POOL}
//	roundderive < {"order": ORDER}
//	    -> {template_hash, cmr, terms, scriptpubkey, control_block, address}
//
//	round < {"genesis", "base", "quote", "price": [N, D] (else ClearPrice),
//	         "orders": [{"terms": ORDER, "coin": COIN}], "pools": [{"terms": POOL,
//	         "base_coin": COIN, "quote_coin": COIN}], "funding": [COIN],
//	         "change_script", "fee_asset", "fee", "clearer_priv"}
//	    Chooses the price (unless given) and the fills (Allocate), lays the round
//	    out (BuildRound) and signs it as the clearer. Prints {hex, txid, price,
//	    digest, fills, moves, short_base, short_quote, coins}: the round
//	    orders' and pools' inputs carry their witnesses, the funding inputs are
//	    left for their wallet to sign.
//
//	roundsign < {"tx", "coins": [COIN per input], "genesis", "price": [N, D],
//	             "base", "quote", "orders": [{"k", "terms": ORDER, "partial"}],
//	             "clearer_priv"}
//	    Signs tx as the clearer and sets each named round order's fill witness.
//	    It judges nothing: it signs whatever transaction it is given, which is how
//	    a test builds a round the chain must refuse.
//
//	poolfill < {"pool": POOL, "base_coin", "quote_coin", "sell", "base", "quote"
//	            (else the quote's), "inputs": [COIN], "outputs": [OUT]}
//	    The pool at inputs 0 and 1 and again at outputs 0 and 1, then inputs and
//	    outputs as given; judged by SimPool.CheckFill unless "unchecked".
//
//	poolwithdraw < {"pool": POOL, "genesis", "coins": [COIN], "inputs": [COIN],
//	                "outputs": [OUT], "maker_priv"}
//	    The pool's coins at inputs 0.., each signed by the maker over its
//	    sig_all_hash.
//
//	roundrefund < {"order": ORDER, "genesis", "coin", "inputs": [COIN],
//	               "outputs": [OUT], "locktime", "refund_priv"}
//
// POOL = {base, quote, bid: [n, d], ask: [n, d], min_lot, reserve_base,
// reserve_quote, maker_key}; ORDER = {asset_a, asset_b, limit: [n, d], min_lot,
// maker_script, expiry, refund_key, clearer}; COIN = {txid, vout, asset, value,
// script}; OUT = {asset, value, script}.

type poolJSON struct {
	Base         string    `json:"base"`
	Quote        string    `json:"quote"`
	Bid          [2]uint64 `json:"bid"`
	Ask          [2]uint64 `json:"ask"`
	MinLot       uint64    `json:"min_lot"`
	ReserveBase  uint64    `json:"reserve_base"`
	ReserveQuote uint64    `json:"reserve_quote"`
	MakerKey     string    `json:"maker_key"`
}

func (p poolJSON) pool() covenant.SimPool {
	sp := covenant.SimPool{Base: hex32(p.Base, "base"), Quote: hex32(p.Quote, "quote"), BidNum: p.Bid[0], BidDen: p.Bid[1],
		AskNum: p.Ask[0], AskDen: p.Ask[1], MinLot: p.MinLot, ReserveBase: p.ReserveBase, ReserveQuote: p.ReserveQuote,
		MakerKey: hex32(p.MakerKey, "maker_key")}
	if err := sp.Validate(); err != nil {
		die("pool terms: %v", err)
	}
	return sp
}

type orderJSON struct {
	AssetA      string    `json:"asset_a"`
	AssetB      string    `json:"asset_b"`
	Limit       [2]uint64 `json:"limit"`
	MinLot      uint64    `json:"min_lot"`
	MakerScript string    `json:"maker_script"`
	Expiry      uint32    `json:"expiry"`
	RefundKey   string    `json:"refund_key"`
	Clearer     string    `json:"clearer"`
}

func (o orderJSON) order() covenant.RoundOrder {
	m, err := hex.DecodeString(o.MakerScript)
	if err != nil {
		die("maker_script: %v", err)
	}
	ro := covenant.RoundOrder{AssetA: hex32(o.AssetA, "asset_a"), AssetB: hex32(o.AssetB, "asset_b"),
		LimitNum: o.Limit[0], LimitDen: o.Limit[1], MinLot: o.MinLot, MakerScript: m, Expiry: o.Expiry,
		RefundKey: hex32(o.RefundKey, "refund_key"), Clearer: hex32(o.Clearer, "clearer")}
	if err := ro.Validate(); err != nil {
		die("order terms: %v", err)
	}
	return ro
}

type coinJSON struct {
	Txid   string `json:"txid"`
	Vout   uint32 `json:"vout"`
	Asset  string `json:"asset"`
	Value  uint64 `json:"value"`
	Script string `json:"script"`
}

func (c coinJSON) coin() covenant.RoundCoin {
	s, err := hex.DecodeString(c.Script)
	if err != nil {
		die("coin script: %v", err)
	}
	return covenant.RoundCoin{Txid: c.Txid, Vout: c.Vout, Asset: hex32(c.Asset, "coin asset"), Value: c.Value, Script: s}
}

type outJSON struct {
	Asset  string `json:"asset"`
	Value  uint64 `json:"value"`
	Script string `json:"script"`
}

func readSpec(v interface{}) {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		die("stdin: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		die("spec: %v", err)
	}
}

func key(h, name string) *btcec.PrivateKey {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		die("%s must be a 32-byte private key in hex", name)
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

func sign(k *btcec.PrivateKey, m [32]byte) [64]byte {
	s, err := schnorr.Sign(k, m[:])
	if err != nil {
		die("sign: %v", err)
	}
	var out [64]byte
	copy(out[:], s.Serialize())
	return out
}

func treeOut(t *covenant.SimTree, hash string, cmr [32]byte) {
	tb, _ := t.Address("tb")
	ert, _ := t.Address("ert")
	emit(map[string]interface{}{
		"template_hash": hash, "cmr": hex.EncodeToString(cmr[:]), "terms": hex.EncodeToString(t.Terms),
		"scriptpubkey": hex.EncodeToString(t.ScriptPubKey), "control_block": hex.EncodeToString(t.ControlBlock),
		"address": map[string]string{"sequentia-testnet": tb, "elementsregtest": ert},
	})
}

// buildTx is a transaction spending coins with the given outputs.
func buildTx(coins []covenant.RoundCoin, outs []outJSON, locktime uint32, seq uint32) (*transaction.Transaction, []covenant.SpentCoin) {
	tx := transaction.NewTx(2)
	tx.Locktime = locktime
	var spent []covenant.SpentCoin
	for _, c := range coins {
		h, err := elementsutil.TxIDToBytes(c.Txid)
		if err != nil || len(h) != 32 {
			die("txid %q", c.Txid)
		}
		in := transaction.NewTxInput(h, c.Vout)
		in.Sequence = seq
		tx.AddInput(in)
		spent = append(spent, covenant.ExplicitCoin(c.Asset, c.Value, c.Script))
	}
	for _, o := range outs {
		s, err := hex.DecodeString(o.Script)
		if err != nil {
			die("output script: %v", err)
		}
		v, err := elementsutil.ValueToBytes(o.Value)
		if err != nil {
			die("value: %v", err)
		}
		a := hex32(o.Asset, "output asset")
		tx.AddOutput(transaction.NewTxOutput(append([]byte{0x01}, a[:]...), v, s))
	}
	return tx, spent
}

func txOut(tx *transaction.Transaction, extra map[string]interface{}) {
	h, err := tx.ToHex()
	if err != nil {
		die("serialize: %v", err)
	}
	extra["hex"], extra["txid"] = h, tx.TxHash().String()
	emit(extra)
}

func ratOf(n, d uint64) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).SetUint64(n), new(big.Int).SetUint64(d))
}

// poolRound runs a pool or round-order subcommand; false when cmd is none.
func poolRound(cmd string) bool {
	switch cmd {
	case "poolderive":
		var s struct {
			Pool poolJSON `json:"pool"`
		}
		readSpec(&s)
		t, err := s.Pool.pool().Derive()
		if err != nil {
			die("derive: %v", err)
		}
		treeOut(t, covenant.PoolTemplateHash, covenant.PoolCMR)
	case "roundderive":
		var s struct {
			Order orderJSON `json:"order"`
		}
		readSpec(&s)
		t, err := s.Order.order().Derive()
		if err != nil {
			die("derive: %v", err)
		}
		treeOut(t, covenant.RoundOrderTemplateHash, covenant.RoundOrderCMR)
	case "round":
		roundCmd()
	case "roundsign":
		roundSignCmd()
	case "poolfill":
		var s struct {
			Pool      poolJSON   `json:"pool"`
			BaseCoin  coinJSON   `json:"base_coin"`
			QuoteCoin coinJSON   `json:"quote_coin"`
			Sell      bool       `json:"sell"`
			Base      uint64     `json:"base"`
			Quote     *uint64    `json:"quote"`
			Inputs    []coinJSON `json:"inputs"`
			Outputs   []outJSON  `json:"outputs"`
			Unchecked bool       `json:"unchecked"`
		}
		readSpec(&s)
		p := s.Pool.pool()
		xc, yc := s.BaseCoin.coin(), s.QuoteCoin.coin()
		dq := uint64(0)
		var err error
		switch {
		case s.Quote != nil:
			dq = *s.Quote
		case s.Sell:
			dq, err = p.AskQuote(s.Base)
		default:
			dq, err = p.BidQuote(s.Base)
		}
		if err != nil {
			die("quote: %v", err)
		}
		x1, y1 := xc.Value-s.Base, yc.Value+dq
		if !s.Sell {
			x1, y1 = xc.Value+s.Base, yc.Value-dq
		}
		spk, _ := p.Script()
		outs := append([]outJSON{{Asset: hex.EncodeToString(p.Base[:]), Value: x1, Script: hex.EncodeToString(spk)},
			{Asset: hex.EncodeToString(p.Quote[:]), Value: y1, Script: hex.EncodeToString(spk)}}, s.Outputs...)
		coins := []covenant.RoundCoin{xc, yc}
		for _, c := range s.Inputs {
			coins = append(coins, c.coin())
		}
		tx, _ := buildTx(coins, outs, 0, 0xffffffff)
		res := map[string]interface{}{"base": s.Base, "quote": dq}
		if !s.Unchecked {
			m, err := p.CheckFill(covenant.Explicit(p.Base, xc.Value, spk), covenant.Explicit(p.Quote, yc.Value, spk),
				covenant.OutputsOf(tx), 0, s.Sell)
			if err != nil {
				die("the pool's program would refuse this fill: %v", err)
			}
			res["move"] = m
		}
		w, err := p.FillWitness(s.Sell)
		if err != nil {
			die("witness: %v", err)
		}
		tx.Inputs[0].Witness = w
		if tx.Inputs[1].Witness, err = p.PairWitness(); err != nil {
			die("witness: %v", err)
		}
		txOut(tx, res)
	case "poolwithdraw":
		var s struct {
			Pool      poolJSON   `json:"pool"`
			Genesis   string     `json:"genesis"`
			Coins     []coinJSON `json:"coins"`
			Inputs    []coinJSON `json:"inputs"`
			Outputs   []outJSON  `json:"outputs"`
			MakerPriv string     `json:"maker_priv"`
		}
		readSpec(&s)
		p := s.Pool.pool()
		var coins []covenant.RoundCoin
		for _, c := range append(append([]coinJSON{}, s.Coins...), s.Inputs...) {
			coins = append(coins, c.coin())
		}
		tx, spent := buildTx(coins, s.Outputs, 0, 0xffffffff)
		t, _ := p.Derive()
		k := key(s.MakerPriv, "maker_priv")
		g := hex32(s.Genesis, "genesis")
		for i := range s.Coins {
			h, err := covenant.SimSigAllHash(g, tx, spent, uint32(i), covenant.PoolCMR, t.ControlBlock)
			if err != nil {
				die("sig_all_hash: %v", err)
			}
			w, err := p.WithdrawWitness(sign(k, h))
			if err != nil {
				die("witness: %v", err)
			}
			tx.Inputs[i].Witness = w
		}
		txOut(tx, map[string]interface{}{})
	case "roundrefund":
		var s struct {
			Order      orderJSON  `json:"order"`
			Genesis    string     `json:"genesis"`
			Coin       coinJSON   `json:"coin"`
			Inputs     []coinJSON `json:"inputs"`
			Outputs    []outJSON  `json:"outputs"`
			Locktime   uint32     `json:"locktime"`
			RefundPriv string     `json:"refund_priv"`
		}
		readSpec(&s)
		o := s.Order.order()
		coins := []covenant.RoundCoin{s.Coin.coin()}
		for _, c := range s.Inputs {
			coins = append(coins, c.coin())
		}
		tx, spent := buildTx(coins, s.Outputs, s.Locktime, 0xfffffffe)
		t, _ := o.Derive()
		h, err := covenant.SimSigAllHash(hex32(s.Genesis, "genesis"), tx, spent, 0, covenant.RoundOrderCMR, t.ControlBlock)
		if err != nil {
			die("sig_all_hash: %v", err)
		}
		w, err := o.RefundWitness(sign(key(s.RefundPriv, "refund_priv"), h))
		if err != nil {
			die("witness: %v", err)
		}
		tx.Inputs[0].Witness = w
		txOut(tx, map[string]interface{}{})
	default:
		return false
	}
	return true
}

type roundSpec struct {
	Genesis string     `json:"genesis"`
	Base    string     `json:"base"`
	Quote   string     `json:"quote"`
	Price   *[2]uint64 `json:"price"`
	Orders  []struct {
		Terms orderJSON `json:"terms"`
		Coin  coinJSON  `json:"coin"`
	} `json:"orders"`
	Pools []struct {
		Terms     poolJSON `json:"terms"`
		BaseCoin  coinJSON `json:"base_coin"`
		QuoteCoin coinJSON `json:"quote_coin"`
	} `json:"pools"`
	Funding      []coinJSON `json:"funding"`
	ChangeScript string     `json:"change_script"`
	FeeAsset     string     `json:"fee_asset"`
	Fee          uint64     `json:"fee"`
	ClearerPriv  string     `json:"clearer_priv"`
}

func roundCmd() {
	var s roundSpec
	readSpec(&s)
	base, quote := hex32(s.Base, "base"), hex32(s.Quote, "quote")
	var sides []covenant.Side
	orders := make([]covenant.RoundOrder, len(s.Orders))
	for k, o := range s.Orders {
		ro := o.Terms.order()
		orders[k] = ro
		c := o.Coin.coin()
		side := covenant.Side{ID: fmt.Sprintf("order:%d", k), Size: c.Value, MinLot: ro.MinLot, Seq: k}
		switch {
		case ro.AssetA == base && ro.AssetB == quote:
			side.Ask, side.Limit = true, ratOf(ro.LimitNum, ro.LimitDen)
		case ro.AssetA == quote && ro.AssetB == base:
			side.Limit = ratOf(ro.LimitDen, ro.LimitNum)
		default:
			die("order %d is not of the market", k)
		}
		sides = append(sides, side)
	}
	pools := make([]covenant.SimPool, len(s.Pools))
	for i, p := range s.Pools {
		sp := p.Terms.pool()
		pools[i] = sp
		if sp.Base != base || sp.Quote != quote {
			die("pool %d is not of the market in its orientation", i)
		}
		ab, bq := sp.Depth(p.BaseCoin.Value, p.QuoteCoin.Value)
		seq := 1_000_000 + i
		if ab > 0 {
			sides = append(sides, covenant.Side{ID: fmt.Sprintf("pool:%d:ask", i), Ask: true, Size: ab, Limit: ratOf(sp.AskNum, sp.AskDen), MinLot: sp.MinLot, Pool: true, Seq: seq})
		}
		if bq > 0 {
			sides = append(sides, covenant.Side{ID: fmt.Sprintf("pool:%d:bid", i), Size: bq, Limit: ratOf(sp.BidNum, sp.BidDen), MinLot: sp.MinLot, Pool: true, Seq: seq})
		}
	}
	var price covenant.Price
	if s.Price != nil {
		price = covenant.Price{Base: base, Quote: quote, N: s.Price[0], D: s.Price[1]}
	} else {
		p, ok, err := covenant.ClearPrice(sides, base, quote)
		if err != nil {
			die("price: %v", err)
		}
		if !ok {
			die("nothing crosses")
		}
		price = p
	}
	fills, sb, sq, err := covenant.Allocate(sides, price)
	if err != nil {
		die("allocate: %v", err)
	}
	r := covenant.Round{Price: price, ChangeScript: mustHex(s.ChangeScript, "change_script"), FeeAsset: hex32(s.FeeAsset, "fee_asset"), Fee: s.Fee}
	poolFill := map[int]*covenant.RoundPool{}
	var poolOrder []int
	for _, f := range fills {
		var k, i int
		var side string
		if _, err := fmt.Sscanf(f.ID, "order:%d", &k); err == nil && !f.Pool {
			r.Orders = append(r.Orders, covenant.RoundTake{Order: orders[k], Coin: s.Orders[k].Coin.coin(), Filled: f.Sold})
			continue
		}
		if _, err := fmt.Sscanf(f.ID, "pool:%d:%s", &i, &side); err != nil {
			die("fill %s", f.ID)
		}
		if poolFill[i] != nil {
			die("both sides of pool %d fill at %d/%d", i, price.N, price.D)
		}
		rp := &covenant.RoundPool{Pool: pools[i], BaseCoin: s.Pools[i].BaseCoin.coin(), QuoteCoin: s.Pools[i].QuoteCoin.coin()}
		if f.Ask {
			rp.Sell, rp.Base, rp.Quote = true, f.Sold, f.Got
		} else {
			rp.Base, rp.Quote = f.Got, f.Sold
		}
		poolFill[i] = rp
		poolOrder = append(poolOrder, i)
	}
	for _, i := range poolOrder {
		r.Pools = append(r.Pools, *poolFill[i])
	}
	for _, c := range s.Funding {
		r.Funding = append(r.Funding, c.coin())
	}
	l, err := covenant.BuildRound(r, hex32(s.Genesis, "genesis"))
	if err != nil {
		die("round: %v", err)
	}
	if err := l.SetClearerSig(sign(key(s.ClearerPriv, "clearer_priv"), l.Digest)); err != nil {
		die("sign: %v", err)
	}
	var coins []coinJSON
	add := func(c covenant.RoundCoin) {
		coins = append(coins, coinJSON{Txid: c.Txid, Vout: c.Vout, Asset: hex.EncodeToString(c.Asset[:]), Value: c.Value, Script: hex.EncodeToString(c.Script)})
	}
	for _, t := range r.Orders {
		add(t.Coin)
	}
	for _, p := range r.Pools {
		add(p.BaseCoin)
		add(p.QuoteCoin)
	}
	for _, c := range r.Funding {
		add(c)
	}
	txOut(l.Tx, map[string]interface{}{"price": []uint64{price.N, price.D}, "digest": hex.EncodeToString(l.Digest[:]),
		"fills": l.Fills, "moves": l.Moves, "allocation": fills, "short_base": sb, "short_quote": sq, "coins": coins})
}

func roundSignCmd() {
	var s struct {
		Tx      string     `json:"tx"`
		Coins   []coinJSON `json:"coins"`
		Genesis string     `json:"genesis"`
		Price   [2]uint64  `json:"price"`
		Base    string     `json:"base"`
		Quote   string     `json:"quote"`
		Orders  []struct {
			K       int       `json:"k"`
			Terms   orderJSON `json:"terms"`
			Partial bool      `json:"partial"`
		} `json:"orders"`
		ClearerPriv string `json:"clearer_priv"`
	}
	readSpec(&s)
	tx, err := transaction.NewTxFromHex(s.Tx)
	if err != nil {
		die("tx: %v", err)
	}
	var spent []covenant.SpentCoin
	for _, c := range s.Coins {
		rc := c.coin()
		spent = append(spent, covenant.ExplicitCoin(rc.Asset, rc.Value, rc.Script))
	}
	d, err := covenant.ClearingDigest(hex32(s.Genesis, "genesis"), tx, spent)
	if err != nil {
		die("digest: %v", err)
	}
	sig := sign(key(s.ClearerPriv, "clearer_priv"), d)
	p := covenant.Price{Base: hex32(s.Base, "base"), Quote: hex32(s.Quote, "quote"), N: s.Price[0], D: s.Price[1]}
	for _, o := range s.Orders {
		w, err := o.Terms.order().FillWitness(o.Partial, p, sig)
		if err != nil {
			die("order %d: %v", o.K, err)
		}
		tx.Inputs[o.K].Witness = w
	}
	txOut(tx, map[string]interface{}{"digest": hex.EncodeToString(d[:]), "sig": hex.EncodeToString(sig[:])})
}

func mustHex(s, name string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		die("%s: %v", name, err)
	}
	return b
}
