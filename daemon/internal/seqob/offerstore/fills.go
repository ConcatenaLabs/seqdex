package offerstore

import (
	"fmt"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"google.golang.org/protobuf/proto"
)

// --- covenant fill records ----------------------------------------------------
//
// A CovenantFill is the relay's record of one covenant order filled by one
// transaction, taken from that transaction's outputs as the order's FILL leaf
// reads them: the maker's credit at output 2k (k the order's input index) and,
// at 2k+1, the order resting again or, after a full fill, whatever stands
// there. It is written by the chain watcher when the fill is in a block, and
// moves on as the chain does:
//
//	confirmed -> final   when its block is certified and its anchor buried
//	                     the required depth
//	any       -> undone  when its block leaves the active chain (an anchor
//	                     reorg, a replaced round), with the reason
//
// The trade a fill makes is recorded with the fill (its txid attached) and
// retracted when the fill is undone, so the trades feed and the candles follow
// the chain. A fill made again in another block, or by another transaction, is
// a new record.

// FillState is where a covenant fill stands.
const (
	FillConfirmed = "confirmed"
	FillFinal     = "final"
	FillUndone    = "undone"
)

// Takers of a covenant fill.
const (
	// TakerChain: an ordinary on-chain fill; whoever built it took the order's
	// asset in its own outputs, which the record does not attribute.
	TakerChain = "chain"
	// TakerArcaRound: the fill is an Arca operator's round. The taker's side is
	// a leaf in the round's batch, so no output of the round pays the taker:
	// the record names the operator's key and the round.
	TakerArcaRound = "arca_round"
	// TakerClearingRound: a round order filled in a clearing round its clearer
	// signed, at the one price the round's last output publishes (Price).
	TakerClearingRound = "clearing_round"
)

// FillOutput is one output of a fill transaction as the record shows it.
type FillOutput struct {
	Vout   uint32 `json:"vout"`
	Asset  string `json:"asset"` // display hex; "" when confidential
	Value  uint64 `json:"value"` // 0 when confidential
	Script string `json:"script"`
}

// FillTaker is who took the order, as far as the chain and the operator say.
type FillTaker struct {
	Via        string   `json:"via"`
	Operator   string   `json:"operator,omitempty"`    // the Arca operator's key (x-only hex)
	RoundTxid  string   `json:"round_txid,omitempty"`  // arca_round: the fill's txid; operator_tree: the round the order's first coin rests in
	BatchVouts []uint32 `json:"batch_vouts,omitempty"` // arca_round: the batch outputs holding the takers' leaves; operator_tree: the order's
	Note       string   `json:"note,omitempty"`
	// clearing_round: the clearer's key (x-only hex) and the round's price.
	Clearer string      `json:"clearer,omitempty"`
	Price   *RoundPrice `json:"price,omitempty"`
}

// RoundPrice is the one price a clearing round publishes: n atoms of quote for
// every d atoms of base, asset ids display hex.
type RoundPrice struct {
	Base  string `json:"base"`
	Quote string `json:"quote"`
	N     uint64 `json:"n"`
	D     uint64 `json:"d"`
}

// CovenantFill is the record of one covenant order filled by one transaction.
type CovenantFill struct {
	Order     Key                `json:"-"`
	OfferID   string             `json:"offer_id"`
	Maker     string             `json:"maker_pubkey"`
	Pair      *seqobv1.AssetPair `json:"-"`
	Txid      string             `json:"txid"`  // the fill transaction; for a fill in an operator's tree (operator_tree), the operator's transfer id
	Input     uint32             `json:"input"` // k
	Spent     string             `json:"spent"` // the order's outpoint the fill spent, txid:vout
	Locked    uint64             `json:"locked"`
	Filled    uint64             `json:"filled"`
	Remainder uint64             `json:"remainder"`
	Credit    FillOutput         `json:"credit"`         // output 2k
	Rest      *FillOutput        `json:"rest,omitempty"` // output 2k+1, the order resting again
	Gap       *FillOutput        `json:"gap,omitempty"`  // output 2k+1 after a full fill, when there is one
	Taker     FillTaker          `json:"taker"`

	Block         string `json:"block"`
	Height        int64  `json:"height"`
	Certified     bool   `json:"certified"`
	AnchorDepth   int64  `json:"anchor_depth"`
	RequiredDepth int64  `json:"required_depth"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`

	Recorded time.Time `json:"recorded"`
	Updated  time.Time `json:"updated"`
}

func fillID(k Key, txid string, input uint32) string {
	return fmt.Sprintf("%s/%s/%s/%d", k.MakerPubkey, k.OfferID, txid, input)
}

// maxFillsPerPair bounds the records a pair keeps, as maxTradesPerPair bounds
// its trades.
const maxFillsPerPair = 1000

// RecordCovenantFill records a fill (state confirmed or final) and the trade it
// makes, at the resting order's price, with the fill's txid so an undo can
// retract it. A record already held for the same order, txid and input that is
// not undone is updated in place instead, and no second trade is recorded.
func (s *Store) RecordCovenantFill(f CovenantFill, resting *seqobv1.Offer) {
	now := s.now()
	f.OfferID, f.Maker = f.Order.OfferID, f.Order.MakerPubkey
	if f.Pair == nil && resting != nil {
		f.Pair = proto.Clone(resting.GetPair()).(*seqobv1.AssetPair)
	}
	if f.Pair == nil {
		return
	}
	pk := pairKey(f.Pair)
	id := fillID(f.Order, f.Txid, f.Input)
	s.mu.Lock()
	if s.fills == nil {
		s.fills = make(map[string][]*CovenantFill)
	}
	for _, have := range s.fills[pk] {
		if fillID(have.Order, have.Txid, have.Input) == id && have.State != FillUndone {
			f.Recorded = have.Recorded
			f.Updated = now
			*have = f
			s.mu.Unlock()
			return
		}
	}
	f.Recorded, f.Updated = now, now
	ring := append(s.fills[pk], &f)
	if len(ring) > maxFillsPerPair {
		ring = ring[len(ring)-maxFillsPerPair:]
	}
	s.fills[pk] = ring
	s.mu.Unlock()
	if resting != nil && f.Filled > 0 {
		s.RecordTradeVia(resting, f.Filled, f.Txid, "fill:"+id, railOfTaker(f.Taker.Via))
	}
}

// UpdateCovenantFill moves a held record on: its finality fields, and its
// state. Moving it to undone retracts the trades its txid recorded for the
// pair. Returns false when no live record matches.
func (s *Store) UpdateCovenantFill(f CovenantFill) bool {
	if f.Pair == nil {
		return false
	}
	pk := pairKey(f.Pair)
	id := fillID(f.Order, f.Txid, f.Input)
	now := s.now()
	s.mu.Lock()
	var found *CovenantFill
	for _, have := range s.fills[pk] {
		if fillID(have.Order, have.Txid, have.Input) == id && have.State != FillUndone {
			found = have
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		return false
	}
	undone := f.State == FillUndone
	recorded := found.Recorded
	f.OfferID, f.Maker = f.Order.OfferID, f.Order.MakerPubkey
	*found = f
	found.Recorded, found.Updated = recorded, now
	s.mu.Unlock()
	if undone {
		s.RetractTrade(f.Pair, f.Txid)
	}
	return true
}

// CovenantFills returns the most recent fill records of a pair (up to limit;
// 0 = all held), oldest first, undone ones included.
func (s *Store) CovenantFills(pair *seqobv1.AssetPair, limit int) []CovenantFill {
	if pair == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ring := s.fills[pairKey(pair)]
	if limit > 0 && len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	out := make([]CovenantFill, 0, len(ring))
	for _, f := range ring {
		out = append(out, *f)
	}
	return out
}

// RepointCovenant points a resting covenant order at the outpoint (txid, vout)
// holding `size` atoms: where a partial fill rested it again (reRested true),
// or, when a fill is undone, back where it rested before (reRested as it was
// then). The order's terms, and so its signed bytes, are unchanged: the
// covenant outpoint is outside them. Records no trade. No-op for a missing or
// non-covenant entry.
func (s *Store) RepointCovenant(k Key, txid string, vout uint32, size uint64, reRested bool) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	no := proto.Clone(e.Offer).(*seqobv1.Offer)
	offer.SetCovenantOutpoint(no, txid, vout)
	// One order per outpoint: if another offer already lists the remainder, the
	// rule decides which stands (outpoint.go).
	gone, err := s.claimLocked(k, no)
	if err != nil {
		ev := s.evictLocked(k)
		s.mu.Unlock()
		s.announceEvicted(ev)
		return nil
	}
	var ev *evicted
	if gone != nil {
		ev = s.evictLocked(*gone)
	}
	s.unindexLocked(k, e.Offer)
	e.Offer = no
	s.indexLocked(k, no)
	e.reRested = reRested
	if size > no.GetBaseAmount() {
		size = no.GetBaseAmount()
	}
	e.heldFill = 0
	e.ActiveAmount = size
	e.SettleTxid = ""
	e.AnchorConfs = 0
	if size >= no.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	pair := no.GetPair()
	conf := no.GetConfidential()
	st := e.orderStatus(k)
	s.mu.Unlock()
	s.announceEvicted(ev)
	// Emit created (subscribers re-learn the outpoint) then status.
	s.broadcast(Event{Type: EventCreated, Pair: pair, Confidential: conf, Offer: no})
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	return nil
}

// SetCovenantAnchorConfs records the anchor depth of a covenant order's
// settling transaction on its status, as the watcher measures it.
func (s *Store) SetCovenantAnchorConfs(k Key, depth uint32) {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) || e.AnchorConfs == depth {
		s.mu.Unlock()
		return
	}
	e.AnchorConfs = depth
	pair, conf, st := e.Offer.GetPair(), e.Offer.GetConfidential(), e.orderStatus(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
}

// RemoveCovenantRefunded removes a covenant order its maker took back through
// the REFUND leaf: EXPIRED, and no trade.
func (s *Store) RemoveCovenantRefunded(k Key, txid string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	e.SettleTxid = txid
	s.mu.Unlock()
	return s.dropWithStatus(k, seqobv1.OfferStatus_OFFER_STATUS_EXPIRED)
}

// railOfTaker is the rail a covenant fill settled on, from who took it.
func railOfTaker(via string) string {
	switch via {
	case TakerArcaRound:
		return RailCovenantRound
	case TakerClearingRound:
		return RailClearingRound
	case TakerOperatorTree:
		return RailLeafResting
	case TakerInteractive:
		return RailInteractive
	default:
		return RailOnchain
	}
}
