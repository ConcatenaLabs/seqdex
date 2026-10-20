package covenant

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

// One transaction fills a tapscript order at input 0 and a Simplicity order at
// input 1, laid out by BuildFillTx and judged by each order's own leaf; a
// whole Simplicity fill may stand the taker's own asset A at its 2k+1, which the
// tapscript leaf would read as a remainder.
func TestBuildFillTxTakesBothLeaves(t *testing.T) {
	sim := testSimOrder(t)
	tap := DefaultOrder(sim.AssetA, sim.AssetB, 9, 10, 1_000, bytes.Repeat([]byte{4}, 32), 600, sha256.Sum256([]byte("x")))
	tap.MakerX = sim.RefundKey
	other := sha256.Sum256([]byte("fee asset"))
	taker := []byte{0x00, 0x14, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	txid := strings.Repeat("ab", 32)
	const simLocked = 20_000_000_000_000
	for _, partial := range []bool{true, false} {
		simFilled := uint64(simLocked)
		if partial {
			simFilled = 6_000_000_000_001
		}
		f := FillTx{
			Takes: []Take{
				{Order: tap, Txid: txid, Vout: 0, Locked: 5_000_000, Filled: 2_000_000},
				{Sim: &sim, Txid: txid, Vout: 1, Locked: simLocked, Filled: simFilled},
			},
			Gaps:   []*Output{nil, nil},
			Inputs: []Outpoint{{Txid: txid, Vout: 2}},
			Tail:   []Output{Explicit(sim.AssetA, 2_000_000+simFilled, taker), Explicit(other, 1000, nil)},
		}
		if !partial {
			// The taker's asset A at the Simplicity order's 2k+1.
			g := Explicit(sim.AssetA, simLocked, taker)
			f.Gaps = []*Output{nil, &g}
			f.Tail = []Output{Explicit(sim.AssetA, 2_000_000, taker), Explicit(other, 1000, nil)}
		}
		tx, plans, err := BuildFillTx(f)
		if err != nil {
			t.Fatalf("partial=%v: %v", partial, err)
		}
		if len(tx.Inputs) != 3 || len(plans) != 2 || plans[1].CreditIndex != 2 || plans[1].RemainderIndex != 3 {
			t.Fatalf("layout %+v", plans)
		}
		if len(tx.Inputs[0].Witness) != 2 || len(tx.Inputs[1].Witness) != 4 {
			t.Fatalf("witnesses %d and %d items", len(tx.Inputs[0].Witness), len(tx.Inputs[1].Witness))
		}
		if fill, p := SimFillPath(tx.Inputs[1].Witness); !fill || p != partial {
			t.Fatalf("the Simplicity input carries path %v %v", fill, p)
		}
		raw, _ := tx.Serialize()
		outs, err := ReadOutputs(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sim.Judge(outs, 1, simLocked, partial); err != nil {
			t.Fatalf("partial=%v: %v", partial, err)
		}
		if _, err := tap.Judge(outs, 0, 5_000_000, true); err != nil {
			t.Fatalf("tapscript: %v", err)
		}
	}
	// The tapscript order still refuses an asset-A gap.
	g := Explicit(sim.AssetA, 5_000_000, taker)
	_, _, err := BuildFillTx(FillTx{Takes: []Take{{Order: tap, Txid: txid, Locked: 5_000_000, Filled: 5_000_000}},
		Gaps: []*Output{&g}, Tail: []Output{Explicit(other, 1000, nil)}})
	if err == nil || !strings.Contains(err.Error(), "reads as a remainder") {
		t.Fatalf("an asset-A gap for the tapscript order: %v", err)
	}
}
