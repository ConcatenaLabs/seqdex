package offerstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// --- the trade log -------------------------------------------------------------
//
// One log per market, whichever way its orders were written and whichever rail
// settled each trade. A market is the unordered pair of its two assets: an order
// selling A for B and an order selling B for A trade in the same market, so their
// trades are kept together, in the market's canonical direction (canonicalPair),
// and served in whichever direction a reader asks for. Candles, the ticker, the
// recent-trades feed and each book's last price all read this one log, so every
// surface that reads the relay shows the same numbers.
//
// A trade is recorded by whoever can attest it: the chain watcher for a covenant
// fill (on the chain, in a clearing round or an operator's round), the resting
// follower for an order resting in an operator's tree (from the operator's own
// view), the interactive session or its maker's signed settled-trade record for
// an interactive, cross-chain or Lightning lift, and a maker the relay trusts
// (-trade-reporters) for its own in-tree swaps and BTC desk (RecordReported).
// Each carries an id unique to its fill, and a second record of the same id is
// ignored, so a fill attested twice is one trade.

// Trade is one recorded trade, in the direction of the pair it was read for.
type Trade struct {
	TS    time.Time
	Price float64 // quote atoms per base atom (the unit of best_bid / best_ask)
	Size  uint64  // base atoms
	Quote uint64  // quote atoms
	// TakerBuys: the taker received the base asset (the maker sold it).
	TakerBuys bool
	// Rail names how the trade settled (Rail* constants). It is shown to
	// developers; a person's view of the market never names it.
	Rail string
	// ID is unique to the fill (its settling transaction and order, the
	// operator's transfer, a maker's swap): a second record of it is ignored.
	ID string
	// Txid is the settling tx this trade was recorded for (empty for a
	// txid-less record). It lets a Bitcoin-driven reorg RETRACT the phantom
	// trade (RetractTrade) so a reorged-away fill no longer leaves a stale
	// last_price (P3.9).
	Txid string
}

// The rails a trade can settle on.
const (
	RailOnchain       = "onchain-fill"   // a covenant order taken on the chain
	RailCovenantRound = "covenant-round" // a covenant order taken in an operator's round
	RailClearingRound = "clearing-round" // a covenant order filled in a clearing round
	RailLeafResting   = "leaf-resting"   // an order resting as a leaf in an operator's tree
	RailInteractive   = "interactive"    // an interactive lift, co-signed with its maker
	RailCross         = "cross"          // BTC on the parent chain against an asset
	RailLightning     = "lightning"      // a Lightning lift
	RailLeafMaker     = "leaf-maker"     // an in-tree swap with a leaf maker (reported)
	RailBTCGateway    = "btc-gateway"    // BTC for an asset in one hash-lock at a maker's desk (reported)
)

// ReportedRails are the rails a maker may report its own trades on.
var ReportedRails = map[string]bool{RailLeafMaker: true, RailBTCGateway: true}

const maxTradesPerPair = 1000 // ring cap per market; oldest dropped

// tradeRec is one line of the durable JSONL trade log: the market key plus the
// trade in the market's canonical direction, so last_price/trades/candles can be
// replayed after a relay restart (the book itself is ephemeral). A record with
// RetractTxid set is a RETRACTION (P3.9): on replay it removes any
// already-replayed trades with that txid for that market, so a reorged-away fill
// does not resurrect after a restart. The log stays append-only.
//
// A line written before markets were canonical has no Quote: its PK is the
// resting order's pair and Size its base, and it is read into the canonical
// market on replay.
type tradeRec struct {
	PK          string    `json:"pk"`
	TS          time.Time `json:"ts"`
	Price       float64   `json:"price"`
	Size        uint64    `json:"size"`
	Quote       uint64    `json:"quote,omitempty"`
	Buy         bool      `json:"buy,omitempty"`
	Rail        string    `json:"rail,omitempty"`
	ID          string    `json:"id,omitempty"`
	Txid        string    `json:"txid,omitempty"`
	RetractTxid string    `json:"retract_txid,omitempty"`
}

// canonicalPair is a market's one direction: the asset whose id sorts first is
// its base.
func canonicalPair(a, b string) (base, quote string, inverted bool) {
	if b < a {
		return b, a, true
	}
	return a, b, false
}

// marketKey is the canonical key of the market a pair trades in, and whether
// the pair is written the other way round.
func marketKey(p *seqobv1.AssetPair) (string, bool) {
	b, q, inv := canonicalPair(p.GetBaseAsset(), p.GetQuoteAsset())
	return b + "/" + q, inv
}

func splitKey(pk string) (string, string) {
	for i := 0; i < len(pk); i++ {
		if pk[i] == '/' {
			return pk[:i], pk[i+1:]
		}
	}
	return pk, ""
}

// flipped is t seen from the other direction of its market.
func (t Trade) flipped() Trade {
	f := t
	f.Size, f.Quote = t.Quote, t.Size
	f.TakerBuys = !t.TakerBuys
	f.Price = 0
	if t.Price > 0 {
		f.Price = 1 / t.Price
	}
	return f
}

// tradeIDOf is the id of a fill of order k settled by txid: one order filled
// once by one transaction. Empty for a txid-less fill.
func tradeIDOf(k Key, txid string) string {
	if txid == "" {
		return ""
	}
	return k.MakerPubkey + ":" + k.OfferID + ":" + txid
}

// RecordTrade appends one executed cross for the resting offer's pair at its
// execution price. Best-effort: bad inputs are ignored.
func (s *Store) RecordTrade(restingOffer *seqobv1.Offer, sizeBase uint64) {
	s.RecordTradeTx(restingOffer, sizeBase, "")
}

// RecordTradeTx is RecordTrade with the SETTLING TXID attached, so a later
// Bitcoin reorg can retract this exact trade (RetractTrade). The trade's rail is
// the interactive lift's; callers that know another pass it to RecordTradeVia.
func (s *Store) RecordTradeTx(restingOffer *seqobv1.Offer, sizeBase uint64, txid string) {
	if restingOffer == nil {
		return
	}
	s.RecordTradeVia(restingOffer, sizeBase, txid, tradeIDOf(keyOf(restingOffer), txid), RailInteractive)
}

// RecordTradeVia records a fill of sizeBase base atoms of the resting offer at
// its price, settled on rail, under id (a repeat of a held id is ignored).
func (s *Store) RecordTradeVia(restingOffer *seqobv1.Offer, sizeBase uint64, txid, id, rail string) {
	if restingOffer == nil || sizeBase == 0 {
		return
	}
	price, _ := displayPrice(restingOffer)
	pair := restingOffer.GetPair()
	if price <= 0 || pair == nil {
		return
	}
	quote := uint64(math.Round(float64(sizeBase) * price))
	if quote == 0 {
		return
	}
	// The maker of a SELL gives the pair's base: its taker buys it.
	t := Trade{Price: price, Size: sizeBase, Quote: quote, TakerBuys: restingOffer.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_SELL,
		Rail: rail, ID: id, Txid: txid}
	s.appendTrade(pair, t)
}

// RecordReported records a trade a maker reported of its own: it gave `give`
// atoms of giveAsset for `want` atoms of wantAsset, on rail, under id. Returns
// false when the trade is not one (an amount of zero, one asset twice) or its id
// is held already.
func (s *Store) RecordReported(giveAsset string, give uint64, wantAsset string, want uint64, rail, id string) bool {
	if give == 0 || want == 0 || giveAsset == "" || wantAsset == "" || giveAsset == wantAsset || id == "" {
		return false
	}
	// In the pair (give, want) the maker sells the base.
	pair := &seqobv1.AssetPair{BaseAsset: giveAsset, QuoteAsset: wantAsset}
	t := Trade{Price: float64(want) / float64(give), Size: give, Quote: want, TakerBuys: true, Rail: rail, ID: id}
	return s.appendTrade(pair, t)
}

// appendTrade stores t (written in pair's direction) in its market, unless a
// trade of the same id is held; then appends it to the durable log.
func (s *Store) appendTrade(pair *seqobv1.AssetPair, t Trade) bool {
	pk, inv := marketKey(pair)
	if inv {
		t = t.flipped()
	}
	t.TS = s.now()
	if t.ID == "" {
		// A fill with nothing to name it by still gets an id, so a relay
		// following this one's trades can tell it from every other.
		t.ID = fmt.Sprintf("%s:%d", t.Rail, t.TS.UnixNano())
	}
	s.mu.Lock()
	if s.trades == nil {
		s.trades = make(map[string][]Trade)
	}
	if t.ID != "" {
		for _, have := range s.trades[pk] {
			if have.ID == t.ID {
				s.mu.Unlock()
				return false
			}
		}
	}
	ring := append(s.trades[pk], t)
	if len(ring) > maxTradesPerPair {
		ring = ring[len(ring)-maxTradesPerPair:]
	}
	s.trades[pk] = ring
	logf := s.tradeLog
	s.mu.Unlock()
	// Durably append AFTER releasing the lock (B-4): a slow disk must never
	// serialize the whole book behind the trade-log write. O_APPEND writes are
	// atomic and Write is goroutine-safe. Best-effort.
	if logf != nil {
		if b, err := json.Marshal(recOf(pk, t)); err == nil {
			_, _ = logf.Write(append(b, '\n'))
		}
	}
	return true
}

func recOf(pk string, t Trade) tradeRec {
	return tradeRec{PK: pk, TS: t.TS, Price: t.Price, Size: t.Size, Quote: t.Quote, Buy: t.TakerBuys, Rail: t.Rail, ID: t.ID, Txid: t.Txid}
}

// RetractTrade removes every recorded trade of pair's market whose settling tx
// is txid, then durably appends a RETRACTION so the trade does not resurrect on
// a restart replay (P3.9). It is the trade-feed half of reorg-undo: when a
// Bitcoin reorg orphans a settling tx, the fill un-happened, so the phantom
// trade, its share of last_price and its candle volume must go. Returns the
// number removed. No-op for an empty txid.
func (s *Store) RetractTrade(pair *seqobv1.AssetPair, txid string) int {
	if pair == nil || txid == "" {
		return 0
	}
	pk, _ := marketKey(pair)
	ts := s.now()
	s.mu.Lock()
	removed := s.retractLocked(pk, txid)
	logf := s.tradeLog
	s.mu.Unlock()
	if removed > 0 && logf != nil {
		if b, err := json.Marshal(tradeRec{PK: pk, TS: ts, RetractTxid: txid}); err == nil {
			_, _ = logf.Write(append(b, '\n'))
		}
	}
	return removed
}

func (s *Store) retractLocked(pk, txid string) int {
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
	return removed
}

// EnableTradeLog makes the trade history durable: it replays an existing
// append-only JSONL log into the in-memory rings, then keeps appending new
// trades to it. Best-effort; call once at startup.
func (s *Store) EnableTradeLog(path string) error {
	if f, err := os.Open(path); err == nil {
		s.mu.Lock()
		if s.trades == nil {
			s.trades = make(map[string][]Trade)
		}
		// bufio.Reader (not Scanner): one over-long or corrupt line must not end
		// the replay (B-5): read line by line, skip a bad one, keep going.
		rd := bufio.NewReaderSize(f, 1<<20)
		for {
			line, rerr := rd.ReadBytes('\n')
			if len(line) > 0 {
				var r tradeRec
				if json.Unmarshal(line, &r) == nil && r.PK != "" {
					s.replayLocked(r)
				}
			}
			if rerr != nil {
				break
			}
		}
		s.mu.Unlock()
		_ = f.Close()
		// Compact to the retained tail so the log cannot grow without bound and
		// each restart's replay stays O(retained) (B-5).
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

func (s *Store) replayLocked(r tradeRec) {
	b, q := splitKey(r.PK)
	pk, inv := marketKey(&seqobv1.AssetPair{BaseAsset: b, QuoteAsset: q})
	if r.RetractTxid != "" {
		s.retractLocked(pk, r.RetractTxid)
		return
	}
	if r.Price <= 0 || r.Size == 0 {
		return
	}
	t := Trade{TS: r.TS, Price: r.Price, Size: r.Size, Quote: r.Quote, TakerBuys: r.Buy, Rail: r.Rail, ID: r.ID, Txid: r.Txid}
	if t.Quote == 0 {
		// A line from before markets were canonical: its pair is the resting
		// order's, its size that pair's base, its rail unrecorded.
		t.Quote = uint64(math.Round(float64(r.Size) * r.Price))
		if t.Quote == 0 {
			return
		}
	}
	if inv {
		t = t.flipped()
	}
	if t.ID != "" {
		for _, have := range s.trades[pk] {
			if have.ID == t.ID {
				return
			}
		}
	}
	ring := append(s.trades[pk], t)
	if len(ring) > maxTradesPerPair {
		ring = ring[len(ring)-maxTradesPerPair:]
	}
	s.trades[pk] = ring
}

// compactTradeLog rewrites the log to exactly the retained in-memory trades
// (best-effort; startup only, before any concurrent writer is armed).
func (s *Store) compactTradeLog(path string) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	s.mu.RLock()
	for pk, ring := range s.trades {
		for _, tr := range ring {
			if b, mErr := json.Marshal(recOf(pk, tr)); mErr == nil {
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

// TradesFor returns the most recent trades of pair's market (up to limit; 0 =
// all held), newest last, in pair's direction.
func (s *Store) TradesFor(pair *seqobv1.AssetPair, limit int) []Trade {
	if pair == nil {
		return nil
	}
	pk, inv := marketKey(pair)
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.mergedLocked(pk)
	if limit > 0 && len(t) > limit {
		t = t[len(t)-limit:]
	}
	out := make([]Trade, len(t))
	for i, tr := range t {
		if inv {
			tr = tr.flipped()
		}
		out[i] = tr
	}
	return out
}

// lastTradeLocked is the newest trade of the market of the pair keyed pk
// ("base/quote"), in that pair's direction. The caller holds s.mu.
func (s *Store) lastTradeLocked(pk string) (Trade, bool) {
	b, q := splitKey(pk)
	mk, inv := marketKey(&seqobv1.AssetPair{BaseAsset: b, QuoteAsset: q})
	ring := s.mergedLocked(mk)
	if len(ring) == 0 {
		return Trade{}, false
	}
	t := ring[len(ring)-1]
	if inv {
		t = t.flipped()
	}
	return t, true
}

// LocalTrades is every market's trades recorded by this relay itself (not
// those it follows), in each market's canonical direction, keyed by market
// ("base/quote"): what GET /v1/trades serves to a relay following this one.
func (s *Store) LocalTrades() map[string][]Trade {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Trade, len(s.trades))
	for pk, ring := range s.trades {
		out[pk] = append([]Trade(nil), ring...)
	}
	return out
}

// SetMirror replaces what is held of a followed relay's trades with its latest
// answer (market key -> trades, canonical direction, as LocalTrades gives
// them). A trade the other relay retracted is gone from its next answer, and so
// from here. A nil answer forgets the relay.
func (s *Store) SetMirror(source string, trades map[string][]Trade) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if trades == nil {
		delete(s.mirrors, source)
		return
	}
	if s.mirrors == nil {
		s.mirrors = make(map[string]map[string][]Trade)
	}
	norm := make(map[string][]Trade, len(trades))
	for k, ring := range trades {
		b, q := splitKey(k)
		pk, inv := marketKey(&seqobv1.AssetPair{BaseAsset: b, QuoteAsset: q})
		for _, t := range ring {
			if inv {
				t = t.flipped()
			}
			norm[pk] = append(norm[pk], t)
		}
	}
	s.mirrors[source] = norm
}

// mergedLocked is the market's trades: this relay's own and every followed
// relay's, one per id, oldest first. The caller holds s.mu.
func (s *Store) mergedLocked(pk string) []Trade {
	own := s.trades[pk]
	if len(s.mirrors) == 0 {
		return own
	}
	out := append([]Trade(nil), own...)
	seen := make(map[string]bool, len(own))
	for _, t := range own {
		if t.ID != "" {
			seen[t.ID] = true
		}
	}
	added := false
	srcs := make([]string, 0, len(s.mirrors))
	for src := range s.mirrors {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		for _, t := range s.mirrors[src][pk] {
			if t.ID != "" && seen[t.ID] {
				continue
			}
			if t.ID != "" {
				seen[t.ID] = true
			}
			out = append(out, t)
			added = true
		}
	}
	if added {
		sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	}
	return out
}
