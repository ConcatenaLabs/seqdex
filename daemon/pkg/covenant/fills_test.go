package covenant

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateVectors = flag.Bool("update-vectors", false, "rewrite testdata/fill_vectors.json from the Go builder")

// The vector file pins the multi-order fill layout across implementations:
// the web wallet's covenant.js builds the same transactions from the same
// specs and must produce these bytes.
const vectorFile = "testdata/fill_vectors.json"

type vecPlan struct {
	InputIndex     uint32 `json:"input_index"`
	CreditIndex    uint32 `json:"credit_index"`
	RemainderIndex uint32 `json:"remainder_index"`
	RequiredB      uint64 `json:"required_b"`
	Remainder      uint64 `json:"remainder"`
	Partial        bool   `json:"partial"`
}

type vector struct {
	Name string `json:"name"`
	Spec
	Expect struct {
		Plans []vecPlan `json:"plans"`
		Hex   string    `json:"hex"`
		Txid  string    `json:"txid"`
	} `json:"expect"`
}

type vectorSet struct {
	About   string   `json:"about"`
	Vectors []vector `json:"vectors"`
}

func (v vector) spec(t *testing.T) FillTx {
	t.Helper()
	f, err := v.Spec.FillTx()
	if err != nil {
		t.Fatalf("%s: %v", v.Name, err)
	}
	return f
}

func rep(b byte) string { return strings.Repeat(hex.EncodeToString([]byte{b}), 32) }

// specVectors are the specs the vector file carries. Their expected bytes are
// whatever BuildFillTx makes of them, written with -update-vectors and then
// held fixed: a change to the layout fails here and in the JavaScript mirror.
func specVectors() []vector {
	x, y, z, pol := rep(0x0a), rep(0x0b), rep(0x0c), rep(0x0d)
	terms := func(a, b string, num, den, lot uint64, prog, mx byte, expiry int64) SpecTerms {
		return SpecTerms{AssetA: a, AssetB: b, RateNum: num, RateDen: den, MakerProg: rep(prog), MakerProgVer: 1, MinLot: lot,
			ExpiryLocktime: expiry, MakerX: rep(mx)}
	}
	operator := "5120" + rep(0x77)
	batch := "5120" + rep(0x66)
	var vs []vector

	// The round A3.2 ran: order 1 (X for Y at 2/1, lots of 10,000) filled in
	// part at input 0, order 2 (X for Z at 5/2) filled whole at input 1 with a
	// Z gap to the operator, the batch and the fee after them.
	v1 := vector{Name: "two orders at k=0 (part) and k=1 (whole), as an Arca fill round lays them out"}
	v1.Takes = []SpecTake{
		{Terms: terms(x, y, 2, 1, 10000, 0x31, 0x41, 1000), Txid: rep(0xa1), Vout: 0, Locked: 600000, Filled: 499500},
		{Terms: terms(x, z, 5, 2, 1000, 0x32, 0x42, 1700000000), Txid: rep(0xa2), Vout: 3, Locked: 400000, Filled: 400000},
	}
	v1.Gaps = []*SpecOutput{nil, {Asset: z, Value: 1, Script: operator}}
	v1.Inputs = []Outpoint{{Txid: rep(0xb1), Vout: 1}, {Txid: rep(0xb2), Vout: 0}}
	v1.Tail = []SpecOutput{{Asset: x, Value: 899500, Script: batch}, {Asset: y, Value: 1000, Script: operator}, {Asset: pol, Value: 2600, Script: ""}}
	vs = append(vs, v1)

	// A taker's own fill of one order spent beside another covenant input: the
	// order this vector is about sits at k=1, credit at 2 and gap at 3.
	v2 := vector{Name: "an order filled whole at k=1 beside a partly filled order at k=0, a taker's change and fee after"}
	v2.Takes = []SpecTake{
		{Terms: terms(y, x, 1, 3, 500, 0x33, 0x43, 2500), Txid: rep(0xa3), Vout: 2, Locked: 9000, Filled: 3000},
		{Terms: terms(x, y, 7, 3, 100, 0x34, 0x44, 1800000000), Txid: rep(0xa4), Vout: 1, Locked: 3000, Filled: 3000},
	}
	taker := "0014" + strings.Repeat("55", 20)
	v2.Gaps = []*SpecOutput{nil, {Asset: pol, Value: 1500, Script: ""}}
	v2.Inputs = []Outpoint{{Txid: rep(0xb3), Vout: 4}}
	v2.Tail = []SpecOutput{{Asset: y, Value: 3000, Script: taker}, {Asset: x, Value: 3000, Script: taker}, {Asset: x, Value: 25, Script: taker}}
	vs = append(vs, v2)
	return vs
}

func TestFillVectors(t *testing.T) {
	if *updateVectors {
		vs := specVectors()
		for i := range vs {
			tx, plans, err := BuildFillTx(vs[i].spec(t))
			if err != nil {
				t.Fatalf("%s: %v", vs[i].Name, err)
			}
			for _, p := range plans {
				vs[i].Expect.Plans = append(vs[i].Expect.Plans, vecPlan{p.InputIndex, p.CreditIndex, p.RemainderIndex, p.RequiredB, p.Remainder, p.Partial})
			}
			raw, err := tx.ToHex()
			if err != nil {
				t.Fatal(err)
			}
			vs[i].Expect.Hex = raw
			vs[i].Expect.Txid = tx.TxHash().String()
		}
		b, _ := json.MarshalIndent(vectorSet{
			About: "Multi-order covenant fill transactions: SeqDEX's pkg/covenant BuildFillTx and the web wallet's covenant.js buildFillTx " +
				"build these bytes from these specs. Asset ids, programs and keys are internal byte order; txids display order. " +
				"Take k is spent at input k, its credit at output 2k, its remainder or gap at 2k+1.",
			Vectors: vs,
		}, "", "  ")
		if err := os.WriteFile(vectorFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var set vectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range set.Vectors {
		f := v.spec(t)
		tx, plans, err := BuildFillTx(f)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		got, _ := tx.ToHex()
		if got != v.Expect.Hex {
			t.Fatalf("%s: hex\n got %s\nwant %s", v.Name, got, v.Expect.Hex)
		}
		if tx.TxHash().String() != v.Expect.Txid {
			t.Fatalf("%s: txid %s, want %s", v.Name, tx.TxHash(), v.Expect.Txid)
		}
		for k, p := range plans {
			e := v.Expect.Plans[k]
			if p.InputIndex != uint32(k) || p.CreditIndex != 2*uint32(k) || p.RemainderIndex != 2*uint32(k)+1 ||
				(vecPlan{p.InputIndex, p.CreditIndex, p.RemainderIndex, p.RequiredB, p.Remainder, p.Partial}) != e {
				t.Fatalf("%s: take %d plan %+v, want %+v", v.Name, k, p, e)
			}
			c, err := f.Takes[k].Order.CheckFill(OutputsOf(tx), uint32(k), f.Takes[k].Locked)
			if err != nil {
				t.Fatalf("%s: take %d: %v", v.Name, k, err)
			}
			if c.Filled != f.Takes[k].Filled || c.Remainder != p.Remainder || c.Credited < p.RequiredB {
				t.Fatalf("%s: take %d read %+v", v.Name, k, c)
			}
			if len(tx.Inputs[k].Witness) != 2 {
				t.Fatalf("%s: input %d witness is not [leaf, control block]", v.Name, k)
			}
		}
		t.Logf("%s: %s (%d inputs, %d outputs)", v.Name, v.Expect.Txid, len(tx.Inputs), len(tx.Outputs))
	}
}

// twoTakes is vector 1's spec, for the negative cases.
func twoTakes(t *testing.T) FillTx { return specVectors()[0].spec(t) }

func TestCheckFillRefusesWhatTheLeafRefuses(t *testing.T) {
	f := twoTakes(t)
	tx, _, err := BuildFillTx(f)
	if err != nil {
		t.Fatal(err)
	}
	good := OutputsOf(tx)
	o0, o1 := f.Takes[0].Order, f.Takes[1].Order
	l0, l1 := f.Takes[0].Locked, f.Takes[1].Locked
	clone := func() []Output { return append([]Output(nil), good...) }
	cases := []struct {
		name  string
		k     uint32
		order Order
		lock  uint64
		mut   func([]Output) []Output
		want  string
	}{
		{"credits swapped between 0 and 2", 0, o0, l0, func(o []Output) []Output { o[0], o[2] = o[2], o[0]; return o },
			"does not hold the asset the order is paid in"},
		{"order 1's credit one atom short", 1, o1, l1, func(o []Output) []Output { o[2].Value--; return o }, "below the ceiling price"},
		{"order 0's credit to another program", 0, o0, l0, func(o []Output) []Output {
			o[0].Script = append([]byte{0x51, 0x20}, make([]byte, 32)...)
			return o
		}, "does not pay the maker's program"},
		{"order 1's gap in the asset it sells", 1, o1, l1, func(o []Output) []Output { o[3].Asset = o1.AssetA; return o },
			"does not pay the order's own script"},
		{"order 1's gap confidential", 1, o1, l1, func(o []Output) []Output { o[3].AssetExplicit = false; return o }, "hides its asset"},
		{"order 0's credit confidential", 0, o0, l0, func(o []Output) []Output { o[0].ValueExplicit = false; return o }, "hides the maker's credit"},
		{"order 0's rest to another script", 0, o0, l0, func(o []Output) []Output { o[1].Script = o[4].Script; return o },
			"does not pay the order's own script"},
		{"order 0's rest below min_lot", 0, o0, l0, func(o []Output) []Output { o[1].Value = o0.MinLot - 1; return o }, "below min_lot"},
		{"order 0's rest leaves a fill below min_lot", 0, o0, l0, func(o []Output) []Output { o[1].Value = l0 - 1; return o }, "below min_lot"},
		{"order 1 with no credit output", 1, o1, l1, func(o []Output) []Output { return o[:2] }, "no output 2"},
		{"the batch moved to order 0's rest slot", 0, o0, l0, func(o []Output) []Output { o[1], o[4] = o[4], o[1]; return o },
			"does not pay the order's own script"},
	}
	for _, c := range cases {
		if _, err := c.order.CheckFill(good, c.k, c.lock); err != nil {
			t.Fatalf("%s: the unchanged layout is refused: %v", c.name, err)
		}
		_, err := c.order.CheckFill(c.mut(clone()), c.k, c.lock)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want %q", c.name, err, c.want)
		}
		t.Logf("%s: %v", c.name, err)
	}
}

func TestBuildFillTxFailsClosed(t *testing.T) {
	base := twoTakes(t)
	cases := []struct {
		name string
		mut  func(*FillTx)
		want string
	}{
		{"a full fill with no gap", func(f *FillTx) { f.Gaps = nil }, "has no gap output"},
		{"a gap in the asset the order sells", func(f *FillTx) {
			g := Explicit(f.Takes[1].Order.AssetA, 1, nil)
			f.Gaps = []*Output{nil, &g}
		}, "holds the asset the order sells"},
		{"a credit below the price", func(f *FillTx) { f.Credits = []uint64{1, 0} }, "below the ceiling price"},
		{"one order taken twice", func(f *FillTx) {
			f.Takes[1].Txid, f.Takes[1].Vout = f.Takes[0].Txid, f.Takes[0].Vout
		}, "taken twice"},
		{"a confidential tail output", func(f *FillTx) { f.Tail[0].AssetExplicit = false }, "is not explicit"},
	}
	for _, c := range cases {
		f := base
		f.Takes = append([]Take(nil), base.Takes...)
		f.Gaps = append([]*Output(nil), base.Gaps...)
		f.Tail = append([]Output(nil), base.Tail...)
		c.mut(&f)
		_, _, err := BuildFillTx(f)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestPlanFillsIndexMap(t *testing.T) {
	o := fixedOrder()
	var takes []Take
	for i := 0; i < 4; i++ {
		takes = append(takes, Take{Order: o, Txid: rep(byte(i + 1)), Locked: 90e8, Filled: 30e8})
	}
	plans, err := PlanFills(takes)
	if err != nil {
		t.Fatal(err)
	}
	for k, p := range plans {
		if p.InputIndex != uint32(k) || p.CreditIndex != 2*uint32(k) || p.RemainderIndex != 2*uint32(k)+1 {
			t.Fatalf("take %d: %d/%d/%d", k, p.InputIndex, p.CreditIndex, p.RemainderIndex)
		}
	}
}
