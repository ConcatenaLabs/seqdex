package watcher

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// fakeOperator serves the three calls ArcaRounds makes: /v1/info, /v1/rounds
// (two pages) and /v1/tree.
func fakeOperator(t *testing.T, genesis, rootSPK, asset string, pages *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"operator": "AB" + strings.Repeat("cd", 31), "genesis_hash": genesis})
		case "/v1/rounds":
			*pages++
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			for k := range req {
				if k != "after" && k != "limit" {
					t.Errorf("rounds request carries %q (the operator refuses unknown fields)", k)
				}
			}
			switch req["after"] {
			case nil:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"rounds": []map[string]interface{}{
					{"cursor": "1", "txid": strings.Repeat("11", 32), "state": "final", "batches": []uint32{2}},
				}, "next": "1"})
			case "1":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"rounds": []map[string]interface{}{
					{"cursor": "2", "txid": strings.Repeat("22", 32), "state": "broadcast", "batches": []uint32{4}},
				}, "next": "2"})
			default:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"rounds": []interface{}{}})
			}
		case "/v1/tree":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"asset": asset, "nodes": [][]map[string]string{
				{{"script_pubkey": "5120" + strings.Repeat("99", 32)}, {"script_pubkey": "5120" + strings.Repeat("98", 32)}},
				{{"script_pubkey": rootSPK}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestArcaRoundsKnowsAnOperatorsRound(t *testing.T) {
	var asset [32]byte
	asset[0] = 0x77
	rootSPK := "5120" + strings.Repeat("55", 32)
	pages := 0
	srv := fakeOperator(t, "ge", rootSPK, displayOf(asset), &pages)
	defer srv.Close()
	spk, _ := hexDecode(rootSPK)
	outs := make([]covenant.Output, 6)
	outs[4] = covenant.Explicit(asset, 1000, spk)

	a := NewArcaRounds([]string{srv.URL + "/"}, "GE")
	r, ok, err := a.Round(strings.Repeat("22", 32), outs)
	if err != nil || !ok {
		t.Fatalf("round on the second page: ok %v err %v", ok, err)
	}
	if r.Operator != "ab"+strings.Repeat("cd", 31) || len(r.BatchVouts) != 1 || r.BatchVouts[0] != 4 {
		t.Fatalf("round info: %+v", r)
	}
	// Any other transaction is not a round (and does not page again within a second).
	if _, ok, _ := a.Round(strings.Repeat("33", 32), outs); ok {
		t.Fatal("an unlisted transaction taken as a round")
	}
	if pages != 3 {
		t.Fatalf("pages read %d, want 3 (two with rounds, one empty)", pages)
	}
	// A batch output that does not pay its published tree's root: not a round.
	outs2 := make([]covenant.Output, 6)
	outs2[2] = covenant.Explicit(asset, 1000, []byte{0x51})
	if _, ok, _ := a.Round(strings.Repeat("11", 32), outs2); ok {
		t.Fatal("a round whose batch output is not its tree's root was taken")
	}
}

func TestArcaRoundsIgnoresAnOperatorOnAnotherChain(t *testing.T) {
	pages := 0
	srv := fakeOperator(t, "other", "51", "00", &pages)
	defer srv.Close()
	a := NewArcaRounds([]string{srv.URL}, "ge")
	if _, ok, err := a.Round(strings.Repeat("22", 32), nil); ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if pages != 0 {
		t.Fatalf("paged an operator on another chain %d times", pages)
	}
}
