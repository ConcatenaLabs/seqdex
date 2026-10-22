package covenant

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/vulpemventures/go-elements/transaction"
)

type templateVectors struct {
	TemplateHash string `json:"template_hash"`
	Addresses    []struct {
		Name         string            `json:"name"`
		Params       map[string]string `json:"params"`
		ScriptPubKey string            `json:"script_pubkey"`
		Leaves       map[string]struct {
			ControlBlock string `json:"control_block"`
			Data         string `json:"data"`
		} `json:"leaves"`
	} `json:"addresses"`
}

func readVectors(t *testing.T, dir string) templateVectors {
	t.Helper()
	b, err := os.ReadFile(dir + "/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v templateVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func h32(t *testing.T, s string) [32]byte {
	t.Helper()
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("hex32 %q", s)
	}
	copy(out[:], b)
	return out
}

func u64hex(t *testing.T, s string) uint64 {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		t.Fatalf("u64 %q", s)
	}
	return binary.BigEndian.Uint64(b)
}

// Every golden vector of seqdex/pool: the terms, the script and the control
// block this package derives are the template's.
func TestPoolVectors(t *testing.T) {
	v := readVectors(t, "seqdex_pool")
	if v.TemplateHash != PoolTemplateHash {
		t.Fatalf("vectors for %s, fills for %s", v.TemplateHash, PoolTemplateHash)
	}
	for _, a := range v.Addresses {
		p := SimPool{Base: h32(t, a.Params["BASE"]), Quote: h32(t, a.Params["QUOTE"]),
			BidNum: u64hex(t, a.Params["BID_NUM"]), BidDen: u64hex(t, a.Params["BID_DEN"]),
			AskNum: u64hex(t, a.Params["ASK_NUM"]), AskDen: u64hex(t, a.Params["ASK_DEN"]),
			MinLot: u64hex(t, a.Params["MIN_LOT"]), ReserveBase: u64hex(t, a.Params["RESERVE_BASE"]),
			ReserveQuote: u64hex(t, a.Params["RESERVE_QUOTE"]), MakerKey: h32(t, a.Params["MAKER_KEY"])}
		tr, err := p.Derive()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(tr.Terms) != a.Leaves["terms"].Data {
			t.Errorf("%s: terms differ", a.Name)
		}
		if hex.EncodeToString(tr.ScriptPubKey) != a.ScriptPubKey {
			t.Errorf("%s: script %x, vector %s", a.Name, tr.ScriptPubKey, a.ScriptPubKey)
		}
		if hex.EncodeToString(tr.ControlBlock) != a.Leaves["pool"].ControlBlock {
			t.Errorf("%s: control block differs", a.Name)
		}
		back, err := PoolFromTerms(tr.Terms)
		if err != nil || back != p {
			t.Errorf("%s: terms do not read back", a.Name)
		}
	}
}

// Every golden vector of seqdex/round-order. The vectors give MAKER as a hash,
// so the terms are built here from it.
func TestRoundOrderVectors(t *testing.T) {
	v := readVectors(t, "seqdex_round_order")
	if v.TemplateHash != RoundOrderTemplateHash {
		t.Fatalf("vectors for %s, fills for %s", v.TemplateHash, RoundOrderTemplateHash)
	}
	for _, a := range v.Addresses {
		var terms []byte
		for _, k := range []string{"ASSET_A", "ASSET_B", "LIMIT_NUM", "LIMIT_DEN", "MIN_LOT", "MAKER", "EXPIRY", "REFUND_KEY", "CLEARER"} {
			b, _ := hex.DecodeString(a.Params[k])
			terms = append(terms, b...)
		}
		tr, err := deriveDataTree(RoundOrderCMR, terms)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(tr.ScriptPubKey) != a.ScriptPubKey || hex.EncodeToString(tr.ControlBlock) != a.Leaves["order"].ControlBlock {
			t.Errorf("%s: derived output differs", a.Name)
		}
	}
	// And Terms() from a maker script: MAKER is its SHA256.
	o := RoundOrder{AssetA: [32]byte{1}, AssetB: [32]byte{2}, LimitNum: 3, LimitDen: 2, MinLot: 5,
		MakerScript: []byte{0x51}, Expiry: 7, RefundKey: [32]byte{3}, Clearer: [32]byte{4}}
	m := sha256.Sum256([]byte{0x51})
	if got := o.Terms(); len(got) != RoundOrderTermsLen || !bytes.Equal(got[88:120], m[:]) {
		t.Fatal("MAKER is not the maker script's hash")
	}
}

type simhashCase struct {
	Name  string `json:"name"`
	Tx    string `json:"tx"`
	Coins []struct {
		Asset  string `json:"asset"`
		Value  uint64 `json:"value"`
		Script string `json:"script"`
	} `json:"coins"`
	Genesis        string `json:"genesis_internal"`
	Index          uint32 `json:"index"`
	CMR            string `json:"cmr"`
	ControlBlock   string `json:"control_block"`
	TxHash         string `json:"tx_hash"`
	ClearingDigest string `json:"clearing_digest"`
	SigAllHash     string `json:"sig_all_hash"`
}

// SimTxHash, ClearingDigest and SimSigAllHash against seqc, which computes them
// with the Simplicity library the chain runs.
func TestSimHashVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/simhash_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []simhashCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		tx, err := transaction.NewTxFromHex(c.Tx)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		var coins []SpentCoin
		for _, k := range c.Coins {
			s, _ := hex.DecodeString(k.Script)
			coins = append(coins, ExplicitCoin(h32(t, k.Asset), k.Value, s))
		}
		th, err := SimTxHash(tx, coins)
		if err != nil || hex.EncodeToString(th[:]) != c.TxHash {
			t.Errorf("%s: tx_hash %x, seqc %s (%v)", c.Name, th, c.TxHash, err)
		}
		g := h32(t, c.Genesis)
		d, err := ClearingDigest(g, tx, coins)
		if err != nil || hex.EncodeToString(d[:]) != c.ClearingDigest {
			t.Errorf("%s: clearing digest %x, seqc %s", c.Name, d, c.ClearingDigest)
		}
		cb, _ := hex.DecodeString(c.ControlBlock)
		s, err := SimSigAllHash(g, tx, coins, c.Index, h32(t, c.CMR), cb)
		if err != nil || hex.EncodeToString(s[:]) != c.SigAllHash {
			t.Errorf("%s: sig_all_hash %x, seqc %s", c.Name, s, c.SigAllHash)
		}
	}
}

func testPool(t *testing.T) (SimPool, []byte) {
	k, _ := btcec.NewPrivateKey()
	var mk [32]byte
	copy(mk[:], schnorr.SerializePubKey(k.PubKey()))
	p := SimPool{Base: [32]byte{0xb}, Quote: [32]byte{0xc}, BidNum: 9, BidDen: 10, AskNum: 11, AskDen: 10,
		MinLot: 1000, ReserveBase: 5000, ReserveQuote: 5000, MakerKey: mk}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	spk, err := p.Script()
	if err != nil {
		t.Fatal(err)
	}
	return p, spk
}

// The pool's judge refuses what its program refuses, rule by rule.
func TestPoolCheckFill(t *testing.T) {
	p, spk := testPool(t)
	xc, yc := Explicit(p.Base, 100_000, spk), Explicit(p.Quote, 100_000, spk)
	sell := func(db, dq uint64) []Output {
		return []Output{Explicit(p.Base, 100_000-db, spk), Explicit(p.Quote, 100_000+dq, spk)}
	}
	buy := func(db, dq uint64) []Output {
		return []Output{Explicit(p.Base, 100_000+db, spk), Explicit(p.Quote, 100_000-dq, spk)}
	}
	ask, _ := p.AskQuote(10_000)
	bid, _ := p.BidQuote(10_000)
	if ask != 11_000 || bid != 9_000 {
		t.Fatalf("quotes %d %d", ask, bid)
	}
	if _, err := p.CheckFill(xc, yc, sell(10_000, ask), 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CheckFill(xc, yc, buy(10_000, bid), 0, false); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		outs []Output
		sell bool
		want string
	}{
		"one atom under the ask":         {sell(10_000, ask-1), true, "below its ask"},
		"one atom over the bid":          {buy(10_000, bid+1), false, "above its bid"},
		"below the base reserve":         {sell(95_001, 200_000), true, "below the reserve"},
		"below the quote reserve":        {buy(200_000, 95_001), false, "below the reserve"},
		"under the min lot":              {sell(999, 2_000), true, "below min_lot"},
		"a sell that takes quote out":    {buy(10_000, bid), true, "a sell pays base out"},
		"the quote coin elsewhere":       {[]Output{Explicit(p.Base, 90_000, spk), Explicit(p.Quote, 111_000, []byte{0x51})}, true, "does not pay the pool's own script"},
		"the base coin in another asset": {[]Output{Explicit(p.Quote, 90_000, spk), Explicit(p.Quote, 111_000, spk)}, true, "holds another asset"},
	}
	for name, c := range cases {
		_, err := p.CheckFill(xc, yc, c.outs, 0, c.sell)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if a, b := p.Depth(100_000, 3_000); a != 95_000 || b != 0 {
		t.Errorf("depth %d %d", a, b)
	}
	bad := p
	bad.BidNum = 12
	if bad.Validate() == nil {
		t.Error("a bid above the ask validated")
	}
}

func rat(n, d int64) *big.Rat { return big.NewRat(n, d) }

// The clearing rule on the harness's round: asks of 4,000,000 at 1 and
// 3,000,000 at 1.05, bids of 5,000,000 quote up to 1.25 and 4,000,000 up to
// 1.15 clear at 23/20, the asks and the better bid whole and the other bid in
// part, the round short by at most one atom of each asset.
func TestClearPriceAndAllocate(t *testing.T) {
	var base, quote [32]byte
	base[0], quote[0] = 1, 2
	sides := []Side{
		{ID: "A1", Ask: true, Size: 4_000_000, Limit: rat(1, 1), MinLot: 1000, Seq: 0},
		{ID: "A2", Ask: true, Size: 3_000_000, Limit: rat(105, 100), MinLot: 1000, Seq: 1},
		{ID: "B1", Size: 5_000_000, Limit: rat(125, 100), MinLot: 1000, Seq: 2},
		{ID: "B2", Size: 4_000_000, Limit: rat(115, 100), MinLot: 1000, Seq: 3},
	}
	p, ok, err := ClearPrice(sides, base, quote)
	if err != nil || !ok || p.N != 23 || p.D != 20 {
		t.Fatalf("price %d/%d ok=%v err=%v", p.N, p.D, ok, err)
	}
	fills, sb, sq, err := Allocate(sides, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Fill{}
	for _, f := range fills {
		got[f.ID] = f
	}
	if got["A1"].Sold != 4_000_000 || got["A2"].Sold != 3_000_000 || got["B1"].Sold != 5_000_000 || got["B2"].Sold != 3_049_998 {
		t.Fatalf("fills %+v", fills)
	}
	if got["A1"].Got != 4_600_000 || got["B2"].Got != 2_652_173 {
		t.Fatalf("credits %+v", fills)
	}
	if sb > 1 || sq > 4 {
		t.Errorf("shortfall %d base, %d quote", sb, sq)
	}
	// A balance point between two limits: every order fills whole at Q/S.
	even := []Side{
		{ID: "a", Ask: true, Size: 4_000_000, Limit: rat(1, 1), MinLot: 1},
		{ID: "b", Ask: true, Size: 3_000_000, Limit: rat(105, 100), MinLot: 1},
		{ID: "c", Size: 5_000_000, Limit: rat(125, 100), MinLot: 1},
		{ID: "d", Size: 3_000_000, Limit: rat(115, 100), MinLot: 1},
	}
	p, ok, _ = ClearPrice(even, base, quote)
	if !ok || p.N != 8 || p.D != 7 {
		t.Fatalf("balance price %d/%d", p.N, p.D)
	}
	// Nothing crosses.
	if _, ok, _ := ClearPrice([]Side{{ID: "a", Ask: true, Size: 1, Limit: rat(2, 1)}, {ID: "b", Size: 1, Limit: rat(1, 1)}}, base, quote); ok {
		t.Fatal("an uncrossed book cleared")
	}
}

// A round laid out by BuildRound is judged by every order's and pool's rules,
// signed once, and its witnesses are the template's layout.
func TestBuildRound(t *testing.T) {
	var base, quote, fee [32]byte
	base[0], quote[0], fee[0] = 1, 2, 3
	clearer, _ := btcec.NewPrivateKey()
	var ck, rk [32]byte
	copy(ck[:], schnorr.SerializePubKey(clearer.PubKey()))
	copy(rk[:], schnorr.SerializePubKey(clearer.PubKey()))
	mk := func(a, b [32]byte, n, d uint64) RoundOrder {
		return RoundOrder{AssetA: a, AssetB: b, LimitNum: n, LimitDen: d, MinLot: 1000, MakerScript: []byte{0x51, 0x20, 9},
			Expiry: 100, RefundKey: rk, Clearer: ck}
	}
	a1, b1 := mk(base, quote, 1, 1), mk(quote, base, 100, 125)
	coin := func(o RoundOrder, id string, v uint64) RoundCoin {
		s, _ := o.Script()
		return RoundCoin{Txid: strings.Repeat(id, 64), Vout: 0, Asset: o.AssetA, Value: v, Script: s}
	}
	p := Price{Base: base, Quote: quote, N: 23, D: 20}
	cb1 := coin(b1, "b", 5_000_000)
	r := Round{Price: p,
		Orders: []RoundTake{{Order: a1, Coin: coin(a1, "a", 4_000_000), Filled: 4_000_000},
			{Order: b1, Coin: cb1, Filled: 4_600_000}},
		Funding:      []RoundCoin{{Txid: strings.Repeat("c", 64), Asset: fee, Value: 100_000, Script: []byte{0x51}}, {Txid: strings.Repeat("d", 64), Asset: base, Value: 1000, Script: []byte{0x51}}},
		ChangeScript: []byte{0x51}, FeeAsset: fee, Fee: 20_000}
	l, err := BuildRound(r, [32]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	last := l.Tx.Outputs[len(l.Tx.Outputs)-1]
	if got, ok := ReadClearing(last.Script); !ok || got != p {
		t.Fatal("the last output is not the clearing output")
	}
	sig, err := schnorr.Sign(clearer, l.Digest[:])
	if err != nil {
		t.Fatal(err)
	}
	var s [64]byte
	copy(s[:], sig.Serialize())
	if err := l.SetClearerSig(s); err != nil {
		t.Fatal(err)
	}
	for k := range r.Orders {
		fill, partial := RoundOrderPath(l.Tx.Inputs[k].Witness)
		if !fill || partial != (k == 1) {
			t.Errorf("input %d: path fill=%v partial=%v", k, fill, partial)
		}
	}
	// Another key's signature is refused before it reaches a witness.
	other, _ := btcec.NewPrivateKey()
	sig2, _ := schnorr.Sign(other, l.Digest[:])
	copy(s[:], sig2.Serialize())
	if err := l.SetClearerSig(s); err == nil {
		t.Fatal("another key signed the round")
	}
	// A credit one atom over the published price is refused by the judge.
	outs := OutputsOf(l.Tx)
	outs[0].Value++
	if _, err := a1.CheckFill(r.Orders[0].Coin.output(), outs, 0, false); err == nil || !strings.Contains(err.Error(), "published price") {
		t.Fatalf("credit over the price: %v", err)
	}
	// A price under a limit is refused when the round is laid out.
	r.Price = Price{Base: base, Quote: quote, N: 99, D: 100}
	if _, err := BuildRound(r, [32]byte{7}); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("a price under a limit: %v", err)
	}
}

// The vendored pool and round-order templates are the ones their PIN.json names.
func TestPoolAndRoundOrderPin(t *testing.T) {
	for _, dir := range []string{"seqdex_pool", "seqdex_round_order"} {
		pinned(t, dir)
	}
}
