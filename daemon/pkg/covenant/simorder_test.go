package covenant

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// The vendored templates, version 2 in seqdex_order_2/ and version 1 in
// seqdex_order/: each one's descriptor, golden vectors and pruned programs,
// copied byte for byte from sequentia-contracts at the commit its PIN.json
// names.
var simDirs = map[*SimTemplate]string{SimOrderV2: "seqdex_order_2", SimOrderV1: "seqdex_order"}

func readJSON(t *testing.T, name string, v interface{}) []byte {
	return readJSONIn(t, "seqdex_order_2", name, v)
}

func readJSONIn(t *testing.T, dir, name string, v interface{}) []byte {
	t.Helper()
	b, err := os.ReadFile(dir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return b
}

// canonical is the descriptor's canonical JSON: keys sorted, no whitespace.
func canonical(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func TestSimOrderTemplateIsPinned(t *testing.T) {
	if SimOrderTemplateHash != SimOrderV2.TemplateHash || SimOrderCMR != SimOrderV2.CMR || SimOrderV1.CMR == SimOrderV2.CMR {
		t.Fatal("new orders are not version 2's, or the versions share a root")
	}
	for tm, dir := range simDirs {
		templateIsPinned(t, tm, dir)
	}
}

func templateIsPinned(t *testing.T, tm *SimTemplate, dir string) {
	var d struct {
		Template     map[string]interface{} `json:"template"`
		TemplateHash string                 `json:"template_hash"`
	}
	dec := json.NewDecoder(bytes.NewReader(readJSONIn(t, dir, "descriptor.json", nil)))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(canonical(t, d.Template))
	if hex.EncodeToString(h[:]) != d.TemplateHash {
		t.Fatalf("descriptor.json's template hashes to %x, it says %s", h, d.TemplateHash)
	}
	if d.TemplateHash != tm.TemplateHash {
		t.Fatalf("%s: descriptor %s, fills.json %s", dir, d.TemplateHash, tm.TemplateHash)
	}
	if v, _ := d.Template["version"].(json.Number).Int64(); v != int64(tm.Version) || d.Template["name"] != "seqdex/order" {
		t.Fatalf("%s: template %v version %v", dir, d.Template["name"], d.Template["version"])
	}
	if got, err := SimTemplateOf(strings.ToUpper(d.TemplateHash)); err != nil || got != tm {
		t.Fatalf("%s: SimTemplateOf %v %v", dir, got, err)
	}
	tree := d.Template["tree"].(map[string]interface{})["branch"].([]interface{})
	cmr := tree[0].(map[string]interface{})["simplicity"].(map[string]interface{})["cmr"].(string)
	if cmr != hex.EncodeToString(tm.CMR[:]) {
		t.Fatalf("%s: descriptor root %s, fills.json %x", dir, cmr, tm.CMR)
	}
	var data []string
	for _, v := range tree[1].(map[string]interface{})["data"].([]interface{}) {
		data = append(data, v.(string))
	}
	if got := strings.Join(data, ","); got != "ASSET_A,ASSET_B,RATE_NUM,RATE_DEN,MIN_LOT,MAKER,EXPIRY,REFUND_KEY" {
		t.Fatalf("the data leaf's order is %s", got)
	}
}

type simVector struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params"`
	Leaves map[string]struct {
		Hash         string `json:"hash"`
		ControlBlock string `json:"control_block"`
		Data         string `json:"data"`
	} `json:"leaves"`
	MerkleRoot      string            `json:"merkle_root"`
	OutputKey       string            `json:"output_key"`
	OutputKeyParity byte              `json:"output_key_parity"`
	ScriptPubKey    string            `json:"script_pubkey"`
	Address         map[string]string `json:"address"`
}

func vectorTerms(t *testing.T, p map[string]string) []byte {
	t.Helper()
	var b []byte
	for _, k := range []string{"ASSET_A", "ASSET_B", "RATE_NUM", "RATE_DEN", "MIN_LOT", "MAKER", "EXPIRY", "REFUND_KEY"} {
		v, err := hex.DecodeString(p[k])
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, v...)
	}
	return b
}

// Every golden vector of the template: the leaves, the root, the output key
// and its parity, the script, the control block and both addresses, derived
// with no compiler.
func TestSimOrderVectors(t *testing.T) {
	for tm, dir := range simDirs {
		vectors(t, tm, dir)
	}
	if _, err := SimTemplateOf("322c0e59"); err == nil {
		t.Fatal("a prefix of a template hash names a version")
	}
}

func vectors(t *testing.T, tm *SimTemplate, dir string) {
	var v struct {
		TemplateHash string      `json:"template_hash"`
		Addresses    []simVector `json:"addresses"`
	}
	readJSONIn(t, dir, "vectors.json", &v)
	if v.TemplateHash != tm.TemplateHash || len(v.Addresses) < 4 {
		t.Fatalf("vectors for %s, %d of them", v.TemplateHash, len(v.Addresses))
	}
	for _, c := range v.Addresses {
		terms := vectorTerms(t, c.Params)
		if hex.EncodeToString(terms) != c.Leaves["terms"].Data {
			t.Fatalf("%s: data leaf bytes differ", c.Name)
		}
		x, err := deriveSimTerms(terms, tm)
		if err != nil {
			t.Fatal(err)
		}
		check := func(what, got, want string) {
			if got != want {
				t.Errorf("%s: %s %s, want %s", c.Name, what, got, want)
			}
		}
		check("program leaf", hex.EncodeToString(x.ProgramLeafHash[:]), c.Leaves["order"].Hash)
		check("terms leaf", hex.EncodeToString(x.TermsLeafHash[:]), c.Leaves["terms"].Hash)
		check("control block", hex.EncodeToString(x.ControlBlock), c.Leaves["order"].ControlBlock)
		check("merkle root", hex.EncodeToString(x.MerkleRoot[:]), c.MerkleRoot)
		check("output key", hex.EncodeToString(x.OutputKey[:]), c.OutputKey)
		check("script", hex.EncodeToString(x.ScriptPubKey), c.ScriptPubKey)
		if x.Parity != c.OutputKeyParity {
			t.Errorf("%s: parity %d, want %d", c.Name, x.Parity, c.OutputKeyParity)
		}
		for chain, hrp := range map[string]string{"sequentia-testnet": "tb", "elementsregtest": "ert"} {
			a, err := x.Address(hrp)
			if err != nil {
				t.Fatal(err)
			}
			check(chain+" address", a, c.Address[chain])
		}
	}
}

func testSimOrder(t *testing.T) SimOrder {
	t.Helper()
	k := sha256.Sum256([]byte("refund key"))
	_, pub := btcecPriv(k[:])
	var rk [32]byte
	copy(rk[:], schnorr.SerializePubKey(pub))
	return SimOrder{
		AssetA: sha256.Sum256([]byte("asset A")), AssetB: sha256.Sum256([]byte("asset B")),
		RateNum: 9_000_000_000_007, RateDen: 10_000_000_000_003, MinLot: 1_000_000,
		MakerScript: append([]byte{0x00, 0x14}, bytes.Repeat([]byte{7}, 20)...),
		Expiry:      1_900_000_000, RefundKey: rk,
	}
}

// The terms an order puts in its data leaf, the MAKER term the SHA256 of its
// whole script.
func TestSimOrderTerms(t *testing.T) {
	o := testSimOrder(t)
	b := o.Terms()
	if len(b) != 156 {
		t.Fatalf("%d bytes", len(b))
	}
	m := sha256.Sum256(o.MakerScript)
	if !bytes.Equal(b[88:120], m[:]) || binary.BigEndian.Uint64(b[64:72]) != o.RateNum ||
		binary.BigEndian.Uint64(b[80:88]) != o.MinLot || binary.BigEndian.Uint32(b[120:124]) != o.Expiry {
		t.Fatalf("terms %x", b)
	}
}

// A fill's witness stack is the constant program of its path, the root, the
// control block, and the terms followed by PATH; a refund's carries the
// signature after one bit of PATH. Each version spends by its own programs
// and root, and the same terms give each version its own address.
func TestSimOrderWitnesses(t *testing.T) {
	o := testSimOrder(t)
	x2, _ := o.Derive()
	o1 := o
	o1.Template = SimOrderV1
	x1, _ := o1.Derive()
	if bytes.Equal(x1.ScriptPubKey, x2.ScriptPubKey) {
		t.Fatal("version 1 and version 2 derive one address")
	}
	if o2 := o; true {
		o2.Template = SimOrderV2
		if x, _ := o2.Derive(); !bytes.Equal(x.ScriptPubKey, x2.ScriptPubKey) {
			t.Fatal("an order naming no version is not version 2")
		}
	}
	for tm, sizes := range map[*SimTemplate][3]int{SimOrderV2: {1105, 994, 516}, SimOrderV1: {1092, 981, 505}} {
		o := o
		o.Template = tm
		witnesses(t, o, sizes)
	}
}

func witnesses(t *testing.T, o SimOrder, sizes [3]int) {
	x, _ := o.Derive()
	simPrograms, cmr := o.tmpl().programs, o.tmpl().CMR
	for _, partial := range []bool{true, false} {
		w, err := o.FillWitness(partial)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]byte{}, o.Terms()...), map[bool]byte{true: 0x40, false: 0x00}[partial])
		prog := simPrograms[map[bool]string{true: "fill", false: "fill_whole"}[partial]]
		if len(w) != 4 || !bytes.Equal(w[0], want) || !bytes.Equal(w[1], prog) ||
			!bytes.Equal(w[2], cmr[:]) || !bytes.Equal(w[3], x.ControlBlock) {
			t.Fatalf("partial=%v: witness stack differs", partial)
		}
		if f, p := SimFillPath(w); !f || p != partial {
			t.Fatalf("partial=%v: SimFillPath reads %v %v", partial, f, p)
		}
	}
	if len(simPrograms["fill"]) != sizes[0] || len(simPrograms["fill_whole"]) != sizes[1] || len(simPrograms["refund_time"]) != sizes[2] {
		t.Fatalf("program sizes %d %d", len(simPrograms["fill"]), len(simPrograms["fill_whole"]))
	}
	var sig [64]byte
	for i := range sig {
		sig[i] = byte(0xa5 ^ i)
	}
	for _, exp := range []uint32{499_999_999, 500_000_000} {
		o.Expiry = exp
		w, err := o.RefundWitness(sig)
		if err != nil {
			t.Fatal(err)
		}
		if len(w[0]) != 221 || w[0][156]&0x80 == 0 {
			t.Fatalf("refund witness %x", w[0])
		}
		// Shift the signature back out of the bit string.
		var got [64]byte
		for i := range got {
			got[i] = w[0][156+i]<<1 | w[0][157+i]>>7
		}
		if got != sig || w[0][220]&0x7f != 0 {
			t.Fatalf("expiry %d: signature not in place", exp)
		}
		kind := map[bool]string{true: "refund_height", false: "refund_time"}[exp < 500_000_000]
		if !bytes.Equal(w[1], simPrograms[kind]) {
			t.Fatalf("expiry %d: not the %s program", exp, kind)
		}
		if f, _ := SimFillPath(w); f {
			t.Fatal("a refund read as a fill")
		}
	}
}

// CheckFill judges as the program does, at any k, and names each rule.
func TestSimOrderCheckFill(t *testing.T) {
	o := testSimOrder(t)
	x, _ := o.Derive()
	const locked = 20_000_000_000_000
	coin := Explicit(o.AssetA, locked, x.ScriptPubKey)
	// The product 2e13 * 9e12 is far beyond what the tapscript leaf computes.
	if err := (Order{RateNum: o.RateNum, RateDen: o.RateDen}).CheckArithmetic(locked); err == nil {
		t.Fatal("the tapscript bound should refuse this order")
	}
	fill := uint64(6_000_000_000_001)
	paid := CeilPrice(fill, o.RateNum, o.RateDen)
	other := sha256.Sum256([]byte("asset C"))
	taker := []byte{0x51, 0x20, 1, 2, 3}
	layout := func(k int, credit, rest Output) []Output {
		var outs []Output
		for i := 0; i < k; i++ {
			outs = append(outs, Explicit(other, 1, taker), Explicit(other, 1, taker))
		}
		return append(outs, credit, rest, Explicit(o.AssetA, fill, taker))
	}
	credit := Explicit(o.AssetB, paid, o.MakerScript)
	rest := Explicit(o.AssetA, locked-fill, x.ScriptPubKey)
	for _, k := range []int{0, 1, 3} {
		c, err := o.CheckFill(coin, layout(k, credit, rest), uint32(k), true)
		if err != nil || c.Filled != fill || c.Remainder != locked-fill || c.Required != paid || c.CreditIndex != uint32(2*k) {
			t.Fatalf("k=%d: %v %+v", k, err, c)
		}
	}
	whole := Explicit(o.AssetB, CeilPrice(locked, o.RateNum, o.RateDen), o.MakerScript)
	if c, err := o.CheckFill(coin, []Output{whole, Explicit(o.AssetA, locked, taker)}, 0, false); err != nil || c.Filled != locked {
		t.Fatalf("whole: %v", err)
	}
	mod := func(f func(*Output)) Output { r := rest; f(&r); return r }
	modc := func(f func(*Output)) Output { r := credit; f(&r); return r }
	bad := []struct {
		name   string
		outs   []Output
		coin   Output
		k      uint32
		reason string
	}{
		{"credit one atom under", layout(0, modc(func(c *Output) { c.Value-- }), rest), coin, 0, "below the price"},
		{"credit in another asset", layout(0, modc(func(c *Output) { c.Asset = other }), rest), coin, 0, "another asset"},
		{"credit confidential", layout(0, modc(func(c *Output) { c.ValueExplicit = false }), rest), coin, 0, "confidential"},
		{"credit elsewhere", layout(0, modc(func(c *Output) { c.Script = taker }), rest), coin, 0, "maker's script"},
		{"credit with a nonce", layout(0, modc(func(c *Output) { c.HasNonce = true }), rest), coin, 0, "nonce"},
		{"rest elsewhere", layout(0, credit, mod(func(r *Output) { r.Script = taker })), coin, 0, "own script"},
		{"rest in another asset", layout(0, credit, mod(func(r *Output) { r.Asset = other })), coin, 0, "another asset"},
		{"rest with a nonce", layout(0, credit, mod(func(r *Output) { r.HasNonce = true })), coin, 0, "nonce"},
		{"rest confidential", layout(0, credit, mod(func(r *Output) { r.AssetExplicit = false })), coin, 0, "confidential"},
		{"rest below min_lot", layout(0, credit, mod(func(r *Output) { r.Value = o.MinLot - 1 })), coin, 0, "below min_lot"},
		{"order at input 1 paying at 0 and 1", layout(0, credit, rest), coin, 1, "rest"},
		{"coin of another asset", layout(0, credit, rest), Explicit(other, locked, x.ScriptPubKey), 0, "explicitly"},
		{"coin confidential", layout(0, credit, rest), Output{Asset: o.AssetA, AssetExplicit: true, Script: x.ScriptPubKey}, 0, "explicitly"},
	}
	for _, b := range bad {
		if _, err := o.CheckFill(b.coin, b.outs, b.k, true); err == nil || !strings.Contains(err.Error(), b.reason) {
			t.Errorf("%s: %v", b.name, err)
		}
	}
	// The extreme price (2^64-1)/(2^64-2): one atom more than the fill.
	e := o
	e.RateNum, e.RateDen, e.MinLot = math.MaxUint64, math.MaxUint64-1, 1
	ex, _ := e.Derive()
	ec := Explicit(e.AssetA, 1_000_000_000, ex.ScriptPubKey)
	ok := []Output{Explicit(e.AssetB, 400_000_001, e.MakerScript), Explicit(e.AssetA, 600_000_000, ex.ScriptPubKey)}
	if _, err := e.CheckFill(ec, ok, 0, true); err != nil {
		t.Fatal(err)
	}
	ok[0].Value--
	if _, err := e.CheckFill(ec, ok, 0, true); err == nil {
		t.Fatal("the extreme price one atom under was accepted")
	}
}

func TestSimOrderPlanAndValidate(t *testing.T) {
	o := testSimOrder(t)
	p, err := o.PlanFill(20_000_000, 5_000_000, 1)
	if err != nil || !p.Partial || p.CreditIndex != 2 || p.RemainderIndex != 3 || len(p.Witness()) != 4 ||
		p.Witness()[0][156] != 0x40 {
		t.Fatalf("%v %+v", err, p)
	}
	if p, err = o.PlanFill(20_000_000, 20_000_000, 0); err != nil || p.Partial || p.Witness()[0][156] != 0 {
		t.Fatalf("whole: %v", err)
	}
	if _, err := o.PlanFill(20_000_000, 19_500_000, 0); err == nil {
		t.Fatal("a rest below min_lot was planned")
	}
	for name, f := range map[string]func(*SimOrder){
		"rate_den 0":  func(o *SimOrder) { o.RateDen = 0 },
		"rate_num 0":  func(o *SimOrder) { o.RateNum = 0 },
		"min_lot 0":   func(o *SimOrder) { o.MinLot = 0 },
		"same assets": func(o *SimOrder) { o.AssetB = o.AssetA },
		"no maker":    func(o *SimOrder) { o.MakerScript = nil },
		"bad key":     func(o *SimOrder) { o.RefundKey = [32]byte{} },
	} {
		b := o
		f(&b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func btcecPriv(b []byte) (*btcec.PrivateKey, *btcec.PublicKey) { return btcec.PrivKeyFromBytes(b) }

// The vendored files are the ones each PIN.json names, byte for byte.
func TestSimOrderPin(t *testing.T) {
	for _, dir := range simDirs {
		pinned(t, dir)
	}
}

func pinned(t *testing.T, dir string) {
	var pin struct {
		Commit string            `json:"commit"`
		Path   string            `json:"path"`
		SHA256 map[string]string `json:"sha256"`
	}
	readJSONIn(t, dir, "PIN.json", &pin)
	if len(pin.Commit) != 40 || pin.Path != "templates/"+dir || len(pin.SHA256) != 3 {
		t.Fatalf("%s/PIN.json: %+v", dir, pin)
	}
	for name, want := range pin.SHA256 {
		h := sha256.Sum256(readJSONIn(t, dir, name, nil))
		if hex.EncodeToString(h[:]) != want {
			t.Errorf("%s hashes to %x, PIN.json says %s", name, h, want)
		}
	}
}
