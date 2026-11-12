package main

// Each quote of a restricted maker has its own exit-for-requote, and every exit withdraws
// the quote it leaves. Before, the exit was armed once, for the first quote: after a fill
// the maker posted its next quote, and the first quote's timer then ended the process with
// that next quote still on the relay. Its supervisor ran the maker again within the relay's
// one-minute grace, so the relay kept the orphan beside the new quote, and the new process
// refused every lift of it as "stale_offer" (review RV45, S1).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// restrictedQuoter makes signed quotes under key; the first expires at firstExpiry, the
// rest in an hour.
func restrictedQuoter(key *btcec.PrivateKey, firstExpiry time.Duration) func() (*seqobv1.Offer, error) {
	var mu sync.Mutex
	n := 0
	return func() (*seqobv1.Offer, error) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		exp := time.Now().Add(time.Hour)
		if first {
			exp = time.Now().Add(firstExpiry)
		}
		o := &seqobv1.Offer{OfferId: randstr.Hex(16), SchemaVersion: 1,
			Pair:       &seqobv1.AssetPair{BaseAsset: strings.Repeat("aa", 32), QuoteAsset: strings.Repeat("bb", 32)},
			TradeDir:   seqobv1.TradeDir_TRADE_DIR_SELL,
			OfferAsset: strings.Repeat("aa", 32), OfferAmount: 100, WantAsset: strings.Repeat("bb", 32), WantAmount: 9,
			BaseAmount: 100, MinFill: 100,
			CreatedAtUnix: uint64(time.Now().Unix()), ExpiresAtUnix: uint64(exp.Unix())}
		return o, offer.SignOffer(o, key)
	}
}

// TestRestrictedRequoteChild is the maker process of TestRestrictedRequoteLeavesNoOrphanQuote:
// it runs the real exit path (os.Exit), so it runs in a process of its own.
func TestRestrictedRequoteChild(t *testing.T) {
	relayURL := os.Getenv("RESTRICTED_REQUOTE_RELAY")
	if relayURL == "" {
		t.Skip("run by TestRestrictedRequoteLeavesNoOrphanQuote")
	}
	key, _ := btcec.NewPrivateKey()
	// The first quote expires in 66 s, so its exit-for-requote (a minute before expiry)
	// comes due 6 s in.
	quote := restrictedQuoter(key, 66*time.Second)
	o, _ := quote()
	wsURL := "ws" + strings.TrimPrefix(relayURL, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		t.Fatal(err)
	}
	srv := &restrictedServer{relay: relayURL, wsURL: wsURL, key: key, expiry: time.Hour, whole: 100, ws: ws,
		quote: quote, exit: os.Exit, retryPause: time.Second,
		lift: func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (string, bool, error) {
			return fmt.Sprintf("%064d", 1), true, nil
		}}
	srv.serve(o)
}

// The first quote is filled at once and the next one posted; when the first quote's
// requote time comes, the maker must not end and leave the next quote on the relay.
func TestRestrictedRequoteLeavesNoOrphanQuote(t *testing.T) {
	r := newFakeRelay(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestRestrictedRequoteChild$")
	cmd.Env = append(os.Environ(), "RESTRICTED_REQUOTE_RELAY="+r.srv.URL)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	first := next(t, r.submits, "first quote")
	r.lift(t, first)
	next(t, r.cancels, "cancel of the filled quote")
	second := next(t, r.submits, "next quote")
	select {
	case err := <-done:
		withdrawn := false
		for len(r.cancels) > 0 {
			if <-r.cancels == second {
				withdrawn = true
			}
		}
		if !withdrawn {
			t.Fatalf("the maker ended (%v) at the first quote's requote time and left its next quote %s on the relay, withdrawn by no one and served by no process", err, second)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// startRequoting runs the restricted loop in this process with due as the requote timing
// and lift as each lift; exits receives the code the maker would end with.
func startRequoting(t *testing.T, r *fakeRelay, due func(*seqobv1.Offer) (time.Duration, string),
	lift func() (bool, error)) (exits chan int) {
	key, _ := btcec.NewPrivateKey()
	quote := restrictedQuoter(key, time.Hour)
	o, err := quote()
	if err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		t.Fatal(err)
	}
	exits = make(chan int, 4)
	srv := &restrictedServer{relay: r.srv.URL, wsURL: wsURL, key: key, expiry: time.Hour, whole: 100, ws: ws,
		quote: quote, exit: func(code int) { exits <- code }, retryPause: 10 * time.Millisecond, due: due,
		lift: func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (string, bool, error) {
			settled, err := lift()
			return fmt.Sprintf("%064d", 1), settled, err
		}}
	go srv.serve(o)
	return exits
}

// dueFirstOnly makes only the first quote's requote come due, after d.
func dueFirstOnly(d time.Duration) func(*seqobv1.Offer) (time.Duration, string) {
	var mu sync.Mutex
	n := 0
	return func(*seqobv1.Offer) (time.Duration, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return d, "offer expiring"
		}
		return time.Hour, "offer expiring"
	}
}

// An exit for a requote withdraws the quote it leaves before the process ends.
func TestRestrictedRequoteExitWithdrawsItsQuote(t *testing.T) {
	r := newFakeRelay(t)
	exits := startRequoting(t, r, dueFirstOnly(200*time.Millisecond), func() (bool, error) { return true, nil })
	first := next(t, r.submits, "first quote")
	if got := next(t, r.cancels, "withdrawal of the quote at its requote time"); got != first {
		t.Fatalf("withdrew %s, want the quote served, %s", got, first)
	}
	select {
	case code := <-exits:
		if code != 0 {
			t.Fatalf("exit %d for a requote", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit at the requote time")
	}
}

// A quote filled before its requote time has no exit left: the next quote serves on.
func TestRestrictedRequoteOfAFilledQuoteIsVoid(t *testing.T) {
	r := newFakeRelay(t)
	exits := startRequoting(t, r, dueFirstOnly(400*time.Millisecond), func() (bool, error) { return true, nil })
	first := next(t, r.submits, "first quote")
	r.lift(t, first)
	next(t, r.cancels, "cancel of the filled quote")
	second := next(t, r.submits, "next quote")
	select {
	case code := <-exits:
		t.Fatalf("exited %d at the filled quote's requote time, leaving %s", code, second)
	case id := <-r.cancels:
		t.Fatalf("withdrew %s at the filled quote's requote time", id)
	case <-time.After(1200 * time.Millisecond):
	}
}

// A requote that comes due during a lift waits for the lift to end; a lift that does not
// settle leaves the quote served, so the exit then withdraws it and ends the process.
func TestRestrictedRequoteWaitsForTheLiftInFlight(t *testing.T) {
	r := newFakeRelay(t)
	release := make(chan struct{})
	exits := startRequoting(t, r, dueFirstOnly(200*time.Millisecond), func() (bool, error) {
		<-release
		return false, fmt.Errorf("the taker went away")
	})
	first := next(t, r.submits, "first quote")
	r.lift(t, first)
	select {
	case code := <-exits:
		t.Fatalf("exited %d with a lift in flight", code)
	case id := <-r.cancels:
		t.Fatalf("withdrew %s with a lift in flight", id)
	case <-time.After(700 * time.Millisecond):
	}
	close(release)
	if got := next(t, r.cancels, "withdrawal once the lift ended"); got != first {
		t.Fatalf("withdrew %s, want %s", got, first)
	}
	select {
	case code := <-exits:
		if code != 0 {
			t.Fatalf("exit %d for a requote", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit once the lift ended")
	}
}

// At start a restricted maker withdraws the quotes an earlier run of it left: its key's
// restricted quotes on this pair and side. Its gated order, a quote on the other side,
// another kind of offer and other makers' quotes stay.
func TestRestrictedMakerWithdrawsEarlierQuotes(t *testing.T) {
	key, _ := btcec.NewPrivateKey()
	me := fmt.Sprintf("%x", key.PubKey().SerializeCompressed())
	var mu sync.Mutex
	cancelled := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/market/"+strings.Repeat("aa", 32)+"/"+strings.Repeat("bb", 32)+"/orderbook":
			fmt.Fprintf(w, `{"offers":[
{"offer_id":"stale","maker_pubkey":"%[1]s","trade_dir":"TRADE_DIR_SELL","restricted":{"policy_server":"x"}},
{"offer_id":"now","maker_pubkey":"%[1]s","trade_dir":"TRADE_DIR_SELL","restricted":{}},
{"offer_id":"gated","maker_pubkey":"%[1]s","trade_dir":"TRADE_DIR_SELL","gated_order":{"x":1}},
{"offer_id":"buyside","maker_pubkey":"%[1]s","trade_dir":"TRADE_DIR_BUY","restricted":{}},
{"offer_id":"plain","maker_pubkey":"%[1]s","trade_dir":"TRADE_DIR_SELL","same_chain":{}},
{"offer_id":"theirs","maker_pubkey":"02ff","trade_dir":"TRADE_DIR_SELL","restricted":{}}]}`, me)
		case strings.HasSuffix(r.URL.Path, "/orderbook"):
			fmt.Fprint(w, `{"offers":[]}`)
		case r.URL.Path == "/v1/offers/cancel":
			var c struct {
				OfferID string `json:"offer_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&c)
			mu.Lock()
			cancelled = append(cancelled, c.OfferID)
			mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &restrictedServer{relay: srv.URL, key: key}
	s.withdrawEarlier(&seqobv1.Offer{OfferId: "now", TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL,
		Pair: &seqobv1.AssetPair{BaseAsset: strings.Repeat("aa", 32), QuoteAsset: strings.Repeat("bb", 32)}})
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(cancelled, ",") != "stale" {
		t.Fatalf("withdrew %v, want only the earlier run's quote", cancelled)
	}
}
