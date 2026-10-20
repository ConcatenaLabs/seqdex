package covenant

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

var updateTermsVectors = flag.Bool("update-terms-vectors", false, "rewrite testdata/terms_bound_vectors.json")

// The vector pins, across Go and the web wallet's covenant.js, which tapscript
// terms the FILL leaf reads as the numbers they are: rate_num, rate_den and
// min_lot below 2^63 build a leaf (its bytes given), any of them at 2^63 or
// more is refused, named, because the leaf's signed 64-bit opcodes would read
// it as negative and fill the order for any credit. Amounts are decimal
// strings: JavaScript's numbers lose them above 2^53.
const termsVectorFile = "testdata/terms_bound_vectors.json"

type termsCase struct {
	Name    string `json:"name"`
	RateNum string `json:"rate_num"`
	RateDen string `json:"rate_den"`
	MinLot  string `json:"min_lot"`
	Refused string `json:"refused,omitempty"`   // the term refused, "" when the leaf builds
	Leaf    string `json:"fill_leaf,omitempty"` // the FILL leaf, when it builds
}

type termsVectors struct {
	About     string      `json:"about"`
	AssetA    string      `json:"asset_a"`
	AssetB    string      `json:"asset_b"`
	MakerProg string      `json:"maker_prog"`
	Cases     []termsCase `json:"cases"`
}

func termsFixture() ([32]byte, [32]byte, []byte) {
	var a, b [32]byte
	copy(a[:], bytes.Repeat([]byte{0xa1}, 32))
	copy(b[:], bytes.Repeat([]byte{0xb2}, 32))
	return a, b, bytes.Repeat([]byte{0xc3}, 32)
}

func TestTapscriptTermsBelow2to63(t *testing.T) {
	a, b, prog := termsFixture()
	const max, two63, top = uint64(math.MaxInt64), uint64(1) << 63, uint64(math.MaxUint64)
	if *updateTermsVectors {
		u := func(n uint64) string { return strconv.FormatUint(n, 10) }
		v := termsVectors{
			About: "Tapscript FILL leaf terms at the 2^63 edge. rate_num, rate_den and min_lot below 2^63 build the leaf " +
				"(fill_leaf, by seqdex pkg/covenant BuildFillLeaf over asset_a, asset_b, maker_prog); a term at 2^63 or more " +
				"is refused (refused names it): OP_MUL64, OP_DIV64 and OP_GREATERTHANOREQUAL64 read it as negative, and the " +
				"order would fill for any credit. Go (BuildFillLeaf, CheckTerms) and the web wallet's covenant.js agree on every case.",
			AssetA: hex.EncodeToString(a[:]), AssetB: hex.EncodeToString(b[:]), MakerProg: hex.EncodeToString(prog),
		}
		for _, c := range []struct {
			name          string
			num, den, lot uint64
			refused       string
		}{
			{"every term at 2^63-1", max, max, max, ""},
			{"small terms", 3, 2, 10_000, ""},
			{"rate_num at 2^63", two63, 1, 1, "rate_num"},
			{"rate_num at 2^64-1", top, 1, 1, "rate_num"},
			{"rate_den at 2^63+5", 1, two63 + 5, 1, "rate_den"},
			{"rate_den at 2^64-1", 6, top, 1, "rate_den"},
			{"min_lot at 2^63", 3, 2, two63, "min_lot"},
			{"min_lot at 2^63+5", 6, 16, two63 + 5, "min_lot"},
		} {
			tc := termsCase{Name: c.name, RateNum: u(c.num), RateDen: u(c.den), MinLot: u(c.lot), Refused: c.refused}
			if c.refused == "" {
				leaf, err := BuildFillLeaf(a, b, c.num, c.den, c.lot, prog, 1)
				if err != nil {
					t.Fatal(err)
				}
				tc.Leaf = hex.EncodeToString(leaf)
			}
			v.Cases = append(v.Cases, tc)
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(termsVectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(termsVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var v termsVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	refusedSome := map[string]bool{}
	for _, c := range v.Cases {
		num, _ := strconv.ParseUint(c.RateNum, 10, 64)
		den, _ := strconv.ParseUint(c.RateDen, 10, 64)
		lot, _ := strconv.ParseUint(c.MinLot, 10, 64)
		leaf, err := BuildFillLeaf(a, b, num, den, lot, prog, 1)
		terr := Order{RateNum: num, RateDen: den, MinLot: lot}.CheckTerms()
		if c.Refused == "" {
			if err != nil || terr != nil || hex.EncodeToString(leaf) != c.Leaf {
				t.Fatalf("%s: %v %v, leaf %x", c.Name, err, terr, leaf)
			}
			continue
		}
		refusedSome[c.Refused] = true
		for _, e := range []error{err, terr} {
			if e == nil || !strings.HasPrefix(e.Error(), c.Refused+" ") || !strings.Contains(e.Error(), "2^63 or more") ||
				!strings.Contains(e.Error(), "Simplicity order (seqdex/order") {
				t.Fatalf("%s: %v", c.Name, e)
			}
		}
		// Every path a maker or a taker takes refuses it too.
		o := DefaultOrder(a, b, num, den, lot, prog, 600, a)
		if _, err := o.Derive(); err == nil {
			t.Fatalf("%s: Derive built it", c.Name)
		}
		if _, err := o.PlanFill(1_000_000, 1_000_000, 0); err == nil {
			t.Fatalf("%s: PlanFill planned it", c.Name)
		}
	}
	if len(refusedSome) != 3 {
		t.Fatalf("the vector refuses %v; it must name each term", refusedSome)
	}
}
