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
