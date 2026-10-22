package covenant

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/vulpemventures/go-elements/address"
)

// SimOrder is a resting order under the Simplicity template seqdex/order
// (sequentia-contracts): one program for every order, the order's terms held
// as data in a leaf beside it. Two versions of the template exist, each with
// its own program and so its own addresses: version 2
// (templates/seqdex_order_2), which every new order uses, and version 1
// (templates/seqdex_order), the same program without the rule that a spend
// carries no annex. A fill signs nothing, so an annex on a version 1 fill is
// bound by nothing: whoever relays the fill can attach one and bring its fee
// rate down to the relay floor. Orders already resting under version 1 are
// still derived and filled here until they are gone.
//
//	P2TR(NUMS, TapBranch(TapLeaf_0xbe(program), H_TapData(terms)))
//
// The program's commitment root is one constant per version (SimTemplate.CMR), so an order's
// address is that root plus its terms, and this file derives it with one hash
// per leaf and one curve tweak. A fill pays the maker at output 2k at least
// ceil(fill * RateNum / RateDen) of AssetB, checked on 128-bit products, so
// every 64-bit amount and price fills; a partial fill leaves the rest at 2k+1
// under the same script. The refund key takes the coin back once the lock time
// reaches Expiry, a block height below 500,000,000 or a median time.
//
// Every spend's pruned program is one of the constants of the template's
// fills.json, vendored in seqdex_order_2/ and seqdex_order/ beside its
// descriptor and vectors (see each PIN.json), so a fill is built here with no
// compiler. A spend built here carries no annex.
type SimOrder struct {
	AssetA      [32]byte // the asset the order sells, internal byte order
	AssetB      [32]byte // the asset a fill pays in, internal byte order
	RateNum     uint64   // RateNum atoms of AssetB ...
	RateDen     uint64   // ... for every RateDen atoms of AssetA
	MinLot      uint64   // the least a fill takes and a remainder holds
	MakerScript []byte   // the scriptPubKey every fill pays; the MAKER term is its SHA256
	Expiry      uint32   // lock time: below 500,000,000 a height, else a median time
	RefundKey   [32]byte // x-only key that takes the coin back after Expiry
	// Template is the version of seqdex/order the order is under: SimOrderV2,
	// or SimOrderV1 for an order resting from before it. nil is SimOrderV2.
	Template *SimTemplate
}

// SimTemplate is one version of seqdex/order: its template hash, which an
// offer signs, its commitment root and its pruned programs.
type SimTemplate struct {
	Version      int
	TemplateHash string
	CMR          [32]byte
	programs     map[string][]byte
}

// Name says which version it is, for a book or a log.
func (t *SimTemplate) Name() string {
	if t.Version == 1 {
		return "seqdex/order version 1 (its fills take an annex no one signs)"
	}
	return fmt.Sprintf("seqdex/order version %d", t.Version)
}

// SimTree is a SimOrder's derived output.
type SimTree struct {
	Terms           []byte   // the data leaf's 156 bytes
	ProgramLeafHash [32]byte // TapLeaf/elements(0xbe || 0x20 || cmr)
	TermsLeafHash   [32]byte // TapData(terms)
	MerkleRoot      [32]byte
	OutputKey       [32]byte
	Parity          byte
	ScriptPubKey    []byte // OP_1 <output key>
	ControlBlock    []byte // the program leaf's: (0xbe | parity) || NUMS || terms leaf hash
}

const simLeafVersion = 0xbe

//go:embed seqdex_order/fills.json
var simFillsV1JSON []byte

//go:embed seqdex_order_2/fills.json
var simFillsV2JSON []byte

type simFillsFile struct {
	Fills        int    `json:"fills"`
	TemplateHash string `json:"template_hash"`
	CMR          string `json:"cmr"`
	Paths        map[string]struct {
		PathBits     string `json:"path_bits"`
		Signature    bool   `json:"signature"`
		Program      string `json:"program"`
		ProgramBytes int    `json:"program_bytes"`
	} `json:"paths"`
}

// The two versions, from the vendored fills.json files, and version 2's
// template hash and root, which every new order is under.
var (
	SimOrderV1           = loadSimTemplate(1, "seqdex_order", simFillsV1JSON)
	SimOrderV2           = loadSimTemplate(2, "seqdex_order_2", simFillsV2JSON)
	SimOrderTemplateHash = SimOrderV2.TemplateHash
	SimOrderCMR          = SimOrderV2.CMR
)

// SimTemplates are the versions this build derives and fills, newest first.
func SimTemplates() []*SimTemplate { return []*SimTemplate{SimOrderV2, SimOrderV1} }

// SimTemplateOf is the version whose template hash is hash.
func SimTemplateOf(hash string) (*SimTemplate, error) {
	for _, t := range SimTemplates() {
		if strings.EqualFold(hash, t.TemplateHash) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("template %q is not seqdex/order version 2 (%s) or version 1 (%s), the ones this build fills",
		hash, SimOrderV2.TemplateHash, SimOrderV1.TemplateHash)
}

func loadSimTemplate(version int, dir string, raw []byte) *SimTemplate {
	var f simFillsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		panic(dir + "/fills.json: " + err.Error())
	}
	t := &SimTemplate{Version: version, TemplateHash: f.TemplateHash, CMR: mustHex32(f.CMR), programs: map[string][]byte{}}
	for _, p := range []string{"fill", "fill_whole", "refund_height", "refund_time"} {
		b, err := hex.DecodeString(f.Paths[p].Program)
		if err != nil || len(b) == 0 || len(b) != f.Paths[p].ProgramBytes {
			panic(dir + "/fills.json: path " + p)
		}
		t.programs[p] = b
	}
	return t
}

// tmpl is the order's version, SimOrderV2 when it names none.
func (o SimOrder) tmpl() *SimTemplate {
	if o.Template == nil {
		return SimOrderV2
	}
	return o.Template
}

// Terms is the data leaf: the terms in the template's order, big-endian.
func (o SimOrder) Terms() []byte {
	b := make([]byte, 0, 156)
	b = append(b, o.AssetA[:]...)
	b = append(b, o.AssetB[:]...)
	var n [8]byte
	for _, v := range []uint64{o.RateNum, o.RateDen, o.MinLot} {
		binary.BigEndian.PutUint64(n[:], v)
		b = append(b, n[:]...)
	}
	m := sha256.Sum256(o.MakerScript)
	b = append(b, m[:]...)
	binary.BigEndian.PutUint32(n[:4], o.Expiry)
	b = append(b, n[:4]...)
	b = append(b, o.RefundKey[:]...)
	return b
}

// Validate refuses terms no order should carry: a price that cannot be met or
// costs nothing, a minimum lot of zero (a fill of nothing would move the order),
// one asset on both sides, no maker script, a refund key no signature can
// satisfy. The program itself takes any terms; these only protect the maker
// and the taker from an order that cannot work.
func (o SimOrder) Validate() error {
	if o.RateNum < 1 || o.RateDen < 1 {
		return fmt.Errorf("rate %d/%d: numerator and denominator must be at least 1", o.RateNum, o.RateDen)
	}
	if o.MinLot < 1 {
		return fmt.Errorf("min_lot must be at least 1")
	}
	if o.AssetA == o.AssetB {
		return fmt.Errorf("the order sells and is paid in the same asset")
	}
	if len(o.MakerScript) == 0 || len(o.MakerScript) > 10000 {
		return fmt.Errorf("maker script of %d bytes", len(o.MakerScript))
	}
	if _, err := schnorr.ParsePubKey(o.RefundKey[:]); err != nil {
		return fmt.Errorf("refund key is not an x-only point: %w", err)
	}
	return nil
}

// Derive computes the order's output from its terms and its version's pinned
// root.
func (o SimOrder) Derive() (*SimTree, error) { return deriveSimTerms(o.Terms(), o.tmpl()) }

// deriveSimTerms computes the output of the order of version tmpl whose data
// leaf is terms.
func deriveSimTerms(terms []byte, tmpl *SimTemplate) (*SimTree, error) {
	if len(terms) != 156 {
		return nil, fmt.Errorf("terms of %d bytes, not 156", len(terms))
	}
	prog := taggedHash("TapLeaf/elements", []byte{simLeafVersion, 0x20}, tmpl.CMR[:])
	data := taggedHash("TapData", terms)
	lo, hi := prog, data
	if bytes.Compare(hi[:], lo[:]) < 0 {
		lo, hi = hi, lo
	}
	root := taggedHash("TapBranch/elements", lo[:], hi[:])
	key, parity, err := tweakOutputKey(NUMS, root)
	if err != nil {
		return nil, err
	}
	cb := make([]byte, 0, 65)
	cb = append(cb, simLeafVersion|parity)
	cb = append(cb, NUMS[:]...)
	cb = append(cb, data[:]...)
	return &SimTree{
		Terms: terms, ProgramLeafHash: prog, TermsLeafHash: data, MerkleRoot: root,
		OutputKey: key, Parity: parity, ScriptPubKey: append([]byte{op1, 0x20}, key[:]...), ControlBlock: cb,
	}, nil
}

// Address is the output's bech32m address under hrp ("tb" on the testnet).
func (t *SimTree) Address(hrp string) (string, error) {
	return address.ToBech32(&address.Bech32{Prefix: hrp, Version: 1, Program: t.OutputKey[:]})
}

// FillWitness is the order input's witness stack for a fill: [witness,
// program, cmr, control block], where witness is the terms followed by PATH,
// 0x40 for a partial fill and 0x00 for a whole one.
func (o SimOrder) FillWitness(partial bool) ([][]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	tm := o.tmpl()
	path, prog := byte(0x00), tm.programs["fill_whole"]
	if partial {
		path, prog = 0x40, tm.programs["fill"]
	}
	w := append(append([]byte{}, t.Terms...), path)
	return [][]byte{w, prog, tm.CMR[:], t.ControlBlock}, nil
}

// RefundWitness is the order input's witness stack for the refund, given the
// refund key's BIP340 signature over the transaction's Simplicity
// sig_all_hash (which this package does not compute). The transaction's lock
// time must be of Expiry's kind and at least Expiry, with a non-final
// sequence.
func (o SimOrder) RefundWitness(sig [64]byte) ([][]byte, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	tm := o.tmpl()
	prog := tm.programs["refund_height"]
	if o.Expiry >= 500_000_000 {
		prog = tm.programs["refund_time"]
	}
	// terms || 1 || sig, padded with zero bits.
	w := make([]byte, 0, len(t.Terms)+65)
	w = append(w, t.Terms...)
	carry := byte(0x80)
	for _, b := range sig {
		w = append(w, carry|b>>1)
		carry = b << 7
	}
	w = append(w, carry)
	return [][]byte{w, prog, tm.CMR[:], t.ControlBlock}, nil
}

// SimFillPath reads the path of a spend from its witness stack: whether it is
// a fill of either version, and whether a partial one. A stack that is not
// one of their programs spending (another root or program) is not a fill. A
// version 1 fill may carry an annex after its four items; the items read are
// the same.
func SimFillPath(stack [][]byte) (fill, partial bool) {
	if len(stack) < 4 || len(stack[0]) != 157 {
		return false, false
	}
	for _, t := range SimTemplates() {
		if !bytes.Equal(stack[2], t.CMR[:]) {
			continue
		}
		switch {
		case stack[0][156] == 0x40 && bytes.Equal(stack[1], t.programs["fill"]):
			return true, true
		case stack[0][156] == 0x00 && bytes.Equal(stack[1], t.programs["fill_whole"]):
			return true, false
		}
	}
	return false, false
}

// PlanFill is the recipe for taking filled atoms of AssetA from an order coin
// holding locked atoms, spent at input k: the credit at 2k, the rest (for a
// partial fill) at 2k+1, and the witness. It refuses what the program refuses
// of the order's own amounts: a fill or a rest below MinLot.
func (o SimOrder) PlanFill(locked, filled uint64, k uint32) (*FillPlan, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if filled == 0 || filled > locked {
		return nil, fmt.Errorf("filled %d out of range (locked %d)", filled, locked)
	}
	if filled < o.MinLot {
		return nil, fmt.Errorf("filled %d below min_lot %d", filled, o.MinLot)
	}
	remainder := locked - filled
	if remainder != 0 && remainder < o.MinLot {
		return nil, fmt.Errorf("remainder %d below min_lot %d", remainder, o.MinLot)
	}
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	w, err := o.FillWitness(remainder != 0)
	if err != nil {
		return nil, err
	}
	return &FillPlan{
		InputIndex: k, CreditIndex: 2 * k, RemainderIndex: 2*k + 1,
		Filled: filled, Remainder: remainder, RequiredB: CeilPrice(filled, o.RateNum, o.RateDen),
		Partial: remainder != 0, ControlBlock: t.ControlBlock, OrderSPK: t.ScriptPubKey, Stack: w,
	}, nil
}

// CheckFill judges outs as this order's program does for a fill at input k of
// coin, the order's own output. partial is the PATH the spend chose. It
// returns nil error exactly when the program accepts, and otherwise names the
// rule that fails:
//
//   - the coin holds AssetA explicitly;
//   - a partial fill: output 2k+1 holds AssetA explicitly, with no nonce, at
//     the order's own script, and at least MinLot;
//   - the fill (the coin less the rest) is at least MinLot and at least one
//     atom;
//   - output 2k pays MakerScript at least ceil(fill * RateNum / RateDen) of
//     AssetB, explicitly and with no nonce, compared on 128-bit products.
func (o SimOrder) CheckFill(coin Output, outs []Output, k uint32, partial bool) (*Checked, error) {
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	if !coin.AssetExplicit || !coin.ValueExplicit || coin.Asset != o.AssetA {
		return nil, fmt.Errorf("the order's coin does not hold the asset the order sells explicitly")
	}
	if !bytes.Equal(coin.Script, t.ScriptPubKey) {
		return nil, fmt.Errorf("the coin is not at the order's script")
	}
	ci, ri := 2*uint64(k), 2*uint64(k)+1
	c := &Checked{InputIndex: k, CreditIndex: uint32(ci), RemainderIndex: uint32(ri), Partial: partial}
	if partial {
		if ri >= uint64(len(outs)) {
			return nil, fmt.Errorf("no output %d (2k+1 for input %d) for the rest", ri, k)
		}
		r := outs[ri]
		switch {
		case !r.AssetExplicit || !r.ValueExplicit:
			return nil, fmt.Errorf("output %d (the rest) is confidential; the program reads it explicit", ri)
		case r.Asset != o.AssetA:
			return nil, fmt.Errorf("output %d (the rest) holds another asset than the order sells", ri)
		case !bytes.Equal(r.Script, t.ScriptPubKey):
			return nil, fmt.Errorf("output %d (the rest) does not pay the order's own script, the same program and terms", ri)
		case r.HasNonce:
			return nil, fmt.Errorf("output %d (the rest) carries a nonce", ri)
		case r.Value < o.MinLot:
			return nil, fmt.Errorf("output %d rests %d atoms again, below min_lot %d", ri, r.Value, o.MinLot)
		case r.Value > coin.Value:
			return nil, fmt.Errorf("output %d rests %d atoms, more than the coin's %d", ri, r.Value, coin.Value)
		}
		c.Remainder = r.Value
	}
	c.Filled = coin.Value - c.Remainder
	if c.Filled < o.MinLot || c.Filled == 0 {
		return nil, fmt.Errorf("the fill takes %d atoms, below min_lot %d", c.Filled, o.MinLot)
	}
	c.Required = CeilPrice(c.Filled, o.RateNum, o.RateDen)
	if ci >= uint64(len(outs)) {
		return nil, fmt.Errorf("no output %d (2k for input %d) to credit the maker", ci, k)
	}
	cr := outs[ci]
	switch {
	case !cr.AssetExplicit || !cr.ValueExplicit:
		return nil, fmt.Errorf("output %d (the credit) is confidential; the program reads it explicit", ci)
	case cr.Asset != o.AssetB:
		return nil, fmt.Errorf("output %d (the credit) holds another asset than the order is paid in", ci)
	case !bytes.Equal(cr.Script, o.MakerScript):
		return nil, fmt.Errorf("output %d (the credit) does not pay the maker's script", ci)
	case cr.HasNonce:
		return nil, fmt.Errorf("output %d (the credit) carries a nonce", ci)
	}
	// filled * num <= paid * den, at 128 bits.
	fh, fl := bits.Mul64(c.Filled, o.RateNum)
	ph, pl := bits.Mul64(cr.Value, o.RateDen)
	if fh > ph || (fh == ph && fl > pl) {
		return nil, fmt.Errorf("output %d credits the maker %d atoms, below the price %d", ci, cr.Value, c.Required)
	}
	c.Credited = cr.Value
	return c, nil
}
