package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

// heldProbe stands in for the chain probe: what each covenant outpoint holds, or
// the refusal the chain would give.
type heldProbe struct {
	held   map[string]uint64
	refuse error
}

func (p heldProbe) CheckOffer(_ context.Context, o *seqobv1.Offer) (uint64, error) {
	if p.refuse != nil {
		return 0, p.refuse
	}
	txid, _, ok := offer.CovenantOutpoint(o)
	if !ok {
		return 0, nil
	}
	return p.held[txid], nil
}

func newServerWithProbe(t *testing.T, p validator.LivenessProbe) (*httptest.Server, *offerstore.Store) {
	t.Helper()
	store := offerstore.New(nil)
	v := validator.New(validator.DefaultConfig(), p)
	srv := New(store, v, session.NewRouter(session.Options{Deadline: time.Minute}), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

// A covenant order resting at its remainder (its outpoint holds 37 of the 100 its
// maker signed) is listed at 37 when its signed offer is posted, by REST and by
// the WebSocket, and keeps that size when its maker re-signs it.
func TestCovenantListedAtWhatItsOutpointHolds(t *testing.T) {
	mk := key(t)
	o := mkSignedCovSell(t, mk, "cov1")
	txid, _, _ := offer.CovenantOutpoint(o)
	p := heldProbe{held: map[string]uint64{txid: 37}}
	k := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}

	ts, store := newServerWithProbe(t, p)
	var st seqobv1.OrderStatus
	resp := postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	decodeProto(t, resp, &st)
	if st.GetActiveAmount() != 37 || st.GetStatus() != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL {
		t.Fatalf("REST post listed %d %v, want 37 PARTIAL", st.GetActiveAmount(), st.GetStatus())
	}
	if e, _ := store.Get(k); e.ActiveAmount != 37 {
		t.Fatalf("store holds %d", e.ActiveAmount)
	}

	// The same signed offer on another relay (a restart), over the WebSocket.
	ts2, store2 := newServerWithProbe(t, p)
	c := dialWS(t, ts2)
	defer c.Close()
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if got := readFrom(t, c).GetOrderStatus(); got.GetActiveAmount() != 37 {
		t.Fatalf("WS post listed %v, want 37", got)
	}

	// Its maker re-signs it (a reconnecting agent's refresh): still 37, not 100.
	re := mkSignedCovSell(t, mk, "cov1")
	re.CreatedAtUnix = o.GetCreatedAtUnix() + 1
	re.MakerSig = nil
	if err := offer.SignOffer(re, mk); err != nil {
		t.Fatal(err)
	}
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: re}})
	if got := readFrom(t, c).GetOrderStatus(); got.GetActiveAmount() != 37 {
		t.Fatalf("refresh listed %v, want 37", got)
	}
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_OfferEdit{OfferEdit: re}})
	if got := readFrom(t, c).GetOrderStatus(); got.GetActiveAmount() != 37 {
		t.Fatalf("edit listed %v, want 37", got)
	}
	if e, _ := store2.Get(k); e.ActiveAmount != 37 {
		t.Fatalf("store holds %d after the re-sign", e.ActiveAmount)
	}

	// An interactive offer is never listed below its signed size, whatever a probe says.
	ts3, _ := newServerWithProbe(t, heldProbe{held: map[string]uint64{"": 5}})
	resp = postProto(t, ts3.URL+"/v1/offers", mkSignedOffer(t, mk, "plain"))
	decodeProto(t, resp, &st)
	if st.GetActiveAmount() != 100 {
		t.Fatalf("an interactive offer listed at %d", st.GetActiveAmount())
	}
}

// What the chain refuses, the relay refuses, naming the chain's reason.
func TestCovenantRefusedForTheChainsReason(t *testing.T) {
	ts, store := newServerWithProbe(t, heldProbe{refuse: errors.New("covenant outpoint holds 4 atoms, below the order's min_lot 5: no fill can take it")})
	o := mkSignedCovSell(t, key(t), "cov1")
	resp := postProto(t, ts.URL+"/v1/offers", o)
	body := new(strings.Builder)
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body.Write(buf[:n])
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body.String(), "liveness: covenant outpoint holds 4 atoms, below the order's min_lot 5") {
		t.Fatalf("refusal %d %s", resp.StatusCode, body)
	}
	if _, ok := store.Get(offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}); ok {
		t.Fatal("a refused offer is in the book")
	}
}

// Every book the relay serves carries each order's live size beside its signed
// offer: the REST orderbook, a maker's own orders and the WebSocket snapshot.
// A covenant order's size follows what its outpoint holds through each fill.
func TestBookServesWhatEachOrderHolds(t *testing.T) {
	mk := key(t)
	cov := mkSignedCovSell(t, mk, "cov1")
	txid, _, _ := offer.CovenantOutpoint(cov)
	ts, store := newServerWithProbe(t, heldProbe{held: map[string]uint64{txid: 37}})
	k := offerstore.Key{MakerPubkey: cov.GetMakerPubkey(), OfferID: "cov1"}
	if resp := postProto(t, ts.URL+"/v1/offers", cov); resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	// An interactive offer whose maker never connected: past the ghost grace the
	// book drops it, and its status with it, so the two lists stay aligned.
	gk := key(t)
	ghost := mkSignedOffer(t, gk, "ghost")
	ghost.CreatedAtUnix -= 2 * ghostGraceSecs
	ghost.MakerSig = nil
	if err := offer.SignOffer(ghost, gk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Submit(ghost); err != nil {
		t.Fatal(err)
	}
	live := mkSignedOffer(t, mk, "live") // interactive, its maker's offer still young
	if _, err := store.Submit(live); err != nil {
		t.Fatal(err)
	}

	sizes := func(b *seqobv1.PublicBook) map[string]uint64 {
		t.Helper()
		if len(b.GetStatuses()) != len(b.GetOffers()) {
			t.Fatalf("%d statuses for %d offers", len(b.GetStatuses()), len(b.GetOffers()))
		}
		m := map[string]uint64{}
		for i, o := range b.GetOffers() {
			st := b.GetStatuses()[i]
			if st.GetOfferId() != o.GetOfferId() || st.GetMakerPubkey() != o.GetMakerPubkey() {
				t.Fatalf("status %d is %s/%s, offer is %s/%s", i, st.GetMakerPubkey(), st.GetOfferId(), o.GetMakerPubkey(), o.GetOfferId())
			}
			m[o.GetOfferId()] = st.GetActiveAmount()
		}
		return m
	}
	book := func() map[string]uint64 {
		var b seqobv1.PublicBook
		getProto(t, ts.URL+"/v1/market/gold/usdx/orderbook", &b)
		return sizes(&b)
	}
	if got := book(); len(got) != 2 || got["cov1"] != 37 || got["live"] != 100 {
		t.Fatalf("book %v, want cov1 37 and live 100 (the ghost dropped)", got)
	}
	// A fill in the mempool holds the order at 0; once confirmed it rests at its remainder.
	if err := store.HoldCovenantForSpend(k, "fill1"); err != nil {
		t.Fatal(err)
	}
	if got := book(); got["cov1"] != 0 {
		t.Fatalf("held: %v", got)
	}
	if err := store.RerestCovenantRemainder(k, strings.Repeat("ab", 32), 1, 12); err != nil {
		t.Fatal(err)
	}
	if got := book(); got["cov1"] != 12 {
		t.Fatalf("re-rested: %v", got)
	}

	var own seqobv1.PublicBook
	getProto(t, ts.URL+"/v1/offers?maker_pubkey="+cov.GetMakerPubkey(), &own)
	if got := sizes(&own); len(got) != 2 || got["cov1"] != 12 || got["live"] != 100 {
		t.Fatalf("own orders %v", got)
	}

	c := dialWS(t, ts)
	defer c.Close()
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}}})
	snap := readFrom(t, c).GetPublicBook()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if got := sizes(snap); len(got) != 2 || got["cov1"] != 12 {
		t.Fatalf("WS snapshot %v", got)
	}
}
