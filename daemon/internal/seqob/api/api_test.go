package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

func newServer(t *testing.T) (*httptest.Server, *offerstore.Store) {
	t.Helper()
	store := offerstore.New(nil)
	v := validator.New(validator.DefaultConfig(), nil)
	router := session.NewRouter(session.Options{Deadline: time.Minute})
	srv := New(store, v, router, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func mkSignedOffer(t *testing.T, k *btcec.PrivateKey, id string) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix()),
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// mkSignedOfferNS builds a signed offer in the requested book namespace: confidential=false
// is the transparent book, true the separate blinded book. The confidential flag is part of
// the canonical signed bytes, so it is set BEFORE signing.
func mkSignedOfferNS(t *testing.T, k *btcec.PrivateKey, id string, confidential bool) *seqobv1.Offer {
	t.Helper()
	o := mkSignedOffer(t, k, id)
	o.Confidential = confidential
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

func postProto(t *testing.T, url string, m proto.Message) *http.Response {
	t.Helper()
	b, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRESTSubmitMarketsOrderbook(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")

	resp := postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Markets lists the pair.
	var ml seqobv1.MarketList
	getProto(t, ts.URL+"/v1/markets", &ml)
	if len(ml.GetMarkets()) != 1 || ml.GetMarkets()[0].GetPair().GetBaseAsset() != "gold" {
		t.Fatalf("markets = %+v", ml.GetMarkets())
	}

	// Orderbook shows the offer.
	var book seqobv1.PublicBook
	getProto(t, ts.URL+"/v1/market/gold/usdx/orderbook", &book)
	if len(book.GetOffers()) != 1 || book.GetOffers()[0].GetOfferId() != "aaaa" {
		t.Fatalf("orderbook = %+v", book.GetOffers())
	}

	// Own orders.
	var own seqobv1.PublicBook
	getProto(t, ts.URL+"/v1/offers?maker_pubkey="+o.GetMakerPubkey(), &own)
	if len(own.GetOffers()) != 1 {
		t.Fatalf("own offers = %+v", own.GetOffers())
	}
}

func TestRESTRejectsBadSignature(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")
	o.WantAmount = 1 // break the signature
	resp := postProto(t, ts.URL+"/v1/offers", o)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad signature, got %d", resp.StatusCode)
	}
}

// TestRESTReplayIsNoOp is the ITEM A integration regression: replaying a genuine,
// byte-identical offer must be a no-op (HTTP 200, not a 409 conflict) and must not
// duplicate the book entry; proving api.New wired the book into the validator so
// a replay never reaches store.Submit (and never charged the maker's budget).
func TestRESTReplayIsNoOp(t *testing.T) {
	ts, store := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")

	resp := postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first submit = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay submit = %d, want 200 (no-op, not a conflict)", resp.StatusCode)
	}
	resp.Body.Close()

	if got := len(store.SnapshotMaker(o.GetMakerPubkey())); got != 1 {
		t.Fatalf("book holds %d offers after replay, want exactly 1", got)
	}
}

func TestWSSnapshotAndDelta(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)

	// Pre-existing offer should appear in the WS snapshot.
	resp := postProto(t, ts.URL+"/v1/offers", mkSignedOffer(t, k, "aaaa"))
	resp.Body.Close()

	c := dialWS(t, ts)
	defer c.Close()
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}}})

	snap := readFrom(t, c)
	if snap.GetPublicBook() == nil || len(snap.GetPublicBook().GetOffers()) != 1 {
		t.Fatalf("expected snapshot with 1 offer, got %+v", snap)
	}

	// New offer arrives as a created delta.
	resp = postProto(t, ts.URL+"/v1/offers", mkSignedOffer(t, k, "bbbb"))
	resp.Body.Close()
	delta := readFrom(t, c)
	if delta.GetPublicOrderCreated() == nil || delta.GetPublicOrderCreated().GetOfferId() != "bbbb" {
		t.Fatalf("expected created delta for bbbb, got %+v", delta)
	}
}

// TestWSNamespaceIsolation is the S7 regression: the market_subscribe snapshot + deltas must
// be filtered to the subscription's book NAMESPACE, so a transparent subscriber NEVER receives
// a confidential offer's snapshot/create and vice-versa — matching REST (?confidential=) and
// the matcher (which refuses cross-namespace crossing). Before the fix both books were served
// interleaved to every subscriber.
func TestWSNamespaceIsolation(t *testing.T) {
	ts, store := newServer(t)
	k := key(t)

	// Pre-existing offers, one per namespace, injected directly into the shared store so the
	// snapshot sent at subscribe time exercises the namespace split (store.Submit only checks
	// the signature; it does not require the validator's confidential-book preconditions).
	if _, err := store.Submit(mkSignedOfferNS(t, k, "t-pre", false)); err != nil {
		t.Fatalf("submit t-pre: %v", err)
	}
	if _, err := store.Submit(mkSignedOfferNS(t, k, "c-pre", true)); err != nil {
		t.Fatalf("submit c-pre: %v", err)
	}

	transparentPair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}
	confidentialPair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx", Confidential: true}

	// A transparent subscriber: its snapshot must contain ONLY the transparent offer.
	connT := dialWS(t, ts)
	defer connT.Close()
	sendTo(t, connT, &seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: transparentPair}})
	snapT := readFrom(t, connT).GetPublicBook()
	if snapT == nil || len(snapT.GetOffers()) != 1 || snapT.GetOffers()[0].GetOfferId() != "t-pre" {
		t.Fatalf("transparent snapshot must be exactly [t-pre], got %+v", snapT.GetOffers())
	}

	// A confidential subscriber: its snapshot must contain ONLY the confidential offer.
	connC := dialWS(t, ts)
	defer connC.Close()
	sendTo(t, connC, &seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: confidentialPair}})
	snapC := readFrom(t, connC).GetPublicBook()
	if snapC == nil || len(snapC.GetOffers()) != 1 || snapC.GetOffers()[0].GetOfferId() != "c-pre" {
		t.Fatalf("confidential snapshot must be exactly [c-pre], got %+v", snapC.GetOffers())
	}

	// Deltas, submitted in a fixed order that makes the namespace filter falsifiable:
	//   c-x (confidential), t-x (transparent), c-y (confidential).
	// The transparent stream, filtered, is [t-x]; the confidential stream is [c-x, c-y].
	if _, err := store.Submit(mkSignedOfferNS(t, k, "c-x", true)); err != nil {
		t.Fatalf("submit c-x: %v", err)
	}
	if _, err := store.Submit(mkSignedOfferNS(t, k, "t-x", false)); err != nil {
		t.Fatalf("submit t-x: %v", err)
	}
	if _, err := store.Submit(mkSignedOfferNS(t, k, "c-y", true)); err != nil {
		t.Fatalf("submit c-y: %v", err)
	}

	// Transparent subscriber's FIRST delta must be t-x. c-x was submitted before t-x, so if the
	// confidential offer leaked into this stream it would arrive first and this assertion fails.
	dt := readFrom(t, connT).GetPublicOrderCreated()
	if dt == nil || dt.GetOfferId() != "t-x" {
		t.Fatalf("transparent subscriber first delta must be t-x (confidential c-x must not leak), got %+v", dt)
	}

	// Confidential subscriber's stream must be exactly [c-x, c-y]. Its SECOND delta being c-y —
	// not the transparent t-x submitted between them — proves the transparent offer was filtered out.
	d1 := readFrom(t, connC).GetPublicOrderCreated()
	if d1 == nil || d1.GetOfferId() != "c-x" {
		t.Fatalf("confidential subscriber first delta must be c-x, got %+v", d1)
	}
	d2 := readFrom(t, connC).GetPublicOrderCreated()
	if d2 == nil || d2.GetOfferId() != "c-y" {
		t.Fatalf("confidential subscriber second delta must be c-y (transparent t-x must not leak), got %+v", d2)
	}
}

func TestWSOpaqueCourierWithLiftNotification(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t) // maker's offer key doubles as its E2E session key
	o := mkSignedOffer(t, mk, "aaaa")

	// Maker connects via WS and submits the offer (registers as reachable).
	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, makerConn); st.GetOrderStatus() == nil {
		t.Fatalf("expected order_status after maker submit, got %+v", st)
	}

	// Taker connects and lifts the offer with its ephemeral session pubkey.
	tk := key(t)
	takerConn := dialWS(t, ts)
	defer takerConn.Close()
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})

	// Maker is notified via lift_requested carrying the TAKER's session pubkey.
	mn := readFrom(t, makerConn)
	lr := mn.GetLiftRequested()
	if lr == nil {
		t.Fatalf("expected lift_requested at maker, got %+v", mn)
	}
	if !bytes.Equal(lr.GetTakerSessionPubkey(), tk.PubKey().SerializeCompressed()) {
		t.Fatalf("lift_requested did not carry the taker session pubkey")
	}
	sessionID := lr.GetSessionId()

	// Taker gets lift_accepted carrying the maker's (offer) session pubkey.
	la := readFrom(t, takerConn)
	if la.GetLiftAccepted() == nil || la.GetLiftAccepted().GetSessionId() != sessionID {
		t.Fatalf("expected lift_accepted, got %+v", la)
	}
	if !bytes.Equal(la.GetLiftAccepted().GetMakerSessionPubkey(), mk.PubKey().SerializeCompressed()) {
		t.Fatalf("lift_accepted did not carry the maker offer pubkey as session key")
	}

	// Real E2E: taker seals to the maker's offer key; the relay couriers opaque
	// bytes; the maker opens with its offer key + the taker's session pubkey.
	makerPub, _ := btcec.ParsePubKey(la.GetLiftAccepted().GetMakerSessionPubkey())
	takerCrypter, _ := client.NewCrypter(tk, makerPub)
	takerPub, _ := btcec.ParsePubKey(lr.GetTakerSessionPubkey())
	makerCrypter, _ := client.NewCrypter(mk, takerPub)

	plaintext := []byte("confidential SwapRequest plaintext")
	sealed, err := takerCrypter.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: sealed}}})

	relayed := readFrom(t, makerConn)
	if relayed.GetSwapMsg() == nil || relayed.GetSwapMsg().GetSessionId() != sessionID {
		t.Fatalf("maker did not receive the courier frame, got %+v", relayed)
	}
	// Relay carried only ciphertext (it never saw the plaintext).
	if bytes.Equal(relayed.GetSwapMsg().GetCiphertext(), plaintext) {
		t.Fatalf("relay must courier ciphertext, not plaintext")
	}
	opened, err := makerCrypter.Open(relayed.GetSwapMsg().GetCiphertext())
	if err != nil {
		t.Fatalf("maker could not open the couriered payload: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("E2E round-trip mismatch: %q", opened)
	}
}

// TestWSSessionReattach covers P3.8(i): after a taker's WS drops, a RECONNECTED
// socket re-binds to the still-live session by re-proving control of its ephemeral
// session key, and again receives couriered frames. A wrong signature is rejected.
func TestWSSessionReattach(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa")

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn) // order_status

	// Taker lifts, then its socket drops.
	tk := key(t)
	takerConn := dialWS(t, ts)
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})
	lr := readFrom(t, makerConn).GetLiftRequested()
	if lr == nil {
		t.Fatal("expected lift_requested at maker")
	}
	sessionID := lr.GetSessionId()
	readFrom(t, takerConn) // lift_accepted
	takerConn.Close()      // the drop
	time.Sleep(50 * time.Millisecond)

	// A reconnected socket with a WRONG key is rejected (403 -> GenericError).
	reconnect := dialWS(t, ts)
	defer reconnect.Close()
	wrong := key(t)
	sendTo(t, reconnect, &seqobv1.To{Msg: &seqobv1.To_SessionReattach{SessionReattach: &seqobv1.SessionReattach{
		SessionId: sessionID, Role: "taker", Sig: offer.SignReattach(sessionID, "taker", wrong),
	}}})
	if e := readFrom(t, reconnect).GetError(); e == nil || e.GetCode() != 403 {
		t.Fatalf("wrong-key reattach must be rejected 403, got %+v", e)
	}

	// The genuine taker re-attaches by signing the challenge with its session key.
	sendTo(t, reconnect, &seqobv1.To{Msg: &seqobv1.To_SessionReattach{SessionReattach: &seqobv1.SessionReattach{
		SessionId: sessionID, Role: "taker", Sig: offer.SignReattach(sessionID, "taker", tk),
	}}})
	ra := readFrom(t, reconnect).GetSessionReattached()
	if ra == nil || ra.GetSessionId() != sessionID {
		t.Fatalf("expected session_reattached, got session %v", ra)
	}

	// The maker couriers a frame; the RE-ATTACHED taker socket receives it.
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{
		SessionId: sessionID, Ciphertext: []byte("post-reattach"),
	}}})
	got := readFrom(t, reconnect)
	if got.GetSwapMsg() == nil || string(got.GetSwapMsg().GetCiphertext()) != "post-reattach" {
		t.Fatalf("re-attached taker did not receive the couriered frame, got %+v", got)
	}
}

// TestWSSettleAckRecordsTradeAndDecrements is the B-1 regression: an interactive lift
// settles via the opaque courier the relay never parses, so the ONLY way the relay learns
// it settled is the maker's SettleAck. On it, the relay must record the trade and decrement
// the resting order (previously it did neither, leaving a ghost order + an empty trades feed).
// It must also reject a settle_ack from anyone but the session maker.
func TestWSSettleAckRecordsTradeAndDecrements(t *testing.T) {
	ts, store := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa") // base_amount 100, pair gold/usdx

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, makerConn); st.GetOrderStatus() == nil {
		t.Fatalf("expected order_status after submit, got %+v", st)
	}

	tk := key(t)
	takerConn := dialWS(t, ts)
	defer takerConn.Close()
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})
	lr := readFrom(t, makerConn).GetLiftRequested()
	if lr == nil {
		t.Fatalf("expected lift_requested at maker")
	}
	sessionID := lr.GetSessionId()
	_ = readFrom(t, takerConn) // drain lift_accepted

	// A TAKER may NOT ack settlement — only the session maker. Expect a 403 error frame.
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
		SessionId: sessionID, SettleTxid: "aa", FillBase: 50,
	}}})
	if ef := readFrom(t, takerConn); ef.GetError() == nil {
		t.Fatalf("expected error frame for a taker-sent settle_ack, got %+v", ef)
	}

	// The MAKER acks: the relay records the trade at the resting price and decrements the order.
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
		SessionId: sessionID, SettleTxid: "deadbeef", FillBase: 50,
	}}})

	// The handler is async and sends no reply on success, so poll the store to convergence.
	restKey := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "aaaa"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		trades := store.TradesFor(o.GetPair(), 10)
		e, ok := store.Get(restKey)
		if len(trades) >= 1 && ok && e.ActiveAmount == 50 {
			if trades[len(trades)-1].Size != 50 {
				t.Fatalf("recorded trade size = %d, want 50", trades[len(trades)-1].Size)
			}
			return // recorded + decremented: B-1 satisfied
		}
		if time.Now().After(deadline) {
			t.Fatalf("settle_ack did not record+decrement in time: trades=%d ok=%v entry=%+v", len(trades), ok, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mkSignedCovSell builds a signed COVENANT SELL offer (funded on-chain), used to
// prove covenant lifts are exempt from per-offer serialization (P3.10).
func mkSignedCovSell(t *testing.T, k *btcec.PrivateKey, id string) *seqobv1.Offer {
	t.Helper()
	b32 := func(f byte) []byte {
		o := make([]byte, 32)
		for i := range o {
			o[i] = f
		}
		return o
	}
	o := mkSignedOffer(t, k, id)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: strings.Repeat("c0", 30) + fmt.Sprintf("%04x", len(id)), CovenantVout: 0,
		AssetA: "aa", AssetB: "bb", RateNum: 45, RateDen: 100,
		MinLot: 1, MakerProgVer: 1, ExpiryLocktime: 400, MakerProg: b32(0xc3), MakerX: b32(0xd4),
	}}
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestSerializeLiftPerOffer covers P3.10: only ONE live lift session per interactive
// offer. A second concurrent lift is refused (409), and the slot frees once the first
// session ends (here, the maker settles a partial fill).
func TestSerializeLiftPerOffer(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa") // interactive SELL, base 100

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn) // order_status

	// Taker 1 lifts 50 -> accepted (session opens).
	tk1 := key(t)
	t1 := dialWS(t, ts)
	defer t1.Close()
	sendTo(t, t1, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk1.PubKey().SerializeCompressed(),
	}}})
	sess1 := readFrom(t, makerConn).GetLiftRequested().GetSessionId()
	if la := readFrom(t, t1).GetLiftAccepted(); la == nil || la.GetSessionId() != sess1 {
		t.Fatalf("taker1 lift not accepted, got %v", la)
	}

	// Taker 2 lifts the SAME offer concurrently -> REFUSED (409, lift in progress).
	tk2 := key(t)
	t2 := dialWS(t, ts)
	defer t2.Close()
	sendTo(t, t2, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk2.PubKey().SerializeCompressed(),
	}}})
	if e := readFrom(t, t2).GetError(); e == nil || e.GetCode() != 409 {
		t.Fatalf("second concurrent lift must be refused 409, got %+v", e)
	}

	// The maker settles session1 (partial 50): session1 closes, freeing the slot.
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
		SessionId: sess1, SettleTxid: "txX", FillBase: 50,
	}}})

	// Taker 2 retries; once the slot frees the lift is accepted.
	deadline := time.Now().Add(3 * time.Second)
	for {
		sendTo(t, t2, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
			OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
			TakerSessionPubkey: tk2.PubKey().SerializeCompressed(),
		}}})
		fr := readFrom(t, t2)
		if fr.GetLiftAccepted() != nil {
			return // slot freed after session1 settled: P3.10 satisfied
		}
		if fr.GetError() == nil {
			t.Fatalf("unexpected frame while retrying lift: %+v", fr)
		}
		if time.Now().After(deadline) {
			t.Fatal("lift slot never freed after the first session settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCovenantLiftNotSerialized covers the P3.10 exemption: covenant offers are
// funded on-chain (first-spend-wins), so concurrent lifts are safe and both open.
func TestCovenantLiftNotSerialized(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedCovSell(t, mk, "cov1")

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn) // order_status

	for i, tk := range []*btcec.PrivateKey{key(t), key(t)} {
		tc := dialWS(t, ts)
		defer tc.Close()
		sendTo(t, tc, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
			OfferId: "cov1", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 30,
			TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
		}}})
		if la := readFrom(t, tc).GetLiftAccepted(); la == nil {
			t.Fatalf("covenant lift %d must be accepted (not serialized), got refusal", i)
		}
	}
}

// apiManualReorg is a ReorgWatcher whose orphan callback the test fires by hand. The
// callback is delivered over a buffered channel so the server goroutine that arms it
// and the test goroutine that fires it synchronize (no data race).
type apiManualReorg struct{ armed chan func() }

func (m *apiManualReorg) WatchSettlement(_ string, _ string, onOrphaned func()) {
	select {
	case m.armed <- onOrphaned:
	default:
	}
}

// TestReorgUndoReopensOfferAndRetractsTrade is the P3.9 end-to-end proof: an
// interactive lift settles (trade recorded, order fully filled + removed); then a
// Bitcoin reorg orphans the settling tx and the relay RE-OPENS the exact order and
// RETRACTS the phantom trade. It wires the same onReopen the daemon uses.
func TestReorgUndoReopensOfferAndRetractsTrade(t *testing.T) {
	store := offerstore.New(nil)
	v := validator.New(validator.DefaultConfig(), nil)
	rw := &apiManualReorg{armed: make(chan func(), 1)}
	onReopen := func(s *session.Session) { // mirrors cmd/seqobd's onReopen
		o := s.SettledOffer()
		if o == nil {
			return
		}
		k := offerstore.Key{MakerPubkey: s.MakerPubkey, OfferID: s.OfferID}
		cur := uint64(0)
		if e, ok := store.Get(k); ok {
			cur = e.ActiveAmount
		}
		_ = store.Reopen(o, cur+s.SettledFill())
		if txid := s.SettleTxid(); txid != "" {
			store.RetractTrade(o.GetPair(), txid)
		}
	}
	router := session.NewRouter(session.Options{Deadline: time.Minute, Reorg: rw, OnReopen: onReopen})
	ts := httptest.NewServer(New(store, v, router, nil).Handler())
	defer ts.Close()

	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa") // SELL 100 gold for 45 usdx (ask 0.45)
	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn) // order_status

	tk := key(t)
	takerConn := dialWS(t, ts)
	defer takerConn.Close()
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 100, // full fill
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})
	sessionID := readFrom(t, makerConn).GetLiftRequested().GetSessionId()
	readFrom(t, takerConn) // lift_accepted

	// Maker acks the full fill: records the trade (txid "txR") and removes the order.
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
		SessionId: sessionID, SettleTxid: "txR", FillBase: 100,
	}}})

	// Wait for the reorg watch to be armed (the settle handler ran), then converge on
	// the settled book state.
	var fire func()
	select {
	case fire = <-rw.armed:
	case <-time.After(2 * time.Second):
		t.Fatal("settle handler never armed the reorg watch")
	}
	restKey := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "aaaa"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, resting := store.Get(restKey)
		trades := store.TradesFor(o.GetPair(), 10)
		if !resting && len(trades) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settle did not converge: resting=%v trades=%d", resting, len(trades))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A Bitcoin reorg orphans the settling tx.
	fire()

	// The order is re-opened at its full size and the phantom trade is retracted.
	e, ok := store.Get(restKey)
	if !ok || e.ActiveAmount != 100 {
		t.Fatalf("reorg-undo must re-open the order at 100, got ok=%v %+v", ok, e)
	}
	if tr := store.TradesFor(o.GetPair(), 10); len(tr) != 0 {
		t.Fatalf("reorg-undo must retract the phantom trade, got %d", len(tr))
	}
}

// TestWSSettledTradeRecordsCrossLNFillSessionIndependent is the P2.8 core: a durable
// cross-chain / Lightning lift settles off the opaque courier path, so the relay learns of
// it only from the maker's SettledTrade. Unlike SettleAck it must NOT depend on a live
// session/role — the swap can finish after the co-sign session was swept or the maker
// reconnected — so this test rests the offer over REST and reports settlement on a FRESH WS
// connection with no session at all. The relay must record the trade + decrement the order,
// and reject a tampered (bad-signature) report without touching the book.
func TestWSSettledTradeRecordsCrossLNFillSessionIndependent(t *testing.T) {
	ts, store := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa") // base 100, pair gold/usdx, want 45 -> price 0.45

	if resp := postProto(t, ts.URL+"/v1/offers", o); resp.StatusCode != http.StatusOK {
		t.Fatalf("submit offer: status %d", resp.StatusCode)
	}

	// Report settlement on a connection that never opened a lift session (no role): the
	// maker-signed, offer-keyed SettledTrade must still record the trade and decrement.
	conn := dialWS(t, ts)
	defer conn.Close()
	st := &seqobv1.SettledTrade{OfferId: "aaaa", FillBase: 40, SettleTxid: "deadbeef", Nonce: 1}
	if err := offer.SignSettledTrade(st, mk); err != nil {
		t.Fatal(err)
	}
	sendTo(t, conn, &seqobv1.To{Msg: &seqobv1.To_SettledTrade{SettledTrade: st}})

	restKey := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "aaaa"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		trades := store.TradesFor(o.GetPair(), 10)
		e, ok := store.Get(restKey)
		if len(trades) >= 1 && ok && e.ActiveAmount == 60 {
			if trades[len(trades)-1].Size != 40 {
				t.Fatalf("recorded trade size = %d, want 40", trades[len(trades)-1].Size)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settled_trade did not record+decrement in time: trades=%d ok=%v entry=%+v", len(trades), ok, e)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A tampered report (a signed field mutated after signing) is rejected with an error
	// frame and must not decrement the resting order.
	bad := &seqobv1.SettledTrade{OfferId: "aaaa", FillBase: 60, Nonce: 2}
	if err := offer.SignSettledTrade(bad, mk); err != nil {
		t.Fatal(err)
	}
	bad.FillBase = 10 // invalidate the signature
	sendTo(t, conn, &seqobv1.To{Msg: &seqobv1.To_SettledTrade{SettledTrade: bad}})
	if ef := readFrom(t, conn); ef.GetError() == nil {
		t.Fatalf("expected an error frame for a tampered settled_trade, got %+v", ef)
	}
	if e, ok := store.Get(restKey); !ok || e.ActiveAmount != 60 {
		t.Fatalf("rejected settled_trade must not change the book: %+v ok=%v", e, ok)
	}
}

// --- helpers ---

func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return c
}

func sendTo(t *testing.T, c *websocket.Conn, to *seqobv1.To) {
	t.Helper()
	b, err := protojson.Marshal(to)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatal(err)
	}
}

func readFrom(t *testing.T, c *websocket.Conn) *seqobv1.From {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read From: %v", err)
	}
	var from seqobv1.From
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &from); err != nil {
		t.Fatalf("unmarshal From: %v", err)
	}
	return &from
}

func getProto(t *testing.T, url string, m proto.Message) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", url, resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(buf.Bytes(), m); err != nil {
		t.Fatalf("unmarshal %s: %v", url, err)
	}
}

// decodeProto reads an httptest response body into a proto message.
func decodeProto(t *testing.T, resp *http.Response, m proto.Message) {
	t.Helper()
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(buf.Bytes(), m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
}

// mkSignedBuy builds a signed interactive BUY of `base` gold for `quote` usdx (bid =
// quote/base) with the given time-in-force.
func mkSignedBuy(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, tif seqobv1.TimeInForce) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_BUY,
		BaseAmount:    base,
		WantAmount:    base,
		WantAsset:     "gold",
		OfferAmount:   quote,
		OfferAsset:    "usdx",
		AllowPartial:  true,
		TimeInForce:   tif,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix()),
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestRESTOrderOptionsDisposition covers P2.9 at the REST submit boundary. An
// interactive IOC or FOK order that finds no auto-cross (interactive is owner-guarded,
// P3.7) is RETIRED (CANCELLED, not left resting); a POST_ONLY that would not take rests.
func TestRESTOrderOptionsDisposition(t *testing.T) {
	ts, store := newServer(t)
	mk, tk := key(t), key(t)

	// Resting interactive SELL (ask 0.45).
	if resp := postProto(t, ts.URL+"/v1/offers", mkSignedOffer(t, mk, "ask")); resp.StatusCode != http.StatusOK {
		t.Fatalf("resting submit status %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// IOC BUY bidding 0.50 crosses the ask, but interactive has no auto-settle owner,
	// so it fills nothing and IOC cancels the remainder -> CANCELLED, not resting.
	var st seqobv1.OrderStatus
	decodeProto(t, postProto(t, ts.URL+"/v1/offers", mkSignedBuy(t, tk, "ioc", 100, 50, seqobv1.TimeInForce_TIME_IN_FORCE_IOC)), &st)
	if st.GetStatus() != seqobv1.OfferStatus_OFFER_STATUS_CANCELLED {
		t.Fatalf("IOC status = %v, want CANCELLED", st.GetStatus())
	}
	if _, ok := store.Get(offerstore.Key{MakerPubkey: st.GetMakerPubkey(), OfferID: "ioc"}); ok {
		t.Fatal("IOC order must not rest in the book")
	}

	// FOK BUY that cannot fill in full -> CANCELLED, not resting.
	var st2 seqobv1.OrderStatus
	decodeProto(t, postProto(t, ts.URL+"/v1/offers", mkSignedBuy(t, tk, "fok", 100, 50, seqobv1.TimeInForce_TIME_IN_FORCE_FOK)), &st2)
	if st2.GetStatus() != seqobv1.OfferStatus_OFFER_STATUS_CANCELLED {
		t.Fatalf("FOK status = %v, want CANCELLED", st2.GetStatus())
	}
	if _, ok := store.Get(offerstore.Key{MakerPubkey: st2.GetMakerPubkey(), OfferID: "fok"}); ok {
		t.Fatal("FOK order must not rest in the book")
	}

	// POST_ONLY BUY bidding 0.40 does NOT cross the 0.45 ask -> rests (OPEN).
	var st3 seqobv1.OrderStatus
	decodeProto(t, postProto(t, ts.URL+"/v1/offers", mkSignedBuy(t, tk, "po", 100, 40, seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY)), &st3)
	if st3.GetStatus() != seqobv1.OfferStatus_OFFER_STATUS_OPEN {
		t.Fatalf("POST_ONLY (no cross) status = %v, want OPEN", st3.GetStatus())
	}
	if _, ok := store.Get(offerstore.Key{MakerPubkey: st3.GetMakerPubkey(), OfferID: "po"}); !ok {
		t.Fatal("POST_ONLY order that did not take must rest in the book")
	}
}

// The covenant fill records are served per pair, as the watcher left them:
// the taker of an Arca round named as the operator's round, no taker output.
func TestFillsServesTheCovenantFillRecords(t *testing.T) {
	ts, store := newServer(t)
	defer ts.Close()
	pair := &seqobv1.AssetPair{BaseAsset: "aa", QuoteAsset: "bb"}
	store.RecordCovenantFill(offerstore.CovenantFill{
		Order: offerstore.Key{MakerPubkey: "mk", OfferID: "o1"}, Pair: pair, Txid: "round1", Input: 1, Spent: "f:0",
		Locked: 100, Filled: 40, Remainder: 60,
		Credit: offerstore.FillOutput{Vout: 2, Asset: "bb", Value: 80, Script: "5120"},
		Rest:   &offerstore.FillOutput{Vout: 3, Asset: "aa", Value: 60, Script: "5120"},
		Taker:  offerstore.FillTaker{Via: offerstore.TakerArcaRound, Operator: "0p", RoundTxid: "round1", BatchVouts: []uint32{4}},
		State:  offerstore.FillConfirmed, AnchorDepth: 1, RequiredDepth: 2,
	}, nil)
	resp, err := http.Get(ts.URL + "/v1/market/aa/bb/fills")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Fills []map[string]interface{} `json:"fills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Fills) != 1 {
		t.Fatalf("fills: %+v", got)
	}
	f := got.Fills[0]
	if f["offer_id"] != "o1" || f["txid"] != "round1" || f["input"].(float64) != 1 || f["state"] != "confirmed" {
		t.Fatalf("record: %+v", f)
	}
	if c := f["credit"].(map[string]interface{}); c["vout"].(float64) != 2 {
		t.Fatalf("credit: %+v", c)
	}
	if tk := f["taker"].(map[string]interface{}); tk["via"] != "arca_round" || tk["operator"] != "0p" {
		t.Fatalf("taker: %+v", tk)
	}
}
