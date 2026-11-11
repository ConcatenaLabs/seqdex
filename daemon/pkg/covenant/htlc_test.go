package covenant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// The swap HTLC and fee pot match openampd's byte for byte (openamp,
// tools/gen_htlc_vectors.py, built with the node's test framework).
// testdata/htlc_vectors.json is a copy of openamp's
// openampd/internal/elements/testdata/htlc_vectors.json.
func TestSwapHTLCMatchesOpenampd(t *testing.T) {
	raw, err := os.ReadFile("testdata/htlc_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	check := func(name string, tree *TapTree, v map[string]interface{}, leaves ...string) {
		t.Helper()
		if got := hex.EncodeToString(tree.ScriptPubKey); got != v["spk"].(string) {
			t.Errorf("%s spk = %s want %s", name, got, v["spk"])
		}
		for _, l := range leaves {
			if got := hex.EncodeToString(tree.Leaves[l].Script); got != v[l+"_script"].(string) {
				t.Errorf("%s %s script = %s want %s", name, l, got, v[l+"_script"])
			}
			if got := hex.EncodeToString(tree.Leaves[l].Control); got != v[l+"_control"].(string) {
				t.Errorf("%s %s control = %s want %s", name, l, got, v[l+"_control"])
			}
		}
	}
	for _, name := range []string{"htlc_claw", "htlc_noclaw"} {
		v := vs[name]
		s := func(k string) string { return v[k].(string) }
		h := SwapHTLC{
			Hash: mustHex32(s("hash")), BuyerX: mustHex32(s("buyer_x")), PolicyX: mustHex32(s("policy_x")),
			TSeq: uint32(v["t_seq"].(float64)), SellerProgram: mustHex32(s("seller_program")),
			Asset: mustHex32(s("asset")), Value: uint64(v["value"].(float64)),
		}
		leaves := []string{"claim", "refund"}
		if s("issuer_x") != "" {
			x := mustHex32(s("issuer_x"))
			h.IssuerX = &x
			leaves = append(leaves, "claw")
		}
		tree, err := h.Tree()
		if err != nil {
			t.Fatal(err)
		}
		check(name, tree, v, leaves...)
	}
	for _, name := range []string{"pot_v1", "pot_v1_b"} {
		v := vs[name]
		s := func(k string) string { return v[k].(string) }
		if v["seller_version"].(float64) != 1 {
			t.Fatalf("%s: a pot pays home to a v1 program only", name)
		}
		p := FeePot{
			Hash: mustHex32(s("hash")), BuyerX: mustHex32(s("buyer_x")), TSeq: uint32(v["t_seq"].(float64)),
			SellerProgram: mustHex32(s("seller_program")), FeeAsset: mustHex32(s("fee_asset")),
			Value: uint64(v["value"].(float64)),
		}
		tree, err := p.Tree()
		if err != nil {
			t.Fatal(err)
		}
		check(name, tree, v, "claim", "refund")
	}
}

func TestSwapLocksAreTimes(t *testing.T) {
	if _, err := (SwapHTLC{TSeq: 499_999_999, Value: 1}).Tree(); err == nil {
		t.Fatal("an HTLC with a height lock was built")
	}
	if _, err := (FeePot{TSeq: 499_999_999, Value: 1}).Tree(); err == nil {
		t.Fatal("a pot with a height lock was built")
	}
}

func TestPotSecretReadsOnlyThePreimage(t *testing.T) {
	s := sha256.Sum256([]byte("secret"))
	h := sha256.Sum256(s[:])
	other := sha256.Sum256([]byte("other"))
	sig := make([]byte, 64)
	if got, ok := PotSecret([][]byte{sig, s[:], {0x82}, {0xc4}}, h); !ok || hex.EncodeToString(got) != hex.EncodeToString(s[:]) {
		t.Fatal("the secret in a claim witness was not read")
	}
	if _, ok := PotSecret([][]byte{sig, other[:], {0x82}, {0xc4}}, h); ok {
		t.Fatal("a 32-byte item that is not the preimage was read as the secret")
	}
	if _, ok := PotSecret([][]byte{{0x01}, {0xc4}}, h); ok {
		t.Fatal("a refund witness gave a secret")
	}
}
