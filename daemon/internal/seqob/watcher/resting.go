package watcher

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// Orders resting as leaves in an Arca operator's tree (RestingLeafTerms).
//
// Such an order is not a coin on the chain: its operator holds it, co-signs
// each fill out of round, and publishes how every order stands (POST
// /v1/order_state). That view is the relay's only evidence, so:
//
//   - RestingProbe admits an offer only when the operator, one the relay
//     follows (-arca-operator) and on the relay's chain, whose key is the
//     offer's operator_key (the relay reaches it at the URL it was given, which
//     need not be the operator_url takers use), reports the order
//     resting at the offer's locator with exactly that value, under exactly the
//     offer's terms, and the order is before its delist time;
//   - Resting (the follower) asks the view again every pass, moves each listed
//     order to the coin its remainder rests at after a fill, delists it when it
//     is filled, cancelled, landed or used up, or at its delist time, and records
//     each fill by the operator's transfer id, final when the round the order's
//     lineage rests on is final at the relay's anchor depth.
//
// The delist time is the earlier of the order's REFUND time (the landed order's
// expiry, a median time) less the operator's order margin (six hours; the
// operator co-signs no fill within it), and the exit deadline of the batches the
// order's coin rests on (their first expiry less the operator's exit deadline,
// three days; the operator co-signs no spend of the coin after it). "Now" is the
// later of the relay's clock and the chain's median time past, so the relay
// delists no later than the operator stops filling.

// leafPos is the batch output a leaf rests in.
type leafPos struct {
	Txid string
	Vout uint32
}

// RestingState is one order as the operator's state view shows it.
type RestingState struct {
	Order          string             `json:"order"`
	State          string             `json:"state"`
	Coin           string             `json:"coin"`
	Asset          string             `json:"asset"`
	Value          string             `json:"value"`
	Owner          string             `json:"owner"`
	Salt           string             `json:"salt"`
	ExitDelayUnits int                `json:"exit_delay_units"`
	Terms          RestingStateTerms  `json:"terms"`
	FillsLeft      int                `json:"fills_left"`
	Expiry         *int64             `json:"expiry"`
	Record         string             `json:"record"`
	Fills          []RestingStateFill `json:"fills"`
}

// RestingStateTerms are the order's terms as the operator's leaf record holds
// them (asset ids in display hex, amounts in decimal).
type RestingStateTerms struct {
	AssetB    string   `json:"asset_b"`
	RateNum   string   `json:"rate_num"`
	RateDen   string   `json:"rate_den"`
	MinLot    string   `json:"min_lot"`
	Maker     string   `json:"maker"`
	Expiry    int64    `json:"expiry"`
	RefundKey string   `json:"refund_key"`
	Credits   []string `json:"credits"`
}

// RestingStateFill is one fill of an order: the transfer that spent the coin
// the order rested at, what it filled and what it credited the maker.
type RestingStateFill struct {
	TransferID string `json:"transfer_id"`
	Coin       string `json:"coin"`
	Filled     string `json:"filled"`
	Credited   string `json:"credited"`
	Partial    bool   `json:"partial"`
	Signed     bool   `json:"signed"`
}

// The most orders one order_state read names (the operator's limit).
const restingPerRead = 100

// operatorWithKey is the followed operator whose /v1/info key is key, on the
// relay's chain. An offer names its operator by key and by the URL takers
// reach it at; the relay reaches the operators it follows at the URLs it was
// given (-arca-operator), which need not be the public one, so it matches an
// offer to an operator by key.
func (a *ArcaRounds) operatorWithKey(key string) (*arcaOperator, error) {
	if a == nil {
		return nil, fmt.Errorf("the relay follows no operator")
	}
	key = strings.ToLower(key)
	var lastErr error
	for _, op := range a.ops {
		op.mu.Lock()
		err := a.infoOf(op)
		k, g := op.key, op.genesis
		op.mu.Unlock()
		if err != nil {
			lastErr = err
			continue
		}
		if k == key && (a.genesis == "" || g == a.genesis) {
			return op, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("the relay follows no operator with key %s that it can ask now (%v)", key, lastErr)
	}
	return nil, fmt.Errorf("the relay follows no operator with key %s on its chain", key)
}

// times are an operator's order margin and exit deadline, as its info says.
func (op *arcaOperator) times() (marginSecs, deadlineSecs int64) {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.marginSecs, op.deadlineSecs
}

// orderStates asks the operator how the orders ids stand. An order the
// operator has not published is absent from the answer.
func (a *ArcaRounds) orderStates(op *arcaOperator, ids []string) (map[string]RestingState, error) {
	out := make(map[string]RestingState, len(ids))
	for i := 0; i < len(ids); i += restingPerRead {
		j := i + restingPerRead
		if j > len(ids) {
			j = len(ids)
		}
		var res struct {
			Orders []RestingState `json:"orders"`
		}
		if err := a.call(http.MethodPost, op.url+"/v1/order_state", map[string]interface{}{"orders": ids[i:j]}, &res); err != nil {
			return nil, err
		}
		for _, s := range res.Orders {
			out[strings.ToLower(s.Order)] = s
		}
	}
	return out, nil
}

// leafRound finds the round and batch output holding leaf id among the trees
// the operator publishes, newest rounds first; ok is false when no published
// tree holds it (yet).
func (a *ArcaRounds) leafRound(op *arcaOperator, id string) (txid string, vout uint32, ok bool, err error) {
	id = strings.ToLower(id)
	op.mu.Lock()
	defer op.mu.Unlock()
	if p, ok := op.leafAt[id]; ok {
		return p.Txid, p.Vout, true, nil
	}
	if time.Since(op.lastPage) >= minRepage {
		if err := a.pageRounds(op); err != nil {
			return "", 0, false, err
		}
	}
	for i := len(op.order) - 1; i >= 0; i-- {
		t := op.order[i]
		for _, v := range op.rounds[t] {
			k := fmt.Sprintf("%s:%d", t, v)
			if op.scanned[k] {
				continue
			}
			var tree struct {
				Leaves []struct {
					LeafID string `json:"leaf_id"`
				} `json:"leaves"`
			}
			if err := a.call(http.MethodPost, op.url+"/v1/tree", map[string]interface{}{"txid": t, "vout": v}, &tree); err != nil {
				return "", 0, false, err
			}
			op.scanned[k] = true
			for _, l := range tree.Leaves {
				if l.LeafID != "" {
					op.leafAt[strings.ToLower(l.LeafID)] = leafPos{Txid: t, Vout: v}
				}
			}
			if p, ok := op.leafAt[id]; ok {
				return p.Txid, p.Vout, true, nil
			}
		}
	}
	return "", 0, false, nil
}

// medianNow is the later of the relay's clock and the chain's median time past.
func medianNow(chain ChainView, now time.Time) int64 {
	t := now.Unix()
	if m, ok := chain.(interface{ TipMedianTime() (int64, error) }); ok && chain != nil {
		if mt, err := m.TipMedianTime(); err == nil && mt > t {
			t = mt
		}
	}
	return t
}

// DelistTime is the earliest time the relay stops listing a resting order: the
// earlier of its REFUND time less the order margin and its exit deadline (the
// first expiry of the batches it rests on, less the exit deadline), each as the
// operator states it. 0 margins fall back to six hours and three days.
func DelistTime(refund int64, firstExpiry *int64, marginSecs, deadlineSecs int64) int64 {
	if marginSecs <= 0 {
		marginSecs = 6 * 3600
	}
	if deadlineSecs <= 0 {
		deadlineSecs = 3 * 86400
	}
	t := refund - marginSecs
	if firstExpiry != nil && *firstExpiry-deadlineSecs < t {
		t = *firstExpiry - deadlineSecs
	}
	return t
}

// RestingProbe is the relay's liveness probe for orders resting in an
// operator's tree.
type RestingProbe struct {
	Ops   *ArcaRounds
	Chain ChainView
	Now   func() time.Time
}

// CheckOffer admits a resting order at what its coin holds, or says why not.
func (p RestingProbe) CheckOffer(_ context.Context, o *seqobv1.Offer) (uint64, error) {
	t := o.GetRestingLeaf()
	if t == nil {
		return 0, nil
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	op, err := p.Ops.operatorWithKey(t.GetOperatorKey())
	if err != nil {
		return 0, err
	}
	margin, deadline := op.times()
	states, err := p.Ops.orderStates(op, []string{t.GetOrderId()})
	if err != nil {
		return 0, fmt.Errorf("the operator %s cannot be asked: %v", t.GetOperatorKey(), err)
	}
	st, ok := states[t.GetOrderId()]
	if !ok {
		return 0, fmt.Errorf("the operator %s reports no resting order %s", t.GetOperatorKey(), t.GetOrderId())
	}
	if err := MatchesState(o, st); err != nil {
		return 0, err
	}
	if at := DelistTime(int64(t.GetLandedExpiry()), st.Expiry, margin, deadline); medianNow(p.Chain, now()) >= at {
		return 0, fmt.Errorf("order %s is past its delist time %d (the earlier of its REFUND time %d less the operator's order margin of %d s, and the exit deadline of the batches it rests on): the operator fills it no more",
			t.GetOrderId(), at, t.GetLandedExpiry(), orDefault(margin, 6*3600))
	}
	v, _ := strconv.ParseUint(st.Value, 10, 64)
	return v, nil
}

func orDefault(v, d int64) int64 {
	if v <= 0 {
		return d
	}
	return v
}

// MatchesState checks a resting order's offer against its operator's view of
// it: resting, at the offer's locator with exactly that value, under the
// offer's terms.
func MatchesState(o *seqobv1.Offer, st RestingState) error {
	t := o.GetRestingLeaf()
	if st.State != "resting" {
		return fmt.Errorf("order %s is %s at its operator: it takes no fill", t.GetOrderId(), st.State)
	}
	if !strings.EqualFold(st.Coin, t.GetCoin()) {
		return fmt.Errorf("the operator reports order %s resting at coin %s, not at %s, the coin the offer names", t.GetOrderId(), st.Coin, t.GetCoin())
	}
	if st.Value != strconv.FormatUint(t.GetValue(), 10) {
		return fmt.Errorf("the operator reports coin %s holding %s atoms, not the %d the offer names", st.Coin, st.Value, t.GetValue())
	}
	want := []struct{ name, got, want string }{
		{"asset_a", st.Asset, t.GetAssetA()},
		{"asset_b", st.Terms.AssetB, t.GetAssetB()},
		{"rate_num", st.Terms.RateNum, strconv.FormatUint(t.GetRateNum(), 10)},
		{"rate_den", st.Terms.RateDen, strconv.FormatUint(t.GetRateDen(), 10)},
		{"min_lot", st.Terms.MinLot, strconv.FormatUint(t.GetMinLot(), 10)},
		{"landed_maker", st.Terms.Maker, hex.EncodeToString(t.GetLandedMaker())},
		{"landed_expiry", strconv.FormatInt(st.Terms.Expiry, 10), strconv.FormatUint(uint64(t.GetLandedExpiry()), 10)},
		{"refund_key", st.Terms.RefundKey, hex.EncodeToString(t.GetRefundKey())},
	}
	for _, w := range want {
		if !strings.EqualFold(w.got, w.want) {
			return fmt.Errorf("order %s has %s %s at its operator, not the %s the offer signs", t.GetOrderId(), w.name, w.got, w.want)
		}
	}
	return nil
}

// Probes is the relay's liveness probe: the chain for covenant orders, the
// operator for orders resting in its tree; every other offer passes.
type Probes struct {
	Chain   ChainView
	Resting *RestingProbe
}

// CheckOffer implements validator.LivenessProbe.
func (p Probes) CheckOffer(ctx context.Context, o *seqobv1.Offer) (uint64, error) {
	if offer.IsResting(o) {
		if p.Resting == nil {
			return 0, fmt.Errorf("the relay follows no operator")
		}
		return p.Resting.CheckOffer(ctx, o)
	}
	return ProbeCovenant(p.Chain, o)
}

// RestingBook is the part of the order book the follower moves.
type RestingBook interface {
	SnapshotResting() []offerstore.RestingEntry
	RepointResting(k offerstore.Key, coin string, value uint64)
	HoldResting(k offerstore.Key)
	RemoveResting(k offerstore.Key, status seqobv1.OfferStatus) error
	RecordCovenantFill(f offerstore.CovenantFill, resting *seqobv1.Offer)
	UpdateCovenantFill(f offerstore.CovenantFill) bool
}

// Resting follows the orders resting in the trees of the operators the relay
// follows (see the top of this file).
type Resting struct {
	ops        *ArcaRounds
	chain      ChainView
	book       RestingBook
	log        Logger
	now        func() time.Time
	finalDepth int64

	mu      sync.Mutex
	fills   map[string]*treeFill // transfer id -> the fill
	roundOf map[string]leafPos   // order id -> the batch output its first coin rests in
}

// treeFill is a fill the follower has seen, and how far it has recorded it.
type treeFill struct {
	rec      offerstore.CovenantFill // without its chain fields
	offer    *seqobv1.Offer          // the order as listed when the fill was seen: its price
	op       *arcaOperator
	orderID  string
	recorded bool // a live (not undone) record is held
	final    bool
	seen     time.Time
}

// NewResting returns a follower of the operators ops follows, measuring fills
// on chain, moving orders in book.
func NewResting(ops *ArcaRounds, chain ChainView, book RestingBook, log Logger) *Resting {
	if log == nil {
		log = nopLogger{}
	}
	return &Resting{ops: ops, chain: chain, book: book, log: log, now: time.Now, finalDepth: DefaultFinalAnchorDepth,
		fills: map[string]*treeFill{}, roundOf: map[string]leafPos{}}
}

// SetFinalAnchorDepth sets the anchor depth at which a fill is final.
func (r *Resting) SetFinalAnchorDepth(d int64) {
	if d > 0 {
		r.finalDepth = d
	}
}

// SetNow replaces the clock (tests).
func (r *Resting) SetNow(now func() time.Time) { r.now = now }

// Run passes every interval until stop closes.
func (r *Resting) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.Once()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// Once is one pass: every listed order checked against its operator, every
// fill seen measured against the chain.
func (r *Resting) Once() {
	entries := r.book.SnapshotResting()
	byKey := map[string][]offerstore.RestingEntry{}
	for _, e := range entries {
		k := strings.ToLower(e.Offer.GetRestingLeaf().GetOperatorKey())
		byKey[k] = append(byKey[k], e)
	}
	now := medianNow(r.chain, r.now())
	for key, list := range byKey {
		ids := make([]string, 0, len(list))
		for _, e := range list {
			ids = append(ids, e.Offer.GetRestingLeaf().GetOrderId())
		}
		op, err := r.ops.operatorWithKey(key)
		var states map[string]RestingState
		if err == nil {
			states, err = r.ops.orderStates(op, ids)
		}
		if err != nil {
			for _, e := range list {
				if e.Active != 0 {
					r.log.Printf("resting %s/%s: its operator %s cannot be asked (%v): listed at 0 until it answers", e.Key.MakerPubkey, e.Key.OfferID, key, err)
				}
				r.book.HoldResting(e.Key)
			}
			continue
		}
		margin, deadline := op.times()
		for _, e := range list {
			r.follow(op, e, states, margin, deadline, now)
		}
	}
	r.measure()
}

func (r *Resting) follow(op *arcaOperator, e offerstore.RestingEntry, states map[string]RestingState, margin, deadline, now int64) {
	t := e.Offer.GetRestingLeaf()
	id := t.GetOrderId()
	st, ok := states[id]
	if !ok {
		r.log.Printf("resting %s/%s: the operator reports no order %s: delisted", e.Key.MakerPubkey, e.Key.OfferID, id)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED)
		return
	}
	r.noteFills(op, e, st)
	switch st.State {
	case "resting":
	case "pending":
		// The coin it rests at is not live yet: nothing fills it now.
		r.book.HoldResting(e.Key)
		return
	case "filled":
		// The coin it rested at is spent by a fill. A part fill rests the order again
		// at a new coin, which the operator names once it holds it: until then the
		// order reads "filled" and is listed at 0, not delisted.
		if partFillOf(st, st.Coin) {
			r.book.HoldResting(e.Key)
			return
		}
		r.log.Printf("resting %s/%s: order %s filled whole: delisted", e.Key.MakerPubkey, e.Key.OfferID, id)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_FILLED)
		return
	case "cancelled":
		r.log.Printf("resting %s/%s: order %s cancelled by its maker: delisted", e.Key.MakerPubkey, e.Key.OfferID, id)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED)
		return
	default: // exhausted (its credit keys used) or unknown (landed, or not live)
		r.log.Printf("resting %s/%s: order %s is %s at its operator: it takes no fill: delisted", e.Key.MakerPubkey, e.Key.OfferID, id, st.State)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_EXPIRED)
		return
	}
	if at := DelistTime(int64(t.GetLandedExpiry()), st.Expiry, margin, deadline); now >= at {
		r.log.Printf("resting %s/%s: order %s reached its delist time %d: delisted", e.Key.MakerPubkey, e.Key.OfferID, id, at)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_EXPIRED)
		return
	}
	v, err := strconv.ParseUint(st.Value, 10, 64)
	if err != nil || !strings.EqualFold(st.Asset, t.GetAssetA()) {
		r.log.Printf("resting %s/%s: the operator serves order %s as %s of %s: delisted", e.Key.MakerPubkey, e.Key.OfferID, id, st.Value, st.Asset)
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_CANCELLED)
		return
	}
	if v < t.GetMinLot() {
		_ = r.book.RemoveResting(e.Key, seqobv1.OfferStatus_OFFER_STATUS_EXPIRED)
		return
	}
	if c, cv, _ := offer.RestingLocator(e.Offer); !strings.EqualFold(c, st.Coin) || cv != v || e.Active != v {
		r.log.Printf("resting %s/%s: order %s rests at coin %s holding %d", e.Key.MakerPubkey, e.Key.OfferID, id, st.Coin, v)
		r.book.RepointResting(e.Key, strings.ToLower(st.Coin), v)
	}
}

// partFillOf reports whether a fill of coin recorded at the operator is a part fill:
// the order then rests again at the fill's remainder.
func partFillOf(st RestingState, coin string) bool {
	for _, f := range st.Fills {
		if strings.EqualFold(f.Coin, coin) && f.Partial {
			return true
		}
	}
	return false
}

// noteFills takes up the signed fills of an order the follower has not seen.
// What each fill took from the coin it spent is read back from the order's
// state: the coins of an order's lineage hold, in turn, what rests now plus
// every later fill.
func (r *Resting) noteFills(op *arcaOperator, e offerstore.RestingEntry, st RestingState) {
	t := e.Offer.GetRestingLeaf()
	key := strings.ToLower(t.GetOperatorKey())
	var signed []RestingStateFill
	for _, f := range st.Fills {
		if f.Signed {
			signed = append(signed, f)
		}
	}
	var cur uint64
	if st.State == "resting" || st.State == "exhausted" {
		cur, _ = strconv.ParseUint(st.Value, 10, 64)
	}
	type amounts struct{ locked, filled, credited, rest uint64 }
	got := make([]amounts, len(signed))
	for i := len(signed) - 1; i >= 0; i-- {
		f, _ := strconv.ParseUint(signed[i].Filled, 10, 64)
		c, _ := strconv.ParseUint(signed[i].Credited, 10, 64)
		rest := cur
		if !signed[i].Partial {
			rest = 0
		}
		got[i] = amounts{locked: rest + f, filled: f, credited: c, rest: rest}
		cur = rest + f
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range signed {
		tid := strings.ToLower(f.TransferID)
		if _, seen := r.fills[tid]; seen {
			continue
		}
		a := got[i]
		rec := offerstore.CovenantFill{
			Order: e.Key, Pair: e.Offer.GetPair(), Txid: tid, Input: 0, Spent: strings.ToLower(f.Coin),
			Locked: a.locked, Filled: a.filled, Remainder: a.rest,
			Credit: offerstore.FillOutput{Vout: 0, Asset: t.GetAssetB(), Value: a.credited},
			Taker: offerstore.FillTaker{Via: offerstore.TakerOperatorTree, Operator: key,
				Note: "filled in the operator's tree by transfer " + tid + ": final when the round its order rests on is"},
			RequiredDepth: r.required(e.Offer),
		}
		if f.Partial {
			rec.Rest = &offerstore.FillOutput{Vout: 1, Asset: t.GetAssetA(), Value: a.rest}
		}
		r.fills[tid] = &treeFill{rec: rec, offer: e.Offer, op: op, orderID: t.GetOrderId(), seen: r.now()}
		r.log.Printf("resting %s/%s: order %s filled %d by transfer %s (credited %d)", e.Key.MakerPubkey, e.Key.OfferID, t.GetOrderId(), a.filled, tid, a.credited)
	}
}

func (r *Resting) required(o *seqobv1.Offer) int64 {
	d := r.finalDepth
	if m := int64(o.GetMinAnchorDepth()); m > d {
		d = m
	}
	return d
}

// keepFinal is how long a final fill is still measured: an anchor reorg of
// that depth undoes it, final or not.
const keepFinal = 24 * time.Hour

// measure records each fill seen as its order's round stands on the chain:
// confirmed while the round is in a block, final once that block is certified
// and its anchor buried the required depth, undone if the round leaves the
// active chain (recorded again if it comes back).
func (r *Resting) measure() {
	r.mu.Lock()
	list := make([]*treeFill, 0, len(r.fills))
	for _, f := range r.fills {
		list = append(list, f)
	}
	r.mu.Unlock()
	if len(list) == 0 || r.chain == nil {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seen.Before(list[j].seen) })
	a, aerr := r.chain.Anchor()
	for _, f := range list {
		pos, ok := r.roundOfOrder(f.op, f.orderID)
		if !ok {
			continue
		}
		ts, err := r.chain.TxStatus(pos.Txid)
		if err != nil {
			continue
		}
		rec := f.rec
		rec.Taker.RoundTxid, rec.Taker.BatchVouts = pos.Txid, []uint32{pos.Vout}
		if !ts.InChain {
			if f.recorded {
				rec.State, rec.Reason = offerstore.FillUndone, "the round its order rests on, "+short(pos.Txid)+", left the active chain"
				r.book.UpdateCovenantFill(rec)
				r.mu.Lock()
				f.recorded, f.final = false, false
				r.mu.Unlock()
				r.log.Printf("resting fill %s undone: %s", short(f.rec.Txid), rec.Reason)
			}
			continue
		}
		depth, certified, final := finality(ts, a, aerr == nil, rec.RequiredDepth)
		rec.Block, rec.Height, rec.Certified, rec.AnchorDepth = ts.Block, ts.Height, certified, depth
		rec.State = offerstore.FillConfirmed
		if final {
			rec.State = offerstore.FillFinal
		}
		if !f.recorded {
			r.book.RecordCovenantFill(rec, f.offer)
		} else {
			r.book.UpdateCovenantFill(rec)
		}
		r.mu.Lock()
		if final && !f.final {
			r.log.Printf("resting fill %s final: its round %s at anchor depth %d", short(f.rec.Txid), short(pos.Txid), depth)
		}
		f.recorded, f.final = true, final
		if final && r.now().Sub(f.seen) > keepFinal {
			delete(r.fills, f.rec.Txid)
		}
		r.mu.Unlock()
	}
}

// roundOfOrder is the batch output the order's first coin rests in, found once.
func (r *Resting) roundOfOrder(op *arcaOperator, id string) (leafPos, bool) {
	r.mu.Lock()
	p, ok := r.roundOf[id]
	r.mu.Unlock()
	if ok {
		return p, true
	}
	txid, vout, ok, err := r.ops.leafRound(op, id)
	if err != nil || !ok {
		return leafPos{}, false
	}
	p = leafPos{Txid: txid, Vout: vout}
	r.mu.Lock()
	r.roundOf[id] = p
	r.mu.Unlock()
	return p, true
}
