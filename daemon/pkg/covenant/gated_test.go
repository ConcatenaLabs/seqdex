package covenant

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// The gated order matches openampd's (openamp, tools/gen_htlc_vectors.py,
// built with the node's test framework) byte for byte, with and without
// clawback. testdata/gated_vectors.json is copied from openamp's
// openampd/internal/elements/testdata/htlc_vectors.json.
func TestGatedOrderMatchesOpenampd(t *testing.T) {
	raw, err := os.ReadFile("testdata/gated_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gated_claw", "gated_noclaw"} {
		v := vs[name]
		s := func(k string) string { return v[k].(string) }
		b32 := func(k string) [32]byte { return mustHex32(s(k)) }
		fill, _ := hex.DecodeString(s("fill"))
		var issuer *[32]byte
		if s("issuer_x") != "" {
			x := b32("issuer_x")
			issuer = &x
		}
		tree, err := GatedTreeFromFill(b32("policy_x"), fill, issuer, uint32(v["expiry"].(float64)), b32("maker_program"), b32("asset"))
		if err != nil {
			t.Fatal(err)
		}
		for what, got := range map[string][]byte{"spk": tree.ScriptPubKey, "fill_script": tree.FillLeaf,
			"refund_script": tree.RefundLeaf, "fill_control": tree.FillControl} {
			if hex.EncodeToString(got) != s(what) {
				t.Errorf("%s %s = %x want %s", name, what, got, s(what))
			}
		}
		if issuer != nil && hex.EncodeToString(tree.ClawLeaf) != s("claw_script") {
			t.Errorf("%s claw_script = %x", name, tree.ClawLeaf)
		}
		if !GatedFillPath([][]byte{{1}, tree.FillLeaf, tree.FillControl}, b32("policy_x")) ||
			GatedFillPath([][]byte{tree.RefundLeaf, tree.FillControl}, b32("policy_x")) {
			t.Errorf("%s: GatedFillPath misreads the fill or the refund", name)
		}
	}
}

// A gated order from its terms wraps exactly BuildFillLeaf's covenant.
func TestGatedOrderTree(t *testing.T) {
	g := GatedOrder{AssetA: mustHex32(hex.EncodeToString(make([]byte, 31)) + "01"), AssetB: NUMS, RateNum: 1000, RateDen: 1,
		MinLot: 10, MakerProg: NUMS[:], PolicyX: NUMS, Expiry: 1_800_000_000, MakerEnclave: NUMS}
	tree, err := g.Tree()
	if err != nil {
		t.Fatal(err)
	}
	fill, _ := BuildFillLeaf(g.AssetA, g.AssetB, 1000, 1, 10, NUMS[:], 1)
	if hex.EncodeToString(tree.FillLeaf) != hex.EncodeToString(append(GatedFillPrefix(NUMS), fill...)) {
		t.Fatal("the gated fill is not the policy gate over BuildFillLeaf")
	}
	if tree.ClawLeaf != nil {
		t.Fatal("a claw leaf without an issuer key")
	}
	g.Expiry = 499_999_999
	if _, err := g.Tree(); err == nil {
		t.Fatal("a height expiry was accepted")
	}
}
