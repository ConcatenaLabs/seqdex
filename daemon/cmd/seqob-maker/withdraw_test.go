package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// A maker restarted under the same key withdraws the offers its earlier run left on the relay,
// on both orientations of its pair, and keeps the ones it just posted and other makers' offers.
func TestWithdrawStale(t *testing.T) {
	key, _ := btcec.NewPrivateKey()
	me := fmt.Sprintf("%x", key.PubKey().SerializeCompressed())
	var mu sync.Mutex
	cancelled := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/aa/bb/orderbook"):
			fmt.Fprintf(w, `{"offers":[{"offer_id":"old1","maker_pubkey":"%s"},{"offer_id":"new1","maker_pubkey":"%s"},{"offer_id":"theirs","maker_pubkey":"02ff"}]}`, me, me)
		case strings.HasSuffix(r.URL.Path, "/bb/aa/orderbook"):
			fmt.Fprintf(w, `{"offers":[{"offer_id":"old2","maker_pubkey":"%s"}]}`, strings.ToUpper(me))
		case r.URL.Path == "/v1/offers/cancel":
			var c struct {
				OfferID string `json:"offer_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&c)
			mu.Lock()
			cancelled = append(cancelled, c.OfferID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := withdrawStale(srv.URL, "aa", "bb", me, map[string]bool{"new1": true}, key)
	if n != 2 || strings.Join(cancelled, ",") != "old1,old2" {
		t.Fatalf("withdrew %d: %v", n, cancelled)
	}
}
