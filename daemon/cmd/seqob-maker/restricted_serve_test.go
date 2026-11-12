package main

// A restricted maker quotes again after every fill (seqdex #165). Before, after a fill it
// cancelled the offer, met the relay's 409 "not found" (the relay had retired the filled
// offer already), and then sat on a healthy connection with no offer: it exited only on a
// websocket read error, so its supervisor never re-ran it and the book read "No one is
// buying" until someone restarted it by hand.

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// fakeRelay records the offers a maker submits on its socket and the cancels it posts, and
// answers each cancel as the relay does for an offer it has already retired.
type fakeRelay struct {
	srv     *httptest.Server
	submits chan string
	cancels chan string
	mu      sync.Mutex
	conn    *websocket.Conn
}

func newFakeRelay(t *testing.T) *fakeRelay {
	r := &fakeRelay{submits: make(chan string, 16), cancels: make(chan string, 16)}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", func(w http.ResponseWriter, req *http.Request) {
		c, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		r.mu.Lock()
		r.conn = c
		r.mu.Unlock()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var to seqobv1.To
			if jsonUnmarshal.Unmarshal(data, &to) != nil {
				continue
			}
			if o := to.GetOfferSubmit(); o != nil {
				r.submits <- o.GetOfferId()
			}
		}
	})
	mux.HandleFunc("/v1/offers/cancel", func(w http.ResponseWriter, req *http.Request) {
		b, _ := ioutil.ReadAll(req.Body)
		var c seqobv1.OfferCancel
		_ = jsonUnmarshal.Unmarshal(b, &c)
		r.cancels <- c.GetOfferId()
		httpStatus := http.StatusConflict
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		fmt.Fprintf(w, `{"code":409,"message":"cancel: offer 02de5d32/%s not found"}`, c.GetOfferId())
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

// lift sends the maker a lift of offer id, as the relay does when a taker lifts it.
func (r *fakeRelay) lift(t *testing.T, id string) {
	t.Helper()
	k, _ := btcec.NewPrivateKey()
	from := &seqobv1.From{Msg: &seqobv1.From_LiftRequested{LiftRequested: &seqobv1.LiftRequested{
		SessionId: randstr.Hex(8), OfferId: id, TakerSessionPubkey: k.PubKey().SerializeCompressed()}}}
	b, err := jsonMarshal.Marshal(from)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatal(err)
	}
}

func next(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5 s", what)
		return ""
	}
}

// startRestricted runs the restricted maker's loop against the fake relay. Every lift
// settles at once; quote makes a new signed offer, or fails while quoteFails is set.
func startRestricted(t *testing.T, r *fakeRelay, quoteFails func() bool) (exits chan int) {
	key, _ := btcec.NewPrivateKey()
	exits = make(chan int, 4)
	n := 0
	var qmu sync.Mutex
	quote := func() (*seqobv1.Offer, error) {
		if quoteFails != nil && quoteFails() {
			return nil, fmt.Errorf("the enclave holds 0 of the 100 this offer sells")
		}
		qmu.Lock()
		n++
		qmu.Unlock()
		o := &seqobv1.Offer{OfferId: randstr.Hex(16), SchemaVersion: 1,
			Pair:       &seqobv1.AssetPair{BaseAsset: strings.Repeat("aa", 32), QuoteAsset: strings.Repeat("bb", 32)},
			TradeDir:   seqobv1.TradeDir_TRADE_DIR_SELL,
			OfferAsset: strings.Repeat("aa", 32), OfferAmount: 100, WantAsset: strings.Repeat("bb", 32), WantAmount: 9,
			BaseAmount: 100, MinFill: 100,
			CreatedAtUnix: uint64(time.Now().Unix()), ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix())}
		if err := offer.SignOffer(o, key); err != nil {
			return nil, err
		}
		return o, nil
	}
	o, err := quote()
	if err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		t.Fatal(err)
	}
	fills := 0
	srv := &restrictedServer{relay: r.srv.URL, wsURL: wsURL, key: key, expiry: time.Hour, whole: 100, ws: ws,
		quote: quote, exit: func(code int) { exits <- code },
		lift: func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (string, bool, error) {
			fills++
			return fmt.Sprintf("%064d", fills), true, nil
		}}
	go srv.serve(o)
	return exits
}

func TestRestrictedMakerQuotesAgainAfterEveryFill(t *testing.T) {
	r := newFakeRelay(t)
	startRestricted(t, r, nil)
	first := next(t, r.submits, "first quote")
	r.lift(t, first)
	if got := next(t, r.cancels, "cancel of the filled offer"); got != first {
		t.Fatalf("cancelled %s, want the filled %s", got, first)
	}
	second := next(t, r.submits, "next quote after the first fill (the relay answered the cancel 409 not found)")
	if second == first {
		t.Fatalf("the next quote reuses the filled offer's id %s", first)
	}
	r.lift(t, second)
	next(t, r.cancels, "cancel of the second filled offer")
	third := next(t, r.submits, "next quote after the second fill")
	if third == second || third == first {
		t.Fatalf("the third quote reuses an id: %s", third)
	}
}

func TestRestrictedMakerExitsNonZeroWhenItCannotQuoteAgain(t *testing.T) {
	r := newFakeRelay(t)
	var mu sync.Mutex
	fail := false
	exits := startRestricted(t, r, func() bool { mu.Lock(); defer mu.Unlock(); return fail })
	first := next(t, r.submits, "first quote")
	mu.Lock()
	fail = true
	mu.Unlock()
	r.lift(t, first)
	select {
	case code := <-exits:
		if code == 0 {
			t.Fatalf("exited 0: a supervisor that restarts only on failure would leave the book empty")
		}
	case id := <-r.submits:
		t.Fatalf("posted %s though it cannot fill it", id)
	case <-time.After(5 * time.Second):
		t.Fatal("neither quoted nor exited: the maker sits connected with no offer")
	}
}

func TestCancelOfAnOfferTheRelayRetiredIsSuccess(t *testing.T) {
	if !cancelAlreadyGone(fmt.Errorf(`status 409: {"code":409,"message":"cancel: offer 02de5d32/4e40accc not found"}`)) {
		t.Fatal("a 409 not found on cancelling a filled offer is the relay having retired it")
	}
	if cancelAlreadyGone(fmt.Errorf(`status 409: {"code":409,"message":"cancel: bad signature"}`)) {
		t.Fatal("another 409 is a failure")
	}
	if cancelAlreadyGone(fmt.Errorf("dial tcp: connection refused")) {
		t.Fatal("a failed post is a failure")
	}
}

// Every other mode that cancels its offer after a whole fill without -requote exits then
// for its supervisor to quote again, rather than waiting for its websocket to fail.
func TestEveryModeExitsAfterAWholeFill(t *testing.T) {
	for _, f := range []string{"main.go", "submarine.go", "pureln.go", "subasset.go", "subasset_sell.go"} {
		b, err := ioutil.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		n := strings.Count(src, "cancelOffer(cfg.relay, o, cfg.makerKey)\n")
		if n == 0 || strings.Count(src, "cancelOffer(cfg.relay, o, cfg.makerKey)\n\t\t\t\t\t\texitAfterFill(idle)\n") != n {
			t.Errorf("%s: a cancel after a whole fill is not followed by exitAfterFill", f)
		}
	}
}
