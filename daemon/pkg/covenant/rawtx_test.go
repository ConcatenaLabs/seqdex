package covenant

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vulpemventures/go-elements/transaction"
)

// An Arca operator's fill round, as the node serialized it: two orders at
// k = 0 (whole) and k = 1 (in part), and an input issuing the batch's token.
func TestReadOutputsReadsAnArcaFillRound(t *testing.T) {
	raw, err := os.ReadFile("testdata/arca_fill_round.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Hex    string `json:"hex"`
		Orders []struct {
			K      uint32    `json:"k"`
			Terms  SpecTerms `json:"terms"`
			Locked uint64    `json:"locked"`
			Filled uint64    `json:"filled"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, _ := hex.DecodeString(v.Hex)
	// go-elements cannot read the issuance's denomination.
	if _, err := transaction.NewTxFromHex(v.Hex); err == nil {
		t.Log("go-elements read the round (it no longer trips on the denomination)")
	} else {
		t.Logf("go-elements: %v", err)
	}
	outs, err := ReadOutputs(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 11 {
		t.Fatalf("%d outputs, want 11", len(outs))
	}
	if outs[10].Value != 264 || len(outs[10].Script) != 0 {
		t.Fatalf("the fee output: %+v", outs[10])
	}
	want := []struct{ filled, remainder, credited uint64 }{{400000, 0, 1000000}, {499500, 100500, 999000}}
	for i, o := range v.Orders {
		order, err := o.Terms.Order()
		if err != nil {
			t.Fatal(err)
		}
		c, err := order.CheckFill(outs, o.K, o.Locked)
		if err != nil {
			t.Fatalf("order at k=%d: %v", o.K, err)
		}
		if c.Filled != o.Filled || c.Filled != want[i].filled || c.Remainder != want[i].remainder || c.Credited != want[i].credited ||
			c.CreditIndex != 2*o.K || c.RemainderIndex != 2*o.K+1 {
			t.Fatalf("order at k=%d: %+v", o.K, c)
		}
	}
	// A round cut short before its lock time is refused, not misread.
	for _, cut := range []int{3, 90, 200, 600} {
		if _, err := ReadOutputs(b[:cut]); err == nil || !strings.Contains(err.Error(), "truncated") && !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "prefix") {
			t.Fatalf("cut at %d: %v", cut, err)
		}
	}
}

// ReadOutputs reads what OutputsOf reads for a transaction go-elements builds.
func TestReadOutputsAgreesWithGoElements(t *testing.T) {
	tx, _, err := BuildFillTx(twoTakes(t))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := tx.Serialize()
	got, err := ReadOutputs(b)
	if err != nil {
		t.Fatal(err)
	}
	want := OutputsOf(tx)
	if len(got) != len(want) {
		t.Fatalf("%d outputs, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Asset != want[i].Asset || got[i].Value != want[i].Value || hex.EncodeToString(got[i].Script) != hex.EncodeToString(want[i].Script) ||
			got[i].AssetExplicit != want[i].AssetExplicit || got[i].ValueExplicit != want[i].ValueExplicit {
			t.Fatalf("output %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
