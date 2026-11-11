package restricted

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An OpenDAMP asset is enforced by its covenant, not by the policy server's
// co-signature, so openampd refuses to check or co-sign a trade of it. The
// maker reads every asset through Policy.Asset before it posts a quote
// (cmd/seqob-maker), so it never quotes one it could not trade.
func TestPolicyAssetRefusesADampAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enf := `"enforcement":"damp",`
		if strings.HasSuffix(r.URL.Path, "/amp") {
			enf = ""
		}
		_, _ = w.Write([]byte(`{"asset":"x",` + enf + `"policy_pub":"00","clawback":false}`))
	}))
	defer srv.Close()
	p := NewPolicy(srv.URL)
	if _, err := p.Asset("damp"); err == nil || !strings.Contains(err.Error(), `enforced by "damp"`) {
		t.Fatalf("a damp asset was read as one the policy server co-signs: %v", err)
	}
	if _, err := p.Asset("amp"); err != nil {
		t.Fatalf("an OpenAMP asset: %v", err)
	}
}
