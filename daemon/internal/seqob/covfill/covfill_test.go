package covfill

// covfill_test.go — assembly unit tests against fixture covenants (the settler's
// fixture idiom) with a scripted elementsd RPC double: no node, byte-level
// assertions on the assembled FILL tx, and fail-closed proofs (a constraint
// mismatch or a testmempoolaccept rejection must never reach sendrawtransaction).

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// --- fixture covenant (the settler/plan test idiom) --------------------------

var (
	assetA = func() [32]byte { // internal order
		var b [32]byte
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()
	assetB = func() [32]byte {
		var b [32]byte
		for i := range b {
			b[i] = byte(32 + i)
		}
		return b
	}()
	makerProg = bytes.Repeat([]byte{0x11}, 32)
	makerX    = func() [32]byte {
		var b [32]byte
		copy(b[:], bytes.Repeat([]byte{0x22}, 32))
		return b
	}()

	feeDisplay = strings.Repeat("ee", 32)
	covTxid    = strings.Repeat("ab", 32)
)

// mkOrder is the fixture: sell asset A at 3 B per A, min_lot 5.
func mkOrder(minLot uint64) covenant.Order {
	return covenant.DefaultOrder(assetA, assetB, 3, 1, minLot, makerProg, 1000, makerX)
}

func displayA() string { return displayHex(assetA) }
func displayB() string { return displayHex(assetB) }

// --- scripted elementsd double ----------------------------------------------

type utxoRow struct {
	Txid          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Amount        float64 `json:"amount"`
	Asset         string  `json:"asset"`
	Spendable     bool    `json:"spendable"`
	Confirmations int     `json:"confirmations"`
}

type scriptedNode struct {
	t *testing.T

	txout   interface{}            // gettxout response (nil = spent/absent)
	txouts  map[string]interface{} // per "txid:vout" gettxout responses, when set
	unspent []utxoRow

	mempoolAllowed bool
	rejectReason   string

	addrN       int
	locked      int // lockunspent(false) calls
	unlocked    int // lockunspent(true) calls
	signCalls   []string
	mempoolHex  []string
	sent        []string
	failGetAddr bool
}

func fundRow(asset string, atoms uint64, n int) utxoRow {
	return utxoRow{
		Txid: strings.Repeat(fmt.Sprintf("%02x", 0xc0+n), 32), Vout: uint32(n),
		Amount: float64(atoms) / 1e8, Asset: asset, Spendable: true, Confirmations: 3,
	}
}

// covTxout builds the canned gettxout result for a live covenant UTXO.
func covTxout(t *testing.T, o covenant.Order, locked uint64) map[string]interface{} {
	t.Helper()
	tap, err := o.Derive()
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return map[string]interface{}{
		"confirmations": 2,
		"value":         float64(locked) / 1e8,
		"asset":         displayHex(o.AssetA),
		"scriptPubKey":  map[string]interface{}{"hex": hex.EncodeToString(tap.ScriptPubKey)},
	}
}

func (n *scriptedNode) Call(out interface{}, method string, params ...interface{}) error {
	respond := func(v interface{}) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(b, out)
	}
	switch method {
	case "gettxout":
		if n.txouts != nil {
			return respond(n.txouts[fmt.Sprintf("%v:%v", params[0], params[1])])
		}
		return respond(n.txout)
	case "listunspent":
		return respond(n.unspent)
	case "getnewaddress":
		n.addrN++
		return respond(fmt.Sprintf("addr%d", n.addrN))
	case "getaddressinfo":
		if n.failGetAddr {
			return fmt.Errorf("scripted getaddressinfo failure")
		}
		addr, _ := params[0].(string)
		// A deterministic p2wpkh spk per address; no confidential form.
		spk := fmt.Sprintf("0014%040s", strings.TrimPrefix(addr, "addr"))
		return respond(map[string]interface{}{"scriptPubKey": strings.ReplaceAll(spk, " ", "0")})
	case "signrawtransactionwithwallet":
		raw, _ := params[0].(string)
		n.signCalls = append(n.signCalls, raw)
		// The double signs nothing (structural tests): echo the hex, complete=false
		// exactly as elementsd reports when the covenant input is not the wallet's.
		return respond(map[string]interface{}{"hex": raw, "complete": false})
	case "testmempoolaccept":
		if hexes, ok := params[0].([]string); ok && len(hexes) > 0 {
			n.mempoolHex = append(n.mempoolHex, hexes[0])
		}
		return respond([]map[string]interface{}{{
			"allowed": n.mempoolAllowed, "reject-reason": n.rejectReason,
		}})
	case "sendrawtransaction":
		raw, _ := params[0].(string)
		n.sent = append(n.sent, raw)
		return respond(strings.Repeat("f1", 32))
	case "lockunspent":
		unlock, _ := params[0].(bool)
		if unlock {
			n.unlocked++
		} else {
			n.locked++
		}
		return respond(true)
	default:
		n.t.Fatalf("unexpected RPC %s", method)
		return nil
	}
}

func okNode(t *testing.T, o covenant.Order, locked uint64, utxos ...utxoRow) *scriptedNode {
	return &scriptedNode{t: t, txout: covTxout(t, o, locked), unspent: utxos, mempoolAllowed: true}
}

func params(o covenant.Order, node Node, take, fee uint64) Params {
	return Params{
		Order: o, Txid: covTxid, Vout: 0,
		TakeAtoms: take, FeeAtoms: fee, FeeAssetDisplay: feeDisplay,
		Node: node,
	}
}

// --- output assertions (the settler's assertOutput idiom) --------------------

func commit(t *testing.T, display string) []byte {
	t.Helper()
	c, err := elementsutil.AssetHashToBytes(display)
	if err != nil {
		t.Fatalf("asset %s: %v", display, err)
	}
	return c
}

func assertOutput(t *testing.T, out *transaction.TxOutput, wantAssetDisplay string, wantValue uint64, wantSPK []byte, label string) {
	t.Helper()
	if !bytes.Equal(out.Asset, commit(t, wantAssetDisplay)) {
		t.Errorf("%s: asset = %x, want %s", label, out.Asset, wantAssetDisplay)
	}
	v, err := elementsutil.ValueFromBytes(out.Value)
	if err != nil {
		t.Fatalf("%s: value decode: %v", label, err)
	}
	if v != wantValue {
		t.Errorf("%s: value = %d, want %d", label, v, wantValue)
	}
	if wantSPK != nil && !bytes.Equal(out.Script, wantSPK) {
		t.Errorf("%s: spk = %x, want %x", label, out.Script, wantSPK)
	}
}

func parseTx(t *testing.T, rawHex string) *transaction.Transaction {
	t.Helper()
	tx, err := transaction.NewTxFromHex(rawHex)
	if err != nil {
		t.Fatalf("parse tx: %v", err)
	}
	return tx
}

var creditSPK = append([]byte{0x51, 0x20}, bytes.Repeat([]byte{0x11}, 32)...)

// --- layout tests -------------------------------------------------------------

// TestFullFillLayout: whole-covenant fill with change on both funded assets.
// Layout must be [credit, B-change(gap), receipt(A), fee-change, fee] — the
// wasm assembler's FULL ordering with the B-change as the non-A gap at slot 1.
func TestFullFillLayout(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 5300, 1), // credit 300 -> change 5000
		fundRow(feeDisplay, 2500, 2), // fee 500 -> change 2000
	)
	res, err := FillCovenant(params(o, node, 0, 500))
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if res.Filled != 100 || res.Remainder != 0 || res.Partial || res.PaidB != 300 {
		t.Fatalf("result = %+v, want full fill of 100 paying 300", res)
	}
	tx := parseTx(t, res.RawHex)
	if len(tx.Inputs) != 3 {
		t.Fatalf("inputs = %d, want 3 (covenant + 2 funding)", len(tx.Inputs))
	}
	// Covenant input 0 carries the introspection-only witness, nothing else does.
	plan, _ := o.PlanFill(100, 100, 0)
	if len(tx.Inputs[0].Witness) != 2 ||
		!bytes.Equal(tx.Inputs[0].Witness[0], plan.FillLeaf) ||
		!bytes.Equal(tx.Inputs[0].Witness[1], plan.ControlBlock) {
		t.Fatal("input 0 witness is not [fill_leaf, control_block]")
	}
	if len(tx.Outputs) != 5 {
		t.Fatalf("outputs = %d, want 5", len(tx.Outputs))
	}
	assertOutput(t, tx.Outputs[0], displayB(), 300, creditSPK, "credit")
	assertOutput(t, tx.Outputs[1], displayB(), 5000, nil, "B-change gap")
	assertOutput(t, tx.Outputs[2], displayA(), 100, nil, "receipt")
	assertOutput(t, tx.Outputs[3], feeDisplay, 2000, nil, "fee change")
	assertOutput(t, tx.Outputs[4], feeDisplay, 500, []byte{}, "fee")
	if len(node.sent) != 1 || node.sent[0] != res.RawHex {
		t.Fatal("broadcast hex mismatch")
	}
}

// TestPartialFillLayout: a slice leaves the remainder re-rested to the SAME
// covenant spk at slot 1 (the leaf's self-replication slot).
func TestPartialFillLayout(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 5120, 1), // credit 120 -> change 5000
		fundRow(feeDisplay, 2500, 2),
	)
	res, err := FillCovenant(params(o, node, 40, 500))
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if res.Filled != 40 || res.Remainder != 60 || !res.Partial || res.PaidB != 120 {
		t.Fatalf("result = %+v, want partial 40/100 paying 120", res)
	}
	tx := parseTx(t, res.RawHex)
	tap, _ := o.Derive()
	if len(tx.Outputs) != 6 {
		t.Fatalf("outputs = %d, want 6", len(tx.Outputs))
	}
	assertOutput(t, tx.Outputs[0], displayB(), 120, creditSPK, "credit")
	assertOutput(t, tx.Outputs[1], displayA(), 60, tap.ScriptPubKey, "remainder covenant")
	assertOutput(t, tx.Outputs[2], displayA(), 40, nil, "receipt")
	assertOutput(t, tx.Outputs[3], displayB(), 5000, nil, "B change")
	assertOutput(t, tx.Outputs[4], feeDisplay, 2000, nil, "fee change")
	assertOutput(t, tx.Outputs[5], feeDisplay, 500, []byte{}, "fee")
}

// TestFeeOutputAsGap: exact funding on both assets leaves no change, so the fee
// output itself takes the full-fill gap slot (valid because fee asset != A).
func TestFeeOutputAsGap(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 300, 1),
		fundRow(feeDisplay, 500, 2),
	)
	res, err := FillCovenant(params(o, node, 0, 500))
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	tx := parseTx(t, res.RawHex)
	if len(tx.Outputs) != 3 {
		t.Fatalf("outputs = %d, want 3", len(tx.Outputs))
	}
	assertOutput(t, tx.Outputs[0], displayB(), 300, creditSPK, "credit")
	assertOutput(t, tx.Outputs[1], feeDisplay, 500, []byte{}, "fee as gap")
	assertOutput(t, tx.Outputs[2], displayA(), 100, nil, "receipt")
}

// TestDustFoldIntoCredit: sub-dust B-change is folded into the maker credit
// (the leaf enforces only a MINIMUM), never emitted as a dust output.
func TestDustFoldIntoCredit(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 999, 1), // change 699 < dust 1000 -> credit 999
		fundRow(feeDisplay, 500, 2),
	)
	res, err := FillCovenant(params(o, node, 0, 500))
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if res.PaidB != 999 {
		t.Fatalf("PaidB = %d, want 999 (dust folded into the credit)", res.PaidB)
	}
	tx := parseTx(t, res.RawHex)
	assertOutput(t, tx.Outputs[0], displayB(), 999, creditSPK, "overpaid credit")
	// No B-change output exists; the fee output is the gap.
	assertOutput(t, tx.Outputs[1], feeDisplay, 500, []byte{}, "fee as gap")
}

// TestDustShrinkTake: a take leaving a sub-min_lot remainder shrinks to
// locked-min_lot (the crosser/web dust rule), never builds an invalid plan.
func TestDustShrinkTake(t *testing.T) {
	o := mkOrder(8)
	node := okNode(t, o, 100,
		fundRow(displayB(), 5000, 1), // credit ceil(92*3/1)=276
		fundRow(feeDisplay, 2500, 2),
	)
	res, err := FillCovenant(params(o, node, 95, 500)) // remainder 5 < min_lot 8
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if res.Filled != 92 || res.Remainder != 8 || res.PaidB != 276 {
		t.Fatalf("result = %+v, want shrunk fill 92 (remainder 8) paying 276", res)
	}
	tx := parseTx(t, res.RawHex)
	tap, _ := o.Derive()
	assertOutput(t, tx.Outputs[1], displayA(), 8, tap.ScriptPubKey, "min_lot remainder")
}

func TestShrinkTakeUnit(t *testing.T) {
	for _, c := range []struct{ locked, take, minLot, want uint64 }{
		{100, 0, 5, 100},   // 0 = whole
		{100, 100, 5, 100}, // exact whole
		{100, 40, 5, 40},   // clean partial
		{100, 97, 5, 95},   // remainder 3 < 5 -> shrink to locked-minLot
		{100, 95, 5, 95},   // remainder == minLot: legal, untouched
		{100, 120, 5, 100}, // over-ask clamps to whole
		{100, 96, 0, 96},   // no min_lot: nothing to shrink
	} {
		if got := ShrinkTake(c.locked, c.take, c.minLot); got != c.want {
			t.Errorf("ShrinkTake(%d,%d,%d) = %d, want %d", c.locked, c.take, c.minLot, got, c.want)
		}
	}
}

// TestWitnessInjectedBeforeMempoolGate: the hex handed to testmempoolaccept must
// already carry the covenant witness — the gate must judge the REAL spend.
func TestWitnessInjectedBeforeMempoolGate(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 300, 1),
		fundRow(feeDisplay, 500, 2),
	)
	if _, err := FillCovenant(params(o, node, 0, 500)); err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if len(node.mempoolHex) != 1 {
		t.Fatalf("testmempoolaccept called %d times, want 1", len(node.mempoolHex))
	}
	gated := parseTx(t, node.mempoolHex[0])
	if len(gated.Inputs[0].Witness) != 2 {
		t.Fatal("mempool-gated hex is missing the covenant witness")
	}
}

// --- fail-closed refusals -----------------------------------------------------

// refuse asserts FillCovenant fails with a message containing want and that
// NOTHING was broadcast.
func refuse(t *testing.T, p Params, node *scriptedNode, want string) {
	t.Helper()
	_, err := FillCovenant(p)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
	if len(node.sent) != 0 {
		t.Fatalf("broadcast happened despite the refusal (%d sends)", len(node.sent))
	}
}

func TestRefusesSpentCovenant(t *testing.T) {
	o := mkOrder(5)
	node := &scriptedNode{t: t, txout: nil, mempoolAllowed: true}
	refuse(t, params(o, node, 0, 500), node, "not an unspent utxo")
}

func TestRefusesWrongOnchainSPK(t *testing.T) {
	o := mkOrder(5)
	tricked := covTxout(t, o, 100)
	tricked["scriptPubKey"] = map[string]interface{}{"hex": "5120" + strings.Repeat("77", 32)}
	node := &scriptedNode{t: t, txout: tricked, mempoolAllowed: true}
	refuse(t, params(o, node, 0, 500), node, "trustless verify")
}

func TestRefusesWrongAsset(t *testing.T) {
	o := mkOrder(5)
	tricked := covTxout(t, o, 100)
	tricked["asset"] = strings.Repeat("dd", 32)
	node := &scriptedNode{t: t, txout: tricked, mempoolAllowed: true}
	refuse(t, params(o, node, 0, 500), node, "holds asset")
}

func TestRefusesNonNUMSInternalKey(t *testing.T) {
	o := mkOrder(5)
	// Build the on-chain view from the honest NUMS order, then advertise a
	// non-NUMS internal key: VerifyAgainstSPK refuses BEFORE any spk comparison
	// (a non-NUMS key is a hidden maker key-path cancel/rug).
	node := &scriptedNode{t: t, txout: covTxout(t, o, 100), mempoolAllowed: true}
	o.InternalKey[5] ^= 0xff
	refuse(t, params(o, node, 0, 500), node, "maker-cancellable")
}

func TestRefusesFeeAssetEqualsSoldAsset(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100, fundRow(displayB(), 5000, 1))
	p := params(o, node, 0, 500)
	p.FeeAssetDisplay = displayA()
	refuse(t, p, node, "sold asset A")
}

func TestRefusesBelowMinLot(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 5000, 1),
		fundRow(feeDisplay, 2500, 2),
	)
	refuse(t, params(o, node, 3, 500), node, "below min_lot")
}

func TestRefusesInsufficientFunding(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 100, 1), // needs 300
		fundRow(feeDisplay, 2500, 2),
	)
	refuse(t, params(o, node, 0, 500), node, "need 300")
}

// TestRefusesAssetAFunding: asset-A coins in the wallet are never candidates —
// the sold asset comes from the covenant input only (the wasm assembler's rule).
func TestRefusesAssetAFundingOnly(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayA(), 10_000, 1), // asset A: must be ignored, not spent
		fundRow(feeDisplay, 2500, 2),
	)
	refuse(t, params(o, node, 0, 500), node, "need 300")
}

// TestMempoolRejectionBlocksBroadcast: the testmempoolaccept gate is
// authoritative — a rejected spend is NEVER sent.
func TestMempoolRejectionBlocksBroadcast(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100,
		fundRow(displayB(), 300, 1),
		fundRow(feeDisplay, 500, 2),
	)
	node.mempoolAllowed = false
	node.rejectReason = "non-mandatory-script-verify-flag"
	refuse(t, params(o, node, 0, 500), node, "non-mandatory-script-verify-flag")
	if len(node.mempoolHex) != 1 {
		t.Fatal("testmempoolaccept was not consulted")
	}
}
