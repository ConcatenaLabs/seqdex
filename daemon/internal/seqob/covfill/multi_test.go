package covfill

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A second fixture order, paid in a third asset C: sells A at 2 C per A.
var assetC = func() [32]byte {
	var b [32]byte
	for i := range b {
		b[i] = byte(64 + i)
	}
	return b
}()

var covTxid2 = strings.Repeat("cd", 32)

func mkOrderC(minLot uint64) covenant.Order {
	prog := bytes.Repeat([]byte{0x33}, 32)
	return covenant.DefaultOrder(assetA, assetC, 2, 1, minLot, prog, 1700000000, makerX)
}

func twoNode(t *testing.T, o1 covenant.Order, l1 uint64, o2 covenant.Order, l2 uint64, utxos ...utxoRow) *scriptedNode {
	return &scriptedNode{t: t, unspent: utxos, mempoolAllowed: true, txouts: map[string]interface{}{
		covTxid + ":0":  covTxout(t, o1, l1),
		covTxid2 + ":3": covTxout(t, o2, l2),
	}}
}

// Two orders in one fill: order 1 in part at input 0 (credit 0, rest 1), order
// 2 whole at input 1 (credit 2, gap 3), then the receipt, the change and the
// fee. Each order's leaf is run over the result.
func TestTwoOrdersOneTransaction(t *testing.T) {
	o1, o2 := mkOrder(5), mkOrderC(5)
	node := twoNode(t, o1, 100, o2, 50,
		fundRow(displayB(), 5120, 1),         // credit 120 -> change 5000
		fundRow(displayHex(assetC), 2100, 2), // credit 100 -> change 2000
		fundRow(feeDisplay, 2500, 3),         // fee 500 -> change 2000
	)
	res, err := FillCovenant(Params{
		Takes: []Take{
			{Order: o1, Txid: covTxid, Vout: 0, TakeAtoms: 40},
			{Order: o2, Txid: covTxid2, Vout: 3},
		},
		FeeAtoms: 500, FeeAssetDisplay: feeDisplay, Node: node,
	})
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	if len(res.Fills) != 2 || res.Fills[1].InputIndex != 1 || res.Fills[1].CreditIndex != 2 || res.Fills[1].RemainderIndex != 3 {
		t.Fatalf("fills = %+v", res.Fills)
	}
	tx := parseTx(t, res.RawHex)
	if len(tx.Inputs) != 5 {
		t.Fatalf("inputs = %d, want 5", len(tx.Inputs))
	}
	tap1, _ := o1.Derive()
	p2, _ := o2.PlanFill(50, 50, 1)
	if !bytes.Equal(tx.Inputs[1].Witness[0], p2.FillLeaf) || !bytes.Equal(tx.Inputs[1].Witness[1], p2.ControlBlock) {
		t.Fatal("input 1 does not carry order 2's FILL witness")
	}
	credit2 := append([]byte{0x51, 0x20}, bytes.Repeat([]byte{0x33}, 32)...)
	assertOutput(t, tx.Outputs[0], displayB(), 120, creditSPK, "order 1 credit")
	assertOutput(t, tx.Outputs[1], displayA(), 60, tap1.ScriptPubKey, "order 1 rest")
	assertOutput(t, tx.Outputs[2], displayHex(assetC), 100, credit2, "order 2 credit")
	assertOutput(t, tx.Outputs[3], displayHex(assetC), 2000, nil, "order 2 gap: its own credit asset's change")
	assertOutput(t, tx.Outputs[4], displayA(), 90, nil, "receipt of both")
	assertOutput(t, tx.Outputs[5], displayB(), 5000, nil, "B change")
	assertOutput(t, tx.Outputs[6], feeDisplay, 2000, nil, "fee change")
	assertOutput(t, tx.Outputs[7], feeDisplay, 500, []byte{}, "fee")
	outs := covenant.OutputsOf(tx)
	for k, c := range []struct {
		o      covenant.Order
		locked uint64
	}{{o1, 100}, {o2, 50}} {
		if _, err := c.o.CheckFill(outs, uint32(k), c.locked); err != nil {
			t.Fatalf("order %d's leaf refuses the fill: %v", k, err)
		}
	}
}

// Two whole fills of orders that sell the same asset, funded exactly: the fee
// stands at order 1's 2k+1, and order 2's needs a gap coin of its credit
// asset, since the only output left is the receipt of the asset it sells.
func TestGapCoinWhenNothingElseFits(t *testing.T) {
	o1, o2 := mkOrder(5), mkOrderC(5)
	node := twoNode(t, o1, 100, o2, 50,
		fundRow(displayB(), 300, 1),
		fundRow(displayHex(assetC), 100, 2),
		fundRow(displayHex(assetC), 1000, 4), // what the gap coin is funded from
		fundRow(feeDisplay, 500, 3),
	)
	res, err := FillCovenant(Params{
		Takes:    []Take{{Order: o1, Txid: covTxid, Vout: 0}, {Order: o2, Txid: covTxid2, Vout: 3}},
		FeeAtoms: 500, FeeAssetDisplay: feeDisplay, Node: node,
	})
	if err != nil {
		t.Fatalf("FillCovenant: %v", err)
	}
	tx := parseTx(t, res.RawHex)
	assertOutput(t, tx.Outputs[0], displayB(), 300, creditSPK, "order 1 credit")
	assertOutput(t, tx.Outputs[1], feeDisplay, 500, []byte{}, "the fee at order 1's gap")
	assertOutput(t, tx.Outputs[3], displayHex(assetC), DefaultDustFold, nil, "the gap coin at order 2's 2k+1")
	assertOutput(t, tx.Outputs[4], displayA(), 150, nil, "receipt")
	if node.unlocked != 1 {
		t.Fatalf("the first selection's coins were released %d times, want once", node.unlocked)
	}
}

func TestRefusesATakePaidInAnotherTakesSoldAsset(t *testing.T) {
	o1 := mkOrder(5)
	o2 := covenant.DefaultOrder(assetC, assetA, 1, 1, 5, bytes.Repeat([]byte{0x33}, 32), 1000, makerX)
	node := twoNode(t, o1, 100, o2, 50)
	_, err := FillCovenant(Params{
		Takes:    []Take{{Order: o1, Txid: covTxid, Vout: 0}, {Order: o2, Txid: covTxid2, Vout: 3}},
		FeeAtoms: 500, FeeAssetDisplay: feeDisplay, Node: node,
	})
	if err == nil || !strings.Contains(err.Error(), "the taker never funds a sold asset") {
		t.Fatalf("got %v", err)
	}
}
