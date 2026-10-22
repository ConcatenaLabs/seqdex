// Package offerstore is a thread-safe, in-memory per-pair order book for SeqOB.
//
// Offers are keyed by (maker_pubkey, offer_id) — offer_id need only be unique per
// maker (review m2). The store mirrors SideSwap's book-distribution shape: a
// per-pair snapshot plus created/removed deltas pushed to subscribers, and an
// active_amount that decrements on partial fills (SideSwap OwnOrder.active_amount).
// The stale-offer state machine maps SideSwap's HistStatus onto our OfferStatus
// (OPEN/PARTIAL/FILLED/CANCELLED/EXPIRED/UTXO_INVALIDATED).
//
// The store holds only signed text; it never holds keys, funds, or a PSET.
package offerstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// Key identifies an offer within the book.
type Key struct {
	MakerPubkey string
	OfferID     string
}

// Entry is a resting offer plus its mutable lifecycle state.
type Entry struct {
	Offer        *seqobv1.Offer
	Status       seqobv1.OfferStatus
	ActiveAmount uint64 // remaining base atoms after partial fills
	SettleTxid   string
	AnchorConfs  uint32
	CreatedAt    time.Time
	heldFill     uint64 // B1: active size captured when a mempool spend zeroed it, so the confirmed fill's trade size isn't lost
	reRested     bool   // F3: this covenant order was re-rested to a remainder outpoint at least once (its funding is no longer the original), so a Ghost of the current outpoint may be a reorg-undone fill whose earlier outpoint is restored — hold it, don't invalidate.
	demoted      bool   // P3.10: this INTERACTIVE offer pushed the maker's committed capital in its offer_asset past the soft cap (likely phantom depth). Advisory; still resting/liftable.
}

// Trade is one recorded cross, captured at on-chain SETTLEMENT: the chain watcher records a
// covenant fill with its fill record (RecordCovenantFill) once it is in a block, and retracts it
// when the fill is undone; the interactive-lift SettleAck records its fill. (The matcher no longer
// records at match.) Feeds last_price, the trades feed, and candles. Price is
// quote-per-base, the same convention as displayPrice / best_bid|ask.
type Trade struct {
	TS    time.Time
	Price float64 // quote per base (matches best_bid/best_ask units)
	Size  uint64  // base atoms filled
	// Txid is the settling tx this trade was recorded for (empty for a match-time or
	// txid-less record). It lets a Bitcoin-driven reorg RETRACT the phantom trade
	// (RetractTrade) so a reorged-away fill no longer leaves a stale last_price (P3.9).
	Txid string
}

const maxTradesPerPair = 1000 // ring cap per market; oldest dropped

// tradeRec is one line of the durable JSONL trade log: the pairKey plus the trade, so
// last_price/trades/candles can be replayed after a relay restart (the book itself is ephemeral).
// A record with RetractTxid set is a RETRACTION (P3.9): on replay it removes any already-
// replayed trades with that txid for that pair, so a reorged-away fill does not resurrect
// after a restart. The append-only log stays append-only (no rewrite on a reorg).
type tradeRec struct {
	PK          string    `json:"pk"`
	TS          time.Time `json:"ts"`
	Price       float64   `json:"price"`
	Size        uint64    `json:"size"`
	Txid        string    `json:"txid,omitempty"`
	RetractTxid string    `json:"retract_txid,omitempty"`
}

// EventType is the kind of book delta.
type EventType int

const (
	// EventCreated: a new (or edited) offer entered the book.
	EventCreated EventType = iota
	// EventRemoved: an offer left the book (cancel/expire/fill/invalidated).
	EventRemoved
	// EventStatus: an offer's status/active_amount changed in place.
	EventStatus
)

// Event is a single book delta delivered to subscribers.
type Event struct {
	Type EventType
	Pair *seqobv1.AssetPair
	// Confidential is the delta's book NAMESPACE (the resting offer's confidential flag),
	// carried here because EventRemoved/EventStatus reference an offer by key only (no Offer
	// pointer) yet must still be filtered to their namespace. Subscribers see only deltas whose
	// Confidential matches their own — the same disjoint transparent/blinded split SnapshotPair
	// and the matcher enforce.
	Confidential bool
	Offer        *seqobv1.Offer       // set for EventCreated
	Ref          *seqobv1.OfferRef    // set for EventRemoved
	Status       *seqobv1.OrderStatus // set for EventStatus
	// Seq is the monotonic per-subscription sequence number of this delta (P3.8),
	// stamped at delivery time. A subscriber that sees Seq skip a value missed a
	// dropped delta and must re-snapshot. 0 until stamped.
	Seq uint64
}

type subscriber struct {
	pair         string // pairKey filter, or "" for all pairs
	confidential bool   // book namespace this subscriber is bound to (only matching-namespace deltas are delivered)
	ch           chan Event
	// seq is the per-subscription delta counter (P3.8). It is incremented for EVERY
	// event ROUTED to this subscriber — delivered OR dropped — so a dropped delta
	// leaves a hole in the delivered sequence that the subscriber (and the WS
	// forwarder) detect as a gap. Atomic: concurrent broadcasts may route here.
	seq atomic.Uint64
}

// Store is the in-memory order book.
type Store struct {
	mu             sync.RWMutex
	entries        map[Key]*Entry
	pairs          map[string]map[Key]bool // pairKey -> set of keys
	covAt          map[string]Key          // covenant outpoint "txid:vout" -> the one key listed there (outpoint.go)
	subs           map[int]*subscriber
	nextSub        int
	cancelNonce    map[Key]uint64 // highest cancel nonce seen per key (replay defense, survives removal)
	settledNonce   map[Key]uint64 // highest settled-trade nonce seen per key (replay defense; offer_id is reused across -requote)
	subBuf         int
	now            func() time.Time
	trades         map[string][]Trade         // pairKey -> recent trades ring (newest last), lazy
	fills          map[string][]*CovenantFill // pairKey -> recent covenant fill records (newest last), lazy
	interactive    []*InteractiveWatch        // interactive fills the chain watcher follows to finality, oldest first, lazy
	poolMoves      map[string]PoolMove        // pool key/txid -> the move the pool follower recorded (pool.go), lazy
	tradeLog       *os.File                   // B1: append-only JSONL sink so trade history survives a relay restart (nil = disabled)
	interactiveCap uint64                     // P3.10: per-maker soft cap on committed capital per offer_asset (0 = disabled). An interactive offer that pushes the maker's cumulative offer_amount in an asset past this is demoted.
}

// RecordTrade appends one executed cross for the resting offer's pair at its execution
// price (displayPrice of the resting order). Called by the matcher when a fill is produced.
// Best-effort + non-blocking: bad inputs are ignored, never an error on the match path.
func (s *Store) RecordTrade(restingOffer *seqobv1.Offer, sizeBase uint64) {
	s.RecordTradeTx(restingOffer, sizeBase, "")
}

// RecordTradeTx is RecordTrade with the SETTLING TXID attached, so a later Bitcoin
// reorg can retract this exact trade (RetractTrade). Callers with a settle txid (the
// interactive-lift SettleAck, cross/LN SettledTrade) pass it; a txid-less record ("")
// behaves exactly like RecordTrade.
func (s *Store) RecordTradeTx(restingOffer *seqobv1.Offer, sizeBase uint64, txid string) {
	if restingOffer == nil || sizeBase == 0 {
		return
	}
	price, _ := displayPrice(restingOffer)
	pair := restingOffer.GetPair()
	if price <= 0 || pair == nil {
		return
	}
	pk := pairKey(pair)
	ts := s.now()
	s.mu.Lock()
	if s.trades == nil {
		s.trades = make(map[string][]Trade)
	}
	t := append(s.trades[pk], Trade{TS: ts, Price: price, Size: sizeBase, Txid: txid})
	if len(t) > maxTradesPerPair {
		t = t[len(t)-maxTradesPerPair:]
	}
	s.trades[pk] = t
	logf := s.tradeLog
	s.mu.Unlock()
	// Durably append AFTER releasing the lock (B-4): a slow/networked disk must never serialize the whole
	// book behind the trade-log write on the match/settle hot path. os.File appends (O_APPEND) are atomic
	// and Write is goroutine-safe, so concurrent appends are fine. Best-effort — a log error never breaks.
	if logf != nil {
		if b, err := json.Marshal(tradeRec{PK: pk, TS: ts, Price: price, Size: sizeBase, Txid: txid}); err == nil {
			_, _ = logf.Write(append(b, '\n'))
		}
	}
}

// RetractTrade removes every recorded trade for pair whose settling tx is txid, then
// durably appends a RETRACTION to the JSONL so the trade does not resurrect on a
// restart replay (P3.9). It is the trade-feed half of reorg-undo: when a Bitcoin
// reorg orphans a settling tx, the fill un-happened, so the phantom trade print, its
// contribution to last_price (the newest ring entry), and its candle volume must go.
// The append-only log stays append-only (no rewrite). Returns the number removed.
// No-op for an empty txid (a txid-less trade cannot be targeted).
func (s *Store) RetractTrade(pair *seqobv1.AssetPair, txid string) int {
	if pair == nil || txid == "" {
		return 0
	}
	pk := pairKey(pair)
	ts := s.now()
	s.mu.Lock()
	ring := s.trades[pk]
	kept := ring[:0:0]
	removed := 0
	for _, tr := range ring {
		if tr.Txid == txid {
			removed++
			continue
		}
		kept = append(kept, tr)
	}
	if removed > 0 {
		s.trades[pk] = kept
	}
	logf := s.tradeLog
	s.mu.Unlock()
	if removed > 0 && logf != nil {
		if b, err := json.Marshal(tradeRec{PK: pk, TS: ts, RetractTxid: txid}); err == nil {
			_, _ = logf.Write(append(b, '\n'))
		}
	}
	return removed
}

// EnableTradeLog makes the trade history durable: it replays an existing append-only JSONL log into
// the in-memory rings, then keeps appending new trades to it. The book itself is ephemeral, but
// last_price/trades/candles should survive a relay restart. Best-effort; call once at startup.
func (s *Store) EnableTradeLog(path string) error {
	if f, err := os.Open(path); err == nil {
		s.mu.Lock()
		if s.trades == nil {
			s.trades = make(map[string][]Trade)
		}
		// bufio.Reader (not Scanner): a single over-long/corrupt line must not silently END the replay
		// (Scanner returns false past its cap) — read line by line, skip a bad one, keep going (B-5).
		rd := bufio.NewReaderSize(f, 1<<20)
		for {
			line, rerr := rd.ReadBytes('\n')
			if len(line) > 0 {
				var r tradeRec
				if json.Unmarshal(line, &r) == nil && r.PK != "" {
					if r.RetractTxid != "" {
						// A reorg retraction (P3.9): drop any already-replayed trades with this
						// txid so a reorged-away fill does not resurrect after a restart.
						ring := s.trades[r.PK]
						kept := ring[:0:0]
						for _, tr := range ring {
							if tr.Txid != r.RetractTxid {
								kept = append(kept, tr)
							}
						}
						s.trades[r.PK] = kept
					} else if r.Price > 0 && r.Size != 0 {
						t := append(s.trades[r.PK], Trade{TS: r.TS, Price: r.Price, Size: r.Size, Txid: r.Txid})
						if len(t) > maxTradesPerPair {
							t = t[len(t)-maxTradesPerPair:]
						}
						s.trades[r.PK] = t
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		s.mu.Unlock()
		_ = f.Close()
		// Compact to just the retained tail (<= maxTradesPerPair per pair) so the log can't grow without
		// bound and each restart's replay stays O(retained), not O(all history ever) (B-5).
		s.compactTradeLog(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.tradeLog = f
	s.mu.Unlock()
	return nil
}

// compactTradeLog rewrites the log to exactly the retained in-memory trades (best-effort; startup only,
// before any concurrent RecordTrade writer is armed).
func (s *Store) compactTradeLog(path string) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	s.mu.RLock()
	for pk, ring := range s.trades {
		for _, tr := range ring {
			if b, mErr := json.Marshal(tradeRec{PK: pk, TS: tr.TS, Price: tr.Price, Size: tr.Size, Txid: tr.Txid}); mErr == nil {
				_, _ = f.Write(append(b, '\n'))
			}
		}
	}
	s.mu.RUnlock()
	if f.Close() == nil {
		_ = os.Rename(tmp, path)
	} else {
		_ = os.Remove(tmp)
	}
}

// TradesFor returns the most recent trades for a pair (up to limit; 0 = all held), newest last.
func (s *Store) TradesFor(pair *seqobv1.AssetPair, limit int) []Trade {
	if pair == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.trades[pairKey(pair)]
	if limit > 0 && len(t) > limit {
		t = t[len(t)-limit:]
	}
	out := make([]Trade, len(t))
	copy(out, t)
	return out
}

// New returns an empty store. If now is nil, time.Now is used.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		entries:      make(map[Key]*Entry),
		pairs:        make(map[string]map[Key]bool),
		covAt:        make(map[string]Key),
		subs:         make(map[int]*subscriber),
		cancelNonce:  make(map[Key]uint64),
		settledNonce: make(map[Key]uint64),
		subBuf:       256,
		now:          now,
	}
}

// SetInteractiveCap sets the P3.10 soft per-maker committed-capital cap: an
// INTERACTIVE offer whose offer_amount pushes the maker's cumulative committed
// capital in that offer_asset past n is DEMOTED (flagged as likely phantom depth,
// but still resting/liftable). 0 (the default) disables demotion. Covenant offers
// are funded on-chain and never demoted. Set at startup.
func (s *Store) SetInteractiveCap(n uint64) {
	s.mu.Lock()
	s.interactiveCap = n
	s.mu.Unlock()
}

// CommittedInteractiveCapital returns the maker's cumulative committed capital in
// offerAsset across its LIVE interactive (non-covenant) offers — the sum of their
// offer_amounts. It is the soft-accounting quantity the relay uses to spot phantom
// depth (many resting offers drawing on the same coins), exported so a UI/monitor can
// read it. Covenant offers are excluded (funded on-chain, no oversell).
func (s *Store) CommittedInteractiveCapital(makerPubkey, offerAsset string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.committedInteractiveLocked(makerPubkey, offerAsset)
}

// committedInteractiveLocked sums live interactive offer_amounts for (maker, asset).
// Caller holds s.mu.
func (s *Store) committedInteractiveLocked(makerPubkey, offerAsset string) uint64 {
	var total uint64
	for k, e := range s.entries {
		if k.MakerPubkey != makerPubkey || offer.FilledWithoutMaker(e.Offer) {
			continue
		}
		if e.Offer.GetOfferAsset() != offerAsset {
			continue
		}
		total += e.Offer.GetOfferAmount()
	}
	return total
}

// SetSubBuf sets the per-subscriber delta-channel capacity for subscribers created
// AFTER this call (the default is 256). Lower it to make a buffer overflow — and the
// resulting delta-sequence gap (P3.8) — deterministic in tests, or raise it for a
// bursty deployment. Call before Subscribe.
func (s *Store) SetSubBuf(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.subBuf = n
	s.mu.Unlock()
}

func pairKey(p *seqobv1.AssetPair) string {
	return p.GetBaseAsset() + "/" + p.GetQuoteAsset()
}

func keyOf(o *seqobv1.Offer) Key {
	return Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
}

// ErrExists is returned by Submit when the (maker_pubkey, offer_id) key is already
// resting. Callers refreshing a still-resting DURABLE offer (a reconnecting cross /
// lightning settlement agent re-posts a re-signed copy) should Edit instead of erroring.
var ErrExists = errors.New("offer already resting")

// Submit verifies the offer's signature and inserts it as a new OPEN order.
// It is an error (ErrExists) to Submit a key that already exists (use Edit to re-sign).
func (s *Store) Submit(o *seqobv1.Offer) (Key, error) { return s.SubmitAt(o, 0) }

// listing is the entry state for an offer listed at listed base atoms: 0 (or
// anything not below the signed base_amount) lists it at its signed size, OPEN;
// less lists it PARTIAL at that size. Only a covenant order is listed below its
// signed size, at what its outpoint holds (the validator's chain probe): its
// outpoint is then a remainder of the order, so it is marked re-rested, and a
// reorg that takes the outpoint away holds the order instead of dropping it.
func listing(o *seqobv1.Offer, listed uint64) (uint64, seqobv1.OfferStatus, bool) {
	if listed == 0 || listed >= o.GetBaseAmount() || !offer.FilledWithoutMaker(o) {
		return o.GetBaseAmount(), seqobv1.OfferStatus_OFFER_STATUS_OPEN, false
	}
	return listed, seqobv1.OfferStatus_OFFER_STATUS_PARTIAL, true
}

// SubmitAt is Submit for an order listed at listed base atoms (see listing):
// the relay passes the size the validator admitted it at.
func (s *Store) SubmitAt(o *seqobv1.Offer, listed uint64) (Key, error) {
	if err := offer.VerifyOffer(o); err != nil {
		return Key{}, fmt.Errorf("offer signature: %w", err)
	}
	if o.GetPair() == nil {
		return Key{}, fmt.Errorf("offer missing pair")
	}
	k := keyOf(o)
	s.mu.Lock()
	if _, ok := s.entries[k]; ok {
		s.mu.Unlock()
		return Key{}, fmt.Errorf("offer %s/%s: %w", k.MakerPubkey, k.OfferID, ErrExists)
	}
	gone, err := s.claimLocked(k, o)
	if err != nil {
		s.mu.Unlock()
		return Key{}, err
	}
	var ev *evicted
	if gone != nil {
		ev = s.evictLocked(*gone)
	}
	active, status, reRested := listing(o, listed)
	e := &Entry{
		Offer:        o,
		Status:       status,
		ActiveAmount: active,
		CreatedAt:    s.now(),
		reRested:     reRested,
	}
	s.put(k, e)
	// P3.10 soft oversell: for an INTERACTIVE offer, once the maker's cumulative
	// committed capital in this offer_asset (now including this offer) exceeds the soft
	// cap, flag this marginal offer as demoted (likely phantom depth). Computed after
	// put() so committedInteractiveLocked includes this offer. Covenant offers exempt.
	if s.interactiveCap > 0 && !offer.FilledWithoutMaker(o) {
		if s.committedInteractiveLocked(k.MakerPubkey, o.GetOfferAsset()) > s.interactiveCap {
			e.demoted = true
		}
	}
	st := e.orderStatus(k)
	s.mu.Unlock()
	s.announceEvicted(ev)
	s.broadcastListed(o, st)
	return k, nil
}

// broadcastListed announces an order entering the book: created, and when it
// is listed below its signed size (a covenant remainder) its status too, so a
// subscriber learns the size it can be filled for, as RepointCovenant tells it.
func (s *Store) broadcastListed(o *seqobv1.Offer, st *seqobv1.OrderStatus) {
	s.broadcast(Event{Type: EventCreated, Pair: o.GetPair(), Confidential: o.GetConfidential(), Offer: o})
	if st.GetActiveAmount() < o.GetBaseAmount() {
		s.broadcast(Event{Type: EventStatus, Pair: o.GetPair(), Confidential: o.GetConfidential(), Status: st})
	}
}

// Edit replaces an existing offer (re-signed, same offer_id) with new terms. The
// new offer's created_at_unix must not move backwards. active_amount resets to
// the new base_amount.
func (s *Store) Edit(o *seqobv1.Offer) error { return s.EditAt(o, 0) }

// EditAt is Edit for an order listed at listed base atoms (see listing): a
// covenant order re-signed while it rests at its remainder keeps the size its
// outpoint holds.
func (s *Store) EditAt(o *seqobv1.Offer, listed uint64) error {
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("offer signature: %w", err)
	}
	k := keyOf(o)
	s.mu.Lock()
	old, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	if o.GetCreatedAtUnix() < old.Offer.GetCreatedAtUnix() {
		s.mu.Unlock()
		return fmt.Errorf("edit created_at_unix moved backwards")
	}
	gone, err := s.claimLocked(k, o)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	var ev *evicted
	if gone != nil {
		ev = s.evictLocked(*gone)
	}
	// Remove from the old pair bucket in case the pair changed, then re-insert.
	s.remove(k)
	active, status, reRested := listing(o, listed)
	e := &Entry{
		Offer:        o,
		Status:       status,
		ActiveAmount: active,
		CreatedAt:    s.now(),
		reRested:     reRested,
	}
	s.put(k, e)
	st := e.orderStatus(k)
	s.mu.Unlock()
	// Emit a removed+created pair so subscribers converge regardless of pair change. Each
	// delta carries its OWN offer's namespace: a namespace change (re-signed transparent<->
	// confidential) must remove from the old book and create in the new one.
	s.announceEvicted(ev)
	s.broadcast(Event{Type: EventRemoved, Pair: old.Offer.GetPair(), Confidential: old.Offer.GetConfidential(), Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	s.broadcastListed(o, st)
	return nil
}

// Cancel verifies a signed cancel and removes the offer. The nonce must exceed
// the highest nonce previously seen for this key, defeating replay of an old
// cancel against a re-posted same offer_id.
func (s *Store) Cancel(c *seqobv1.OfferCancel) error {
	if err := offer.VerifyCancel(c); err != nil {
		return fmt.Errorf("cancel signature: %w", err)
	}
	k := Key{MakerPubkey: c.GetMakerPubkey(), OfferID: c.GetOfferId()}
	s.mu.Lock()
	if c.GetNonce() <= s.cancelNonce[k] {
		s.mu.Unlock()
		return fmt.Errorf("stale cancel nonce")
	}
	e, ok := s.entries[k]
	// The nonce high-water mark is in memory, so it does not survive a relay restart
	// while makers re-post the same offer ids. A captured cancel that predates the
	// resting offer's post time is a replay from an earlier life of that id, whatever
	// the mark says. Clock nonces (the Go maker's nanoseconds, the wallet's seconds)
	// can be placed in time; an opaque counter cannot and is not judged here.
	if ok && nonceBefore(c.GetNonce(), e.Offer.GetCreatedAtUnix()) {
		s.mu.Unlock()
		return fmt.Errorf("cancel nonce predates the resting offer")
	}
	// The high-water mark survives removal (a replayed cancel must stay stale after the
	// offer is gone), but it is only ever CREATED for a key that rests or once rested:
	// a valid signature over any (pubkey, offer_id) is free to mint, and recording every
	// one would let a single key grow the map without bound.
	if _, seen := s.cancelNonce[k]; ok || seen {
		s.cancelNonce[k] = c.GetNonce()
	}
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	s.remove(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Confidential: conf, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// Get returns a copy-free snapshot of the entry for k.
func (s *Store) Get(k Key) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[k]
	return e, ok
}

// RestingMakerSig returns the maker_sig of the offer currently resting under
// (makerPubkey, offerID), or ok=false if none rests. It lets the validator tell
// a byte-identical replay (same signed content => same signature) from a genuine
// new/edited offer WITHOUT charging the per-maker_pubkey rate budget (the relay
// replay-griefing defense; see internal/seqob/validator). The maker_sig is a
// deterministic, collision-resistant function of the entire signed offer, so an
// equal signature means equal signed content.
func (s *Store) RestingMakerSig(makerPubkey, offerID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[Key{MakerPubkey: makerPubkey, OfferID: offerID}]
	if !ok {
		return nil, false
	}
	return e.Offer.GetMakerSig(), true
}

// OrderStatusOf returns the live OrderStatus for a resting key, if present. Used
// to re-ack a no-op replay without re-inserting the offer.
func (s *Store) OrderStatusOf(k Key) (*seqobv1.OrderStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[k]
	if !ok {
		return nil, false
	}
	return e.orderStatus(k), true
}

// SnapshotPair returns all live offers for a pair in the requested book
// namespace: confidential=false is the transparent (unblinded) book, confidential=true
// is the separate blinded book. The two namespaces are DISJOINT views over the pair
// so a confidential offer never appears in the transparent book and vice-versa
// (Confidential-book requirement 1); the matcher additionally guarantees they never
// cross.
func (s *Store) SnapshotPair(p *seqobv1.AssetPair, confidential bool) []*seqobv1.Offer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.pairs[pairKey(p)]
	out := make([]*seqobv1.Offer, 0, len(set))
	for k := range set {
		if e, ok := s.entries[k]; ok && e.Offer.GetConfidential() == confidential {
			out = append(out, e.Offer)
		}
	}
	return out
}

// SnapshotPairEntries returns value copies of every live entry for a pair in the
// requested book namespace (confidential true/false), for the matcher's
// price-time-priority walk. Copies (not the live *Entry) so the matcher reads a
// consistent snapshot; the enclosed *Offer is immutable once signed, so sharing its
// pointer is safe. Filtering by confidential here keeps the blinded and unblinded
// books as disjoint order sets.
func (s *Store) SnapshotPairEntries(p *seqobv1.AssetPair, confidential bool) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.pairs[pairKey(p)]
	out := make([]Entry, 0, len(set))
	for k := range set {
		if e, ok := s.entries[k]; ok && e.Offer.GetConfidential() == confidential {
			out = append(out, *e)
		}
	}
	return out
}

// SnapshotMaker returns all live offers posted by a given maker_pubkey.
func (s *Store) SnapshotMaker(makerPubkey string) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0)
	for k, e := range s.entries {
		if k.MakerPubkey == makerPubkey {
			out = append(out, e)
		}
	}
	return out
}

// Markets summarizes every pair currently in the book.
func (s *Store) Markets() []*seqobv1.Market { return s.MarketsFiltered(nil) }

// MarketsFiltered summarizes the book as Markets does, counting only the offers
// keep accepts (nil keeps all). The relay passes its liftability filter so the
// summary agrees with the orderbook it serves: a pair whose offers are all
// unliftable ghosts must not advertise a best price and an order count that the
// book itself comes back empty for.
//
// The transparent and blinded books of one pair are DISJOINT namespaces
// (SnapshotPair, the matcher, the delta stream all split on the offer's
// confidential flag), so they are summarized as two markets, and the emitted
// pair carries the namespace in AssetPair.confidential. A client selecting the
// blinded book by that flag would otherwise never see it: makers post pair
// structs with the flag unset and the flag on the offer.
func (s *Store) MarketsFiltered(keep func(*seqobv1.Offer) bool) []*seqobv1.Market {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type agg struct {
		pair             *seqobv1.AssetPair
		bestBid, bestAsk float64
		n                uint64
	}
	out := make([]*seqobv1.Market, 0, len(s.pairs))
	for pk, set := range s.pairs {
		if len(set) == 0 {
			continue
		}
		var books [2]*agg // [0] transparent, [1] confidential
		for k := range set {
			e, ok := s.entries[k]
			if !ok {
				continue
			}
			if e.ActiveAmount == 0 {
				continue // B-7: a held/zeroed entry isn't fillable — don't count it or advertise its price
			}
			if keep != nil && !keep(e.Offer) {
				continue
			}
			ns := 0
			if e.Offer.GetConfidential() {
				ns = 1
			}
			a := books[ns]
			if a == nil {
				pair := proto.Clone(e.Offer.GetPair()).(*seqobv1.AssetPair)
				pair.Confidential = e.Offer.GetConfidential()
				a = &agg{pair: pair}
				books[ns] = a
			}
			a.n++
			price, isBid := displayPrice(e.Offer)
			if price <= 0 {
				continue
			}
			if isBid {
				if price > a.bestBid {
					a.bestBid = price
				}
			} else {
				if a.bestAsk == 0 || price < a.bestAsk {
					a.bestAsk = price
				}
			}
		}
		var last float64
		if t := s.trades[pk]; len(t) > 0 {
			last = t[len(t)-1].Price // newest recorded cross
		}
		for _, a := range books {
			if a == nil || a.n == 0 {
				// Every entry in this namespace is held/zeroed or filtered out: there is no
				// fillable market to advertise. Skip rather than emit a Market with a nil
				// Pair (blank ticker + stale last price) that a UI renders as garbage.
				continue
			}
			out = append(out, &seqobv1.Market{
				Pair:      a.pair,
				BestBid:   a.bestBid,
				BestAsk:   a.bestAsk,
				LastPrice: last,
				NOrders:   a.n,
			})
		}
	}
	return out
}

// displayPrice returns the quote-per-base price and whether the offer is a bid
// (maker buying base) versus an ask (maker selling base). The authoritative
// ratio is want/offer (integers); this float is for display/sort only.
func displayPrice(o *seqobv1.Offer) (float64, bool) {
	base := o.GetBaseAmount()
	if base == 0 {
		return 0, false
	}
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		// give base (offer_amount), want quote (want_amount): ask.
		if o.GetOfferAmount() == 0 {
			return 0, false
		}
		return float64(o.GetWantAmount()) / float64(o.GetOfferAmount()), false
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		// give quote (offer_amount), want base (want_amount): bid.
		if o.GetWantAmount() == 0 {
			return 0, true
		}
		return float64(o.GetOfferAmount()) / float64(o.GetWantAmount()), true
	default:
		return 0, false
	}
}

// ApplyPartialFill decrements an offer's active_amount by filledBase and records
// the settling txid. Status becomes PARTIAL while base remains, else FILLED.
//
// Anchor-aware finality (Principle 1 / review B3): the maker dial min_anchor_depth
// defaults to 0 (0-conf tolerant; no enforced floor — policy override). FILLED is
// surfaced honestly: at 0-conf it is reached immediately, but a later anchor
// orphan can Reopen the order. Callers raise anchorConfs as the settling tx gains
// Bitcoin-anchor confirmations.
func (s *Store) ApplyPartialFill(k Key, filledBase uint64, settleTxid string, anchorConfs uint32) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	if filledBase > e.ActiveAmount {
		filledBase = e.ActiveAmount
	}
	e.ActiveAmount -= filledBase
	e.SettleTxid = settleTxid
	e.AnchorConfs = anchorConfs
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	if e.ActiveAmount == 0 && anchorConfs >= e.Offer.GetMinAnchorDepth() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_FILLED
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	st := e.orderStatus(k)
	filled := e.ActiveAmount == 0 && e.Status == seqobv1.OfferStatus_OFFER_STATUS_FILLED
	if filled {
		s.remove(k)
	}
	s.mu.Unlock()

	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	if filled {
		s.broadcast(Event{Type: EventRemoved, Pair: pair, Confidential: conf, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	}
	return nil
}

// RecordSettledFill records a settled CROSS-CHAIN / LIGHTNING fill reported by the
// offer's maker (the SettledTrade wire message; the caller has already verified the
// maker signature and that maker_pubkey == k.MakerPubkey). It is the durable-settlement
// analogue of the interactive SettleAck path — record the trade at the RESTING order's
// price, then decrement/finalize the order (advancing anchorConfs so a min_anchor_depth>0
// offer can reach FILLED) — but keyed to the offer instead of a live session, because a
// cross/LN swap can finish after the courier session was swept or the maker reconnected.
//
// The nonce must strictly exceed the highest settled-fill nonce seen for this key: -requote
// re-posts the SAME offer_id, so a stale settled-trade must not decrement the fresh quote.
// If the offer already left the book (a prior fill removed it), this is a no-op returning nil
// (there is nothing to price or decrement); the nonce is still advanced so the stale copy
// cannot later apply. Reorg retraction of the recorded trade is handled separately.
func (s *Store) RecordSettledFill(k Key, fillBase uint64, settleTxid string, anchorConfs uint32, nonce uint64) error {
	// A settled-trade record without a nonce cannot be told from a replay of
	// itself; every signer in use (the Go maker) sends a clock nonce, so a zero is
	// refused rather than admitted unguarded.
	if nonce == 0 {
		return fmt.Errorf("settled-trade nonce must be non-zero")
	}
	s.mu.Lock()
	if nonce <= s.settledNonce[k] {
		s.mu.Unlock()
		return fmt.Errorf("stale settled-trade nonce")
	}
	e, ok := s.entries[k]
	if _, seen := s.settledNonce[k]; ok || seen {
		s.settledNonce[k] = nonce // same growth rule as the cancel nonce: only for keys that ever rested
	}
	if !ok {
		s.mu.Unlock()
		return nil // offer already gone (a prior fill removed it): nothing to price or decrement
	}
	fill := fillBase
	if fill > e.ActiveAmount {
		fill = e.ActiveAmount // never decrement below the resting size (it shrank via another fill)
	}
	restingOffer := e.Offer // capture under the lock: ApplyPartialFill may remove the entry at full fill
	s.mu.Unlock()
	if fill == 0 {
		return nil
	}
	// Record BEFORE ApplyPartialFill (which may remove the entry, after which the pair/price is
	// no longer fetchable). Both re-lock internally; this mirrors wsSettleAck's ordering.
	s.RecordTrade(restingOffer, fill)
	return s.ApplyPartialFill(k, fill, settleTxid, anchorConfs)
}

// MarkInvalidated drops an offer because the maker could not co-sign (coins
// moved). Maps to SideSwap HistStatus UTXO_INVALIDATED; no on-chain cost.
func (s *Store) MarkInvalidated(k Key) error {
	return s.dropWithStatus(k, seqobv1.OfferStatus_OFFER_STATUS_UTXO_INVALIDATED)
}

// RetireIncoming removes an order the RELAY itself retires immediately after
// accepting it, per the order's time-in-force (an IOC remainder, or a FOK /
// POST_ONLY that must not rest). Unlike Cancel it needs no maker-signed nonce (the
// relay is enforcing the maker's own stated option on its own just-accepted order),
// and unlike MarkInvalidated it carries the caller's terminal status (CANCELLED).
// No-op (nil) if the entry is already gone (e.g. a concurrent fill removed it).
func (s *Store) RetireIncoming(k Key, status seqobv1.OfferStatus) error {
	s.mu.Lock()
	if _, ok := s.entries[k]; !ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return s.dropWithStatus(k, status)
}

func (s *Store) dropWithStatus(k Key, status seqobv1.OfferStatus) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	e.Status = status
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	st := e.orderStatus(k)
	s.remove(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Confidential: conf, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// Reopen restores a (possibly already-removed) offer to the book as OPEN/PARTIAL.
// Used by the session reorg hook: when a settling tx's Bitcoin anchor is later
// orphaned (Principle 1), the swap un-happens and the order must come back. Also
// used when a lift session aborts before settlement.
func (s *Store) Reopen(o *seqobv1.Offer, restoreActive uint64) error {
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("offer signature: %w", err)
	}
	k := keyOf(o)
	if restoreActive == 0 || restoreActive > o.GetBaseAmount() {
		restoreActive = o.GetBaseAmount()
	}
	status := seqobv1.OfferStatus_OFFER_STATUS_OPEN
	if restoreActive < o.GetBaseAmount() {
		status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	s.mu.Lock()
	// Un-happening a fill restores the order the maker had resting at the time; it must
	// not resurrect one the maker has since withdrawn. (An expired copy needs no guard:
	// the sweeper removes it again within its interval.)
	if cancelledSince(s.cancelNonce[k], o.GetCreatedAtUnix()) {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s was cancelled by its maker; not reopened", k.MakerPubkey, k.OfferID)
	}
	gone, err := s.claimLocked(k, o)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not reopened: %w", k.MakerPubkey, k.OfferID, err)
	}
	var ev *evicted
	if gone != nil {
		ev = s.evictLocked(*gone)
	}
	e := &Entry{Offer: o, Status: status, ActiveAmount: restoreActive, CreatedAt: s.now()}
	s.put(k, e)
	s.mu.Unlock()
	s.announceEvicted(ev)
	s.broadcast(Event{Type: EventCreated, Pair: o.GetPair(), Confidential: o.GetConfidential(), Offer: o})
	return nil
}

// cancelledSince reports whether a cancel with nonce n was signed after an offer
// created at createdAtUnix. Makers use a clock as the nonce — the Go maker in
// nanoseconds, the JS wallet in seconds — so the unit is inferred from magnitude;
// an opaque small nonce cannot be placed in time and is treated as not-after.
func cancelledSince(n, createdAtUnix uint64) bool {
	if n == 0 || createdAtUnix == 0 {
		return false
	}
	const nanosFloor = 1e15 // any unix-nanosecond clock this century; far above any unix-second one
	if n >= nanosFloor {
		return n >= createdAtUnix*1e9
	}
	return n >= createdAtUnix && n >= 1e9 // a second-resolution clock, not a counter
}

// nonceBefore is the converse of cancelledSince: a clock nonce that lies BEFORE the
// offer's post time. Opaque counters (below any unix-second clock) are never "before".
func nonceBefore(n, createdAtUnix uint64) bool {
	if n == 0 || createdAtUnix == 0 || n < 1e9 {
		return false
	}
	const nanosFloor = 1e15
	if n >= nanosFloor {
		return n < createdAtUnix*1e9
	}
	return n < createdAtUnix
}

// SweepExpired removes offers whose expires_at_unix has passed, emitting EXPIRED
// status then removal. Returns the number swept.
func (s *Store) SweepExpired() int {
	now := uint64(s.now().Unix())
	s.mu.Lock()
	expired := make([]Key, 0)
	for k, e := range s.entries {
		exp := e.Offer.GetExpiresAtUnix()
		if exp != 0 && exp <= now {
			expired = append(expired, k)
		}
	}
	type ev struct {
		pair *seqobv1.AssetPair
		conf bool
		st   *seqobv1.OrderStatus
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(expired))
	for _, k := range expired {
		e := s.entries[k]
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_EXPIRED
		evs = append(evs, ev{pair: e.Offer.GetPair(), conf: e.Offer.GetConfidential(), st: e.orderStatus(k), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventStatus, Pair: e.pair, Confidential: e.conf, Status: e.st})
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Confidential: e.conf, Ref: e.ref})
	}
	return len(expired)
}

// RemoveByMaker drops every resting INTERACTIVE SAME-CHAIN offer for makerPubkey,
// emitting a removal event per offer. Called when a maker's websocket disconnects:
// an interactive same-chain co-sign offer needs the maker ONLINE to co-sign a lift,
// so once its connection drops the offer is unliftable and must not linger as an
// un-fillable "ghost".
//
// DURABLE settlement types are the deliberate exception and are NOT evicted, because a
// lift does not need this maker's live co-sign at disconnect time:
//   - COVENANT offers: funded on-chain, fillable permissionlessly with the maker OFFLINE
//     (the whole point of the passive CLOB).
//   - CROSS-CHAIN (BTC<->asset) and LIGHTNING/sub-asset offers: serviced by the maker's
//     always-online settlement agent (seqob-maker), which reconnects and re-registers.
//     A transient WS blip must NOT evict these — that emptied the cross book (BTC vanished
//     from the DEX). Ghost-safety holds regardless: a taker's pre-lock terms handshake
//     fails fast and spends nothing if no agent answers, and the TTL sweep removes truly
//     abandoned offers. See durable-cross-offer design.
//
// Returns the number removed.
// RemoveInteractiveByMaker evicts EVERY offer of a maker that needs the maker online to
// be lifted — that is all of them except a covenant, which the taker settles itself.
//
// RemoveByMaker deliberately spares cross-chain and Lightning offers, because they are
// durable across a maker RECONNECT and evicting on every transient WS blip emptied the
// cross book (TestCrossSurvivesMakerDisconnect). But durable-across-a-reconnect is not
// the same as liftable-while-the-maker-is-GONE: those offers keep resting, keep being
// served at top of book, and answer every taker with silence or a refusal until they
// expire. A fleet that restarts its makers leaves the book full of them.
//
// The caller applies this only after a grace period in which the maker did not come
// back, so a blip still costs nothing.
func (s *Store) RemoveInteractiveByMaker(makerPubkey string) int {
	s.mu.Lock()
	victims := make([]Key, 0)
	for k, e := range s.entries {
		if k.MakerPubkey == makerPubkey && !offer.FilledWithoutMaker(e.Offer) {
			victims = append(victims, k)
		}
	}
	type ev struct {
		pair *seqobv1.AssetPair
		conf bool
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(victims))
	for _, k := range victims {
		e := s.entries[k]
		evs = append(evs, ev{pair: e.Offer.GetPair(), conf: e.Offer.GetConfidential(), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Confidential: e.conf, Ref: e.ref})
	}
	return len(victims)
}

// RemoveInteractiveByKeys removes exactly the named non-covenant offers. It is the
// per-CONNECTION eviction primitive: pubkey-scoped eviction cannot tell two maker
// processes sharing one identity key apart, so a dead sibling's offers ghosted while
// any live sibling pinned the pubkey — and won price-time ties with prices no live
// process would honor. The caller names the keys the departed connection itself
// posted; a key re-posted since (and so owned by a live connection) is not passed in.
func (s *Store) RemoveInteractiveByKeys(keys []Key) int {
	s.mu.Lock()
	type ev struct {
		pair *seqobv1.AssetPair
		conf bool
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(keys))
	for _, k := range keys {
		e, ok := s.entries[k]
		if !ok || offer.FilledWithoutMaker(e.Offer) {
			continue
		}
		evs = append(evs, ev{pair: e.Offer.GetPair(), conf: e.Offer.GetConfidential(), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Confidential: e.conf, Ref: e.ref})
	}
	return len(evs)
}

func (s *Store) RemoveByMaker(makerPubkey string) int {
	s.mu.Lock()
	victims := make([]Key, 0)
	for k, e := range s.entries {
		// Evict ONLY interactive same-chain co-sign offers (they need the maker online now). A same-chain
		// COVENANT offer carries a covenant and is permissionless, so it is spared alongside cross/lightning.
		if k.MakerPubkey == makerPubkey && e.Offer.GetSameChain() != nil && !offer.FilledWithoutMaker(e.Offer) {
			victims = append(victims, k)
		}
	}
	type ev struct {
		pair *seqobv1.AssetPair
		conf bool
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(victims))
	for _, k := range victims {
		e := s.entries[k]
		evs = append(evs, ev{pair: e.Offer.GetPair(), conf: e.Offer.GetConfidential(), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Confidential: e.conf, Ref: e.ref})
	}
	return len(victims)
}

// --- covenant chain-watcher reconciliation (internal/seqob/watcher) ----------
//
// These mutate a resting COVENANT order to match on-chain reality. They are the
// only book operations the chain watcher needs, and they are DISTINCT from
// RemoveByMaker (which deliberately never evicts a covenant on a maker
// disconnect): the watcher removes or resizes a covenant strictly on CHAIN
// evidence (spent / partially filled / funding gone at the tip). None of them
// touch interactive offers.

// CovenantEntry is a resting covenant offer's key + terms + active size, for the
// watcher to reconcile. The returned *CovenantTerms is the live pointer; the
// watcher only reads it.
type CovenantEntry struct {
	Key      Key
	Offer    *seqobv1.Offer
	Terms    *seqobv1.CovenantTerms           // the tapscript order's terms, or nil
	Sim      *seqobv1.SimplicityCovenantTerms // the Simplicity order's terms, or nil
	Round    *seqobv1.RoundOrderTerms         // a round order's terms, or nil
	Active   uint64
	ReRested bool // F3: the current outpoint is a remainder from a prior partial fill, not the original funding
}

// SnapshotCovenants returns every resting offer that settles via a covenant.
func (s *Store) SnapshotCovenants() []CovenantEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CovenantEntry, 0)
	for k, e := range s.entries {
		if offer.IsCovenant(e.Offer) {
			out = append(out, CovenantEntry{Key: k, Offer: e.Offer, Terms: e.Offer.GetCovenant(),
				Sim: e.Offer.GetSimplicityCovenant(), Round: e.Offer.GetRoundOrder(), Active: e.ActiveAmount, ReRested: e.reRested})
		}
	}
	return out
}

// HoldCovenantGhost hides a re-rested covenant order whose current (remainder) outpoint is GONE at the
// tip with no spender — most likely a Bitcoin reorg undid the fill (anchoring supremacy). Unlike
// RemoveGhost (permanent MarkInvalidated), this is REVERSIBLE: active drops to 0 (hidden from matching,
// so no taker fills a vanished outpoint) but the entry is kept, so if the reorg heals and the fill
// re-confirms, a later reconcile pass sees the remainder again and re-opens it (F3). No-op for a
// non-covenant/missing entry.
func (s *Store) HoldCovenantGhost(k Key, reason string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	changed := e.ActiveAmount != 0
	if e.ActiveAmount > 0 {
		e.heldFill = e.ActiveAmount
	}
	e.ActiveAmount = 0
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	}
	return nil
}

// ReconcileCovenantActive sets a resting covenant order's active size to its
// on-chain UTXO value. Idempotent. It also RE-OPENS an order the watcher had HELD
// for an unconfirmed spend (HoldCovenantForSpend / HoldCovenantGhost) that then
// dropped or was reorged away — the covenant still holds its value at the tip, so
// the order becomes fillable again. (The matcher no longer decrements at match, so
// there is no optimistic match-time decrement to correct.) No-op for a
// non-covenant or missing entry.
func (s *Store) ReconcileCovenantActive(k Key, active uint64) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	if active > e.Offer.GetBaseAmount() {
		active = e.Offer.GetBaseAmount()
	}
	changed := e.ActiveAmount != active || e.SettleTxid != ""
	e.ActiveAmount = active
	e.SettleTxid = ""
	e.heldFill = 0 // B-8: clear any stale held-fill size so a later RemoveCovenantFilled uses the live active, not a stale hold
	if active >= e.Offer.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	}
	return nil
}

// RerestCovenantRemainder points a covenant order at the remainder outpoint a
// partial fill re-created (new txid:vout) and sets the reduced active size, so
// the rest of the order stays fillable. The covenant PROGRAM is unchanged (same
// scriptPubKey), so the maker's signed terms still describe it; only the
// chain-observable outpoint moves. The stored Offer is cloned before mutation so
// concurrent matcher reads of the old pointer stay consistent. The trade the
// fill made is recorded with its fill record (RecordCovenantFill), not here.
func (s *Store) RerestCovenantRemainder(k Key, txid string, vout uint32, size uint64) error {
	return s.RepointCovenant(k, txid, vout, size, true)
}

// HoldCovenantForSpend hides a covenant order from matching while its spend is
// unconfirmed (fill in the mempool): active drops to 0 (the matcher skips
// zero-active entries) and the settling txid is recorded. A later reconcile
// pass restores it (ReconcileCovenantActive) if the spend drops, or removes /
// re-rests it once the spend confirms.
func (s *Store) HoldCovenantForSpend(k Key, spenderTxid string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	changed := e.ActiveAmount != 0 || e.SettleTxid != spenderTxid
	if e.ActiveAmount > 0 {
		e.heldFill = e.ActiveAmount // B1: remember the size so the confirmed-fill trade isn't lost once active zeroes
	}
	e.ActiveAmount = 0
	e.SettleTxid = spenderTxid
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	}
	return nil
}

// RemoveCovenantFilled removes a fully-filled covenant order, recording the
// settling txid and emitting FILLED. The watcher calls it once the fill is
// final; the trade the fill made is recorded with its fill record
// (RecordCovenantFill). No-op for a non-covenant or missing entry.
func (s *Store) RemoveCovenantFilled(k Key, settleTxid string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || !offer.IsCovenant(e.Offer) {
		s.mu.Unlock()
		return nil
	}
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_FILLED
	e.SettleTxid = settleTxid
	e.ActiveAmount = 0
	pair := e.Offer.GetPair()
	conf := e.Offer.GetConfidential()
	st := e.orderStatus(k)
	s.remove(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Confidential: conf, Status: st})
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Confidential: conf, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// RunExpirySweeper sweeps every interval until ctx-like stop channel closes.
func (s *Store) RunExpirySweeper(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.SweepExpired()
		}
	}
}

// Subscribe registers a delta subscriber bound to a book NAMESPACE: confidential=false is
// the transparent book, confidential=true the separate blinded book. Both the returned
// snapshot and every subsequent delta are filtered to that namespace, so a transparent
// subscriber never sees a confidential offer and vice-versa — the same disjoint split
// SnapshotPair serves over REST and the matcher enforces on crossing. If pairFilter is
// non-nil, only deltas for that pair are delivered; a nil pairFilter subscribes to every
// pair (still within the requested namespace). Returns the current snapshot, a subscription
// id, and the delta channel.
func (s *Store) Subscribe(pairFilter *seqobv1.AssetPair, confidential bool) ([]*seqobv1.Offer, int, <-chan Event) {
	entries, id, ch := s.SubscribeEntries(pairFilter, confidential)
	snap := make([]*seqobv1.Offer, len(entries))
	for i := range entries {
		snap[i] = entries[i].Offer
	}
	return snap, id, ch
}

// SubscribeEntries is Subscribe with the snapshot as entry copies, so a book
// served from it carries each order's live state (Entry.OrderStatus) taken at
// the same instant the subscription starts.
func (s *Store) SubscribeEntries(pairFilter *seqobv1.AssetPair, confidential bool) ([]Entry, int, <-chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	pk := ""
	if pairFilter != nil {
		pk = pairKey(pairFilter)
	}
	sub := &subscriber{pair: pk, confidential: confidential, ch: make(chan Event, s.subBuf)}
	s.subs[id] = sub

	var snap []Entry
	if pk != "" {
		for k := range s.pairs[pk] {
			if e, ok := s.entries[k]; ok && e.Offer.GetConfidential() == confidential {
				snap = append(snap, *e)
			}
		}
	} else {
		snap = make([]Entry, 0, len(s.entries))
		for _, e := range s.entries {
			if e.Offer.GetConfidential() == confidential {
				snap = append(snap, *e)
			}
		}
	}
	return snap, id, sub.ch
}

// Unsubscribe removes and closes a subscription.
func (s *Store) Unsubscribe(id int) {
	s.mu.Lock()
	sub, ok := s.subs[id]
	if ok {
		delete(s.subs, id)
		close(sub.ch) // under the write lock, so no broadcast (which sends under RLock) is mid-send
	}
	s.mu.Unlock()
}

// --- locked helpers ---

func (s *Store) put(k Key, e *Entry) {
	if old, ok := s.entries[k]; ok {
		s.unindexLocked(k, old.Offer)
	}
	s.entries[k] = e
	s.indexLocked(k, e.Offer)
	pk := pairKey(e.Offer.GetPair())
	set := s.pairs[pk]
	if set == nil {
		set = make(map[Key]bool)
		s.pairs[pk] = set
	}
	set[k] = true
}

func (s *Store) remove(k Key) {
	e, ok := s.entries[k]
	if !ok {
		return
	}
	pk := pairKey(e.Offer.GetPair())
	s.unindexLocked(k, e.Offer)
	delete(s.entries, k)
	if set := s.pairs[pk]; set != nil {
		delete(set, k)
		if len(set) == 0 {
			delete(s.pairs, pk)
		}
	}
}

// OrderStatus is the entry's live state as the relay serves it: its status and
// active_amount, the base atoms that can be filled now (a covenant order: what
// its outpoint holds; 0 while a spend of it is unconfirmed).
func (e *Entry) OrderStatus() *seqobv1.OrderStatus { return e.orderStatus(keyOf(e.Offer)) }

func (e *Entry) orderStatus(k Key) *seqobv1.OrderStatus {
	return &seqobv1.OrderStatus{
		OfferId:      k.OfferID,
		MakerPubkey:  k.MakerPubkey,
		Status:       e.Status,
		ActiveAmount: e.ActiveAmount,
		SettleTxid:   e.SettleTxid,
		AnchorConfs:  e.AnchorConfs,
		Demoted:      e.demoted,
	}
}

// broadcast delivers an event to all matching subscribers without blocking. If a
// subscriber's buffer is full the event is dropped for that subscriber (it can
// re-snapshot); the book is never blocked by a slow client.
func (s *Store) broadcast(ev Event) {
	pk := pairKey(ev.Pair)
	s.mu.RLock()
	subs := make([]*subscriber, 0, len(s.subs))
	for _, sub := range s.subs {
		// Namespace filter: a delta only reaches subscribers bound to its OWN book
		// (transparent vs confidential), so the two books stay disjoint over the delta
		// stream exactly as they are over the snapshot (SnapshotPair) and the matcher.
		if sub.confidential != ev.Confidential {
			continue
		}
		if sub.pair == "" || sub.pair == pk {
			subs = append(subs, sub)
		}
	}
	// Deliver while still holding the read lock: Unsubscribe closes the channel under
	// the write lock, so a send can never race a close (a send on a closed channel
	// panics, and broadcast runs on the expiry sweeper and watcher goroutines, where
	// nothing recovers it). The sends are non-blocking, so the lock is held briefly.
	for _, sub := range subs {
		// Stamp a per-subscriber sequence number and increment for EVERY routed event
		// (P3.8), so a dropped delta (buffer full -> default) leaves a hole the
		// subscriber detects as a gap and re-snapshots. Copy the Event per subscriber
		// since each gets its own Seq.
		e := ev
		e.Seq = sub.seq.Add(1)
		select {
		case sub.ch <- e:
		default:
			// Dropped: the seq is consumed, so the next delivered delta shows a gap.
		}
	}
	s.mu.RUnlock()
}
