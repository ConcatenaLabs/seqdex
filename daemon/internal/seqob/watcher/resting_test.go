package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// fakeTreeOperator serves what the relay reads of an operator for its resting
// orders: /v1/info, /v1/order_state, /v1/rounds and /v1/tree.
type fakeTreeOperator struct {
	mu     sync.Mutex
	key    string
	orders map[string]RestingState
	down   bool
	round  string // the round whose batch output 1 holds every order's first coin
}

func (f *fakeTreeOperator) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, `{"code":"internal","message":"down"}`, http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"operator": f.key, "genesis_hash": "ge",
				"fills": map[string]interface{}{"order_margin_seconds": 21600}, "participations": map[string]interface{}{"exit_deadline_seconds": 259200}})
		case "/v1/order_state":
			var req struct {
				Orders []string `json:"orders"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			out := []RestingState{}
			for _, id := range req.Orders {
				if s, ok := f.orders[id]; ok {
					out = append(out, s)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"orders": out})
		case "/v1/rounds":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["after"] != nil {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"rounds": []interface{}{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"rounds": []map[string]interface{}{
				{"cursor": "1", "txid": f.round, "state": "final", "batches": []uint32{1}}}, "next": "1"})
		case "/v1/tree":
			leaves := []map[string]string{{"leaf_id": strings.Repeat("99", 32)}}
			for id := range f.orders {
				leaves = append(leaves, map[string]string{"leaf_id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"leaves": leaves})
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *fakeTreeOperator) set(id string, edit func(*RestingState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.orders[id]
	edit(&s)
	f.orders[id] = s
}

const (
	restAssetA = "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f"
	restAssetB = "2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e"
)

var (
	restOrderID = strings.Repeat("d1", 32)
	restCoin2   = strings.Repeat("e2", 32)
	restCoin3   = strings.Repeat("e3", 32)
	restRefund  = int64(1_900_000_000)
)

func restingState(coin, value string) RestingState {
	exp := int64(1_890_000_000)
	return RestingState{Order: restOrderID, State: "resting", Coin: coin, Asset: restAssetA, Value: value,
		Terms: RestingStateTerms{AssetB: restAssetB, RateNum: "2", RateDen: "1", MinLot: "10000",
			Maker: strings.Repeat("31", 32), Expiry: restRefund, RefundKey: strings.Repeat("41", 32)},
		FillsLeft: 4, Expiry: &exp}
}

func restingOfferFor(t *testing.T, url, opKey, coin string, value uint64) *seqobv1.Offer {
	t.Helper()
	b32 := func(c byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = c
		}
		return out
	}
	o := &seqobv1.Offer{
		OfferId: "rest-1", SchemaVersion: 1, Pair: &seqobv1.AssetPair{BaseAsset: restAssetA, QuoteAsset: restAssetB},
		TradeDir: seqobv1.TradeDir_TRADE_DIR_SELL, BaseAmount: 600000, OfferAmount: 600000, OfferAsset: restAssetA,
		WantAmount: 1200000, WantAsset: restAssetB, AllowPartial: true, MinFill: 10000,
		CreatedAtUnix: uint64(time.Now().Unix()), ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix()),
		Settlement: &seqobv1.Offer_RestingLeaf{RestingLeaf: &seqobv1.RestingLeafTerms{
			OperatorUrl: url, OperatorKey: opKey, OrderId: restOrderID, AssetA: restAssetA, AssetB: restAssetB,
			RateNum: 2, RateDen: 1, MinLot: 10000, LandedMaker: b32(0x31), LandedExpiry: uint32(restRefund), RefundKey: b32(0x41),
			Coin: coin, Value: value,
		}},
	}
	priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 7))
	if err := offer.SignOffer(o, priv); err != nil {
		t.Fatal(err)
	}
	return o
}

func newRestingRig(t *testing.T) (*fakeTreeOperator, *httptest.Server, *ArcaRounds, *fakeChain) {
	op := &fakeTreeOperator{key: strings.Repeat("5c", 32), orders: map[string]RestingState{restOrderID: restingState(restOrderID, "600000")},
		round: strings.Repeat("77", 32)}
	srv := op.serve(t)
	t.Cleanup(srv.Close)
	return op, srv, NewArcaRounds([]string{srv.URL}, "ge"), newFakeChain()
}

func TestRestingProbeAdmitsOnlyWhatTheOperatorReports(t *testing.T) {
	op, srv, ops, chain := newRestingRig(t)
	now := time.Unix(1_800_000_000, 0)
	p := RestingProbe{Ops: ops, Chain: chain, Now: func() time.Time { return now }}
	check := func(name string, o *seqobv1.Offer, want string) {
		t.Helper()
		v, err := p.CheckOffer(context.Background(), o)
		if want == "" {
			if err != nil || v != o.GetRestingLeaf().GetValue() {
				t.Fatalf("%s: listed %d, %v", name, v, err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want %q", name, err, want)
		}
	}
	check("as reported", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), "")
	check("a coin the operator does not report", restingOfferFor(t, srv.URL, op.key, restCoin2, 600000),
		"the operator reports order "+restOrderID+" resting at coin "+restOrderID+", not at "+restCoin2)
	check("more than the leaf holds", restingOfferFor(t, srv.URL, op.key, restOrderID, 500000),
		"holding 600000 atoms, not the 500000 the offer names")
	op.set(restOrderID, func(s *RestingState) { s.Value = "400000" })
	check("a value above what the leaf holds", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000),
		"holding 400000 atoms, not the 600000 the offer names")
	op.set(restOrderID, func(s *RestingState) { s.Value = "600000"; s.Terms.RateNum = "3" })
	check("other terms", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), "has rate_num 3 at its operator, not the 2 the offer signs")
	op.set(restOrderID, func(s *RestingState) { s.Terms.RateNum = "2" })
	check("an operator not followed", restingOfferFor(t, srv.URL, strings.Repeat("7e", 32), restOrderID, 600000), "follows no operator with key "+strings.Repeat("7e", 32))
	check("the public URL, not the relay's", restingOfferFor(t, "https://sequentiatestnet.com/arca", op.key, restOrderID, 600000), "")
	other := restingOfferFor(t, srv.URL, op.key, restOrderID, 600000)
	other.GetRestingLeaf().OrderId = strings.Repeat("d2", 32)
	check("an order the operator does not report", other, "reports no resting order "+strings.Repeat("d2", 32))
	// The batch's first expiry less three days comes before the REFUND less six hours.
	now = time.Unix(1_890_000_000-259200, 0)
	check("at its exit deadline", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), "is past its delist time 1889740800")
	now = time.Unix(1_800_000_000, 0)
	op.set(restOrderID, func(s *RestingState) { s.Expiry = nil })
	now = time.Unix(restRefund-21600, 0)
	check("at its REFUND time less six hours", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), "is past its delist time 1899978400")
	now = time.Unix(1_800_000_000, 0)
	op.set(restOrderID, func(s *RestingState) { s.State = "cancelled" })
	check("cancelled", restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), "is cancelled at its operator: it takes no fill")
	op.mu.Lock()
	op.down = true
	op.mu.Unlock()
	ops2 := NewArcaRounds([]string{srv.URL}, "ge")
	if _, err := (RestingProbe{Ops: ops2, Chain: chain}).CheckOffer(context.Background(), restingOfferFor(t, srv.URL, op.key, restOrderID, 600000)); err == nil || !strings.Contains(err.Error(), "that it can ask now") {
		t.Fatalf("operator down: %v", err)
	}
}

func TestRestingFollowerMovesRecordsAndDelists(t *testing.T) {
	op, srv, ops, chain := newRestingRig(t)
	store := offerstore.New(nil)
	o := restingOfferFor(t, srv.URL, op.key, restOrderID, 600000)
	k, err := store.SubmitAt(o, 600000)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResting(ops, chain, store, nil)
	r.SetFinalAnchorDepth(2)
	r.SetNow(func() time.Time { return time.Unix(1_800_000_000, 0) })
	active := func() (uint64, string, bool) {
		e, ok := store.Get(k)
		if !ok {
			return 0, "", false
		}
		c, _, _ := offer.RestingLocator(e.Offer)
		return e.ActiveAmount, c, true
	}
	pair := o.GetPair()

	// A part fill: the remainder rests at a new coin; the order's round is in a
	// block, anchor 100, the tip's anchor 100: confirmed, not final.
	chain.txs[op.round] = TxStatus{InChain: true, Block: "b1", Height: 50, Confirmations: 1, BlockAnchor: 100, CertPresent: true, Certified: true}
	op.set(restOrderID, func(s *RestingState) {
		s.Coin, s.Value = restCoin2, "400000"
		s.Fills = []RestingStateFill{{TransferID: strings.Repeat("aa", 32), Coin: restOrderID, Filled: "200000", Credited: "400000", Partial: true, Signed: true}}
	})
	r.Once()
	if a, c, ok := active(); !ok || a != 400000 || c != restCoin2 {
		t.Fatalf("after a part fill: active %d at %s (listed %v)", a, c, ok)
	}
	if e, _ := store.Get(k); e.Status != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL || offer.VerifyOffer(e.Offer) != nil {
		t.Fatalf("re-pointed order: %v, signature %v", e.Status, offer.VerifyOffer(e.Offer))
	}
	fills := store.CovenantFills(pair, 0)
	if len(fills) != 1 || fills[0].Txid != strings.Repeat("aa", 32) || fills[0].State != offerstore.FillConfirmed || fills[0].Filled != 200000 ||
		fills[0].Locked != 600000 || fills[0].Remainder != 400000 || fills[0].Credit.Value != 400000 || fills[0].Taker.Via != offerstore.TakerOperatorTree ||
		fills[0].Taker.RoundTxid != op.round || fills[0].Rest == nil || fills[0].Rest.Value != 400000 {
		t.Fatalf("fill record: %+v", fills)
	}
	if tr := store.TradesFor(pair, 0); len(tr) != 1 || tr[0].Size != 200000 {
		t.Fatalf("trades: %+v", tr)
	}
	// Buried two anchors: final.
	chain.anchor.Height = 102
	r.Once()
	if f := store.CovenantFills(pair, 0); f[0].State != offerstore.FillFinal || f[0].AnchorDepth != 2 {
		t.Fatalf("buried: %+v", f[0])
	}
	// The round leaves the active chain: undone, its trade retracted; back: recorded again.
	chain.txs[op.round] = TxStatus{InMempool: true}
	r.Once()
	if f := store.CovenantFills(pair, 0); f[0].State != offerstore.FillUndone || !strings.Contains(f[0].Reason, "left the active chain") {
		t.Fatalf("rolled back: %+v", f[0])
	}
	if tr := store.TradesFor(pair, 0); len(tr) != 0 {
		t.Fatalf("trades after the undo: %+v", tr)
	}
	chain.txs[op.round] = TxStatus{InChain: true, Block: "b1'", Height: 50, Confirmations: 3, BlockAnchor: 100, CertPresent: true, Certified: true}
	r.Once()
	if f := store.CovenantFills(pair, 0); len(f) != 2 || f[1].State != offerstore.FillFinal {
		t.Fatalf("mined again: %+v", f)
	}
	// The operator cannot be asked: listed at 0; back: listed again.
	op.mu.Lock()
	op.down = true
	op.mu.Unlock()
	r.Once()
	if a, _, ok := active(); !ok || a != 0 {
		t.Fatalf("operator down: active %d listed %v", a, ok)
	}
	op.mu.Lock()
	op.down = false
	op.mu.Unlock()
	r.Once()
	if a, _, _ := active(); a != 400000 {
		t.Fatalf("operator back: active %d", a)
	}
	// A part fill the operator has recorded but whose remainder it does not hold yet:
	// the order reads "filled" at the coin it rested at. Listed at 0, not delisted;
	// then at the remainder once the operator holds it.
	op.set(restOrderID, func(s *RestingState) {
		s.State = "filled"
		s.Fills = append(s.Fills, RestingStateFill{TransferID: strings.Repeat("ab", 32), Coin: restCoin2, Filled: "100000", Credited: "200000", Partial: true, Signed: false})
	})
	r.Once()
	if a, _, ok := active(); !ok || a != 0 {
		t.Fatalf("a part fill in progress: active %d, listed %v", a, ok)
	}
	op.set(restOrderID, func(s *RestingState) {
		s.State, s.Coin, s.Value = "resting", restCoin3, "300000"
		s.Fills[len(s.Fills)-1].Signed = true
	})
	r.Once()
	if a, c, ok := active(); !ok || a != 300000 || c != restCoin3 {
		t.Fatalf("after the part fill: active %d at %s, listed %v", a, c, ok)
	}
	// The rest filled whole: delisted FILLED, the last fill recorded.
	// First recorded and not yet signed: held at 0, not delisted, so its record is not
	// lost; then signed: recorded, and delisted.
	op.set(restOrderID, func(s *RestingState) {
		s.State, s.Coin, s.Value = "filled", restCoin3, "300000"
		s.Fills = append(s.Fills, RestingStateFill{TransferID: strings.Repeat("bb", 32), Coin: restCoin3, Filled: "300000", Credited: "600000", Signed: false})
	})
	r.Once()
	if a, _, ok := active(); !ok || a != 0 {
		t.Fatalf("a whole fill not signed yet: active %d, listed %v", a, ok)
	}
	op.set(restOrderID, func(s *RestingState) { s.Fills[len(s.Fills)-1].Signed = true })
	r.Once()
	if _, _, ok := active(); ok {
		t.Fatal("a whole fill left the order listed")
	}
	f := store.CovenantFills(pair, 0)
	if last := f[len(f)-1]; last.Txid != strings.Repeat("bb", 32) || last.Locked != 300000 || last.Remainder != 0 || last.Rest != nil || last.State != offerstore.FillFinal {
		t.Fatalf("whole fill: %+v", last)
	}
}

func TestRestingFollowerDelists(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(*RestingState)
		now    int64
		listed bool
	}{
		{"cancelled", func(s *RestingState) { s.State = "cancelled" }, 1_800_000_000, false},
		{"landed or not live", func(s *RestingState) { s.State = "unknown" }, 1_800_000_000, false},
		{"filled whole", func(s *RestingState) { s.State = "filled" }, 1_800_000_000, false},
		{"its coin not live yet", func(s *RestingState) { s.State = "pending" }, 1_800_000_000, true},
		{"its credit keys used", func(s *RestingState) { s.State = "exhausted" }, 1_800_000_000, false},
		{"gone from the operator", nil, 1_800_000_000, false},
		{"at its delist time", func(s *RestingState) {}, 1_890_000_000 - 259200, false},
		{"a second before it", func(s *RestingState) {}, 1_890_000_000 - 259201, true},
	} {
		op, srv, ops, chain := newRestingRig(t)
		store := offerstore.New(nil)
		k, err := store.SubmitAt(restingOfferFor(t, srv.URL, op.key, restOrderID, 600000), 600000)
		if err != nil {
			t.Fatal(err)
		}
		if c.edit == nil {
			op.mu.Lock()
			delete(op.orders, restOrderID)
			op.mu.Unlock()
		} else {
			op.set(restOrderID, c.edit)
		}
		r := NewResting(ops, chain, store, nil)
		now := c.now
		r.SetNow(func() time.Time { return time.Unix(now, 0) })
		r.Once()
		if _, ok := store.Get(k); ok != c.listed {
			t.Fatalf("%s: listed %v", c.name, ok)
		}
	}
}

func TestOneListingPerRestingOrder(t *testing.T) {
	_, srv, _, _ := newRestingRig(t)
	store := offerstore.New(nil)
	a := restingOfferFor(t, srv.URL, strings.Repeat("5c", 32), restOrderID, 600000)
	if _, err := store.SubmitAt(a, 600000); err != nil {
		t.Fatal(err)
	}
	b := restingOfferFor(t, srv.URL, strings.Repeat("5c", 32), restOrderID, 600000)
	b.OfferId = "rest-2"
	priv, _ := btcec.PrivKeyFromBytes(append(make([]byte, 31), 9))
	b.MakerPubkey = ""
	if err := offer.SignOffer(b, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitAt(b, 600000); err == nil || !strings.Contains(err.Error(), "resting order already listed") {
		t.Fatalf("a second listing of one order: %v", err)
	}
}
