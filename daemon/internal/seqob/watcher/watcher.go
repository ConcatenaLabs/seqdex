package watcher

import (
	"encoding/hex"
	"fmt"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// ChainView is the read-only node surface the watcher needs. It is deliberately
// small and total so the reconciliation loop can be driven by a fake in tests.
// Every method reads the LIVE tip; there is no cache.
type ChainView interface {
	// Tip returns the current best block hash and height. Used for reorg
	// detection and to prove each pass reads the live tip.
	Tip() (hash string, height int64, err error)
	// Inspect reports the on-chain reality of a covenant outpoint at the current
	// tip. orderSPKHex is the covenant program scriptPubKey and assetADisplay the
	// display (reversed) asset-A id; both let Inspect recognise a re-created
	// remainder covenant output (asset == asset_a, spk == the program, at index
	// 2k+1 for the covenant input's index k in the spender).
	Inspect(txid string, vout uint32, orderSPKHex, assetADisplay string) (CovState, error)
	// TxStatus reports where a transaction stands: in a block of the active
	// chain (its height, anchor and certification), in the mempool, or neither.
	TxStatus(txid string) (TxStatus, error)
	// Anchor reports the tip's Bitcoin anchor height and the node's check of it.
	Anchor() (AnchorTip, error)
}

// CovEntry is one resting covenant offer the watcher must reconcile.
type CovEntry struct {
	Key      offerstore.Key
	Offer    *seqobv1.Offer                   // the signed offer as it rests (its outpoint is outside the signed bytes)
	Terms    *seqobv1.CovenantTerms           // a tapscript order's terms, or nil
	Sim      *seqobv1.SimplicityCovenantTerms // a Simplicity order's terms, or nil
	Round    *seqobv1.RoundOrderTerms         // a round order's terms, or nil
	Active   uint64                           // book's current active size (advisory; reconciled to chain)
	ReRested bool                             // F3: current outpoint is a remainder from a prior partial fill, not the original funding
}

// outpoint is where the order rests.
func (c CovEntry) outpoint() (string, uint32) {
	if c.Round != nil {
		return c.Round.GetCovenantTxid(), c.Round.GetCovenantVout()
	}
	if c.Sim != nil {
		return c.Sim.GetCovenantTxid(), c.Sim.GetCovenantVout()
	}
	return c.Terms.GetCovenantTxid(), c.Terms.GetCovenantVout()
}

// BookController is the covenant-aware subset of the relay's book the watcher
// drives. offerstore.Store implements it (via the storeBook adapter). These are
// distinct from RemoveByMaker (which deliberately never evicts covenant offers on
// a maker disconnect); the watcher removes covenants only on CHAIN evidence.
type BookController interface {
	// SnapshotCovenants returns every resting covenant offer to reconcile.
	SnapshotCovenants() []CovEntry
	// ReconcileLive sets a resting covenant order's active size to its on-chain
	// UTXO value (idempotent; also re-opens a fill that never confirmed).
	ReconcileLive(k offerstore.Key, active uint64) error
	// Repoint points a covenant order at an outpoint holding size atoms: the
	// remainder a partial fill rested again (reRested true), or, when a fill is
	// undone, where the order rested before it. Its terms are unchanged.
	Repoint(k offerstore.Key, txid string, vout uint32, size uint64, reRested bool) error
	// RemoveFilled removes a covenant order whose full fill is final (FILLED).
	RemoveFilled(k offerstore.Key, spenderTxid string) error
	// RemoveRefunded removes a covenant order its maker took back through
	// REFUND once that spend is final (EXPIRED, no trade).
	RemoveRefunded(k offerstore.Key, spenderTxid string) error
	// HoldForSpend hides a covenant order from matching while its spend is
	// unconfirmed, or confirmed and not yet final. A later pass re-opens it if
	// the spend drops or removes/re-rests it as the spend settles.
	HoldForSpend(k offerstore.Key, spenderTxid string) error
	// RemoveGhost removes a covenant order whose funding UTXO is gone at the tip
	// (reorg / never-confirmed): maps to OFFER_STATUS_UTXO_INVALIDATED.
	RemoveGhost(k offerstore.Key, reason string) error
	// HoldGhost hides (does NOT remove) a RE-RESTED covenant order whose remainder
	// outpoint is gone: a Bitcoin reorg may have undone the fill, so keep the entry
	// so a later pass can re-open it if the fill re-confirms (F3, reversible).
	HoldGhost(k offerstore.Key, reason string) error
	// Reopen puts back an order a final fill removed, when that fill is undone.
	Reopen(o *seqobv1.Offer, active uint64) error
	// RecordFill records a covenant fill and the trade it makes.
	RecordFill(f offerstore.CovenantFill, resting *seqobv1.Offer)
	// UpdateFill moves a recorded fill on: its finality, or undone (which
	// retracts its trade).
	UpdateFill(f offerstore.CovenantFill)
	// SetAnchorConfs shows the anchor depth of an order's settling transaction
	// on its status.
	SetAnchorConfs(k offerstore.Key, depth uint32)
}

// Logger is the minimal logging surface (satisfied by *log.Logger).
type Logger interface {
	Printf(format string, v ...interface{})
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

// DefaultFinalAnchorDepth is the Bitcoin blocks that must bury a fill's anchor
// before the relay calls it final, as the Arca specification's finality rule
// counts them; an offer's min_anchor_depth raises it for that offer.
const DefaultFinalAnchorDepth = 2

// Watcher reconciles the relay's covenant book to the node's current tip, and
// keeps the record of every covenant fill it sees in a block.
type Watcher struct {
	chain ChainView
	book  BookController
	log   Logger

	finalDepth int64
	rounds     RoundSource

	lastTipHash   string
	lastTipHeight int64

	// tracks holds, per order, the fills the watcher has recorded, oldest
	// first: each spent the outpoint the one before it rested the order at.
	// A fill's block can leave the chain at any depth (an anchor reorg), so a
	// fill stays tracked, final or not, while its record is held.
	tracks map[offerstore.Key]*track

	// poolTracks holds, per side of a pool, the moves the pool follower has
	// recorded, oldest first (pools.go).
	poolTracks map[offerstore.Key][]*poolStep
}

// step is one fill (or refund) of one order the watcher has seen in a block.
type step struct {
	prevTxid     string
	prevVout     uint32
	prevSize     uint64
	prevReRested bool
	prevOffer    *seqobv1.Offer // the order as it rested before this spend

	spender  string
	k        uint32
	refund   bool
	partial  bool
	required int64

	record  offerstore.CovenantFill
	final   bool
	removed bool // the order left the book when this step became final
}

type track struct{ steps []*step }

// maxTrackedSteps bounds what the watcher keeps; past it, the oldest final
// steps of orders that left the book are dropped first.
const maxTrackedSteps = 20000

// New builds a Watcher over a chain view and a book controller.
func New(chain ChainView, book BookController, log Logger) *Watcher {
	if log == nil {
		log = nopLogger{}
	}
	return &Watcher{chain: chain, book: book, log: log, finalDepth: DefaultFinalAnchorDepth, tracks: map[offerstore.Key]*track{}}
}

// SetFinalAnchorDepth sets the anchor depth a fill needs to be final (at least
// 1; an offer's min_anchor_depth raises it for that offer).
func (w *Watcher) SetFinalAnchorDepth(d int64) {
	if d < 1 {
		d = 1
	}
	w.finalDepth = d
}

// SetRounds names the Arca operators whose rounds the watcher recognises.
func (w *Watcher) SetRounds(r RoundSource) { w.rounds = r }

// Stats summarises one reconciliation pass.
type Stats struct {
	Scanned     int
	Live        int
	PartialFill int
	Filled      int
	Final       int
	Undone      int
	Pending     int
	Ghost       int
	Errors      int
	ReorgSeen   bool
}

// ReconcileOnce runs one full reconciliation pass: read the live tip, check
// every fill already recorded against it (final, still waiting, or undone),
// then classify each resting covenant offer against the current UTXO set and
// drive the book. It is safe to call repeatedly on a ticker.
func (w *Watcher) ReconcileOnce() (Stats, error) {
	var st Stats

	tipHash, tipHeight, err := w.chain.Tip()
	if err != nil {
		return st, fmt.Errorf("read tip: %w", err)
	}
	// Reorg / rollback detection. Every pass reconciles against the live UTXO set
	// regardless (so a vanished funding UTXO is caught as GHOST anyway); tracking
	// the tip lets us log the event and know a re-scan is warranted. Anchoring
	// supremacy is enforced by the NODE (it reorgs when Bitcoin reorgs); we only
	// consume its resulting tip.
	if w.lastTipHash != "" && tipHash != w.lastTipHash {
		if tipHeight <= w.lastTipHeight {
			st.ReorgSeen = true
			w.log.Printf("watcher: reorg/rollback detected (was %s@%d, now %s@%d); re-scanning all covenant outpoints",
				short(w.lastTipHash), w.lastTipHeight, short(tipHash), tipHeight)
		}
	}
	w.lastTipHash, w.lastTipHeight = tipHash, tipHeight

	anchor, aerr := w.chain.Anchor()
	if aerr != nil {
		st.Errors++
		w.log.Printf("watcher: read anchor: %v; no fill becomes final this pass", aerr)
	}

	// 1. The fills already recorded: still in the chain (and final yet?), or undone.
	for k, tr := range w.tracks {
		w.recheck(k, tr, anchor, aerr == nil, &st)
	}

	// 1b. The interactive fills the relay recorded: their transactions' standing.
	w.followInteractive(anchor, aerr == nil, &st)

	// 2. The resting orders.
	covs := w.book.SnapshotCovenants()
	st.Scanned = len(covs)
	for _, c := range covs {
		if tr := w.tracks[c.Key]; tr != nil && len(tr.steps) > 0 && !tr.steps[len(tr.steps)-1].partial {
			continue // spent whole; held until that spend is final, and watched in step 1
		}
		exp, err := c.expect()
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s: derive expected program: %v", c.Key.MakerPubkey, c.Key.OfferID, err)
			continue
		}
		txid, vout := c.outpoint()
		state, err := w.chain.Inspect(txid, vout, exp.OrderSPKHex, exp.AssetADisplay)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s: inspect %s:%d: %v", c.Key.MakerPubkey, c.Key.OfferID, txid, vout, err)
			continue
		}
		v := Classify(state, exp)
		w.apply(c, v, state, anchor, aerr == nil, &st)
	}
	// 3. The pools, when the book lists them.
	if pb, ok := w.book.(PoolBook); ok {
		w.reconcilePools(pb, anchor, aerr == nil, &st)
	}
	w.prune()
	return st, nil
}

// Run reconciles every interval until stop is closed. Errors from a pass are
// logged, not fatal (a transient node hiccup must not tear down the watcher).
func (w *Watcher) Run(interval time.Duration, stop <-chan struct{}) {
	// Reconcile once immediately so the book converges without waiting a tick.
	if _, err := w.ReconcileOnce(); err != nil {
		w.log.Printf("watcher: initial pass: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := w.ReconcileOnce(); err != nil {
				w.log.Printf("watcher: pass: %v", err)
			}
		}
	}
}

// finality measures a transaction in a block: its anchor depth, whether its
// block is certified, and whether that makes it final at `required`. On an
// anchored chain the depth is the tip's anchor height less the block's, and
// nothing is final while the node has not checked the tip's anchor against
// Bitcoin ("ok"). A chain without anchors (a test chain) counts its own blocks
// above the fill instead.
func finality(ts TxStatus, a AnchorTip, anchorKnown bool, required int64) (depth int64, certified, final bool) {
	certified = !ts.CertPresent || ts.Certified
	if !anchorKnown {
		return 0, certified, false
	}
	if a.Anchored {
		depth = a.Height - ts.BlockAnchor
		if depth < 0 {
			depth = 0
		}
		return depth, certified, certified && a.Status == "ok" && depth >= required
	}
	depth = ts.Confirmations - 1
	return depth, certified, certified && depth >= required
}

func (w *Watcher) required(o *seqobv1.Offer) int64 {
	r := w.finalDepth
	if d := int64(o.GetMinAnchorDepth()); d > r {
		r = d
	}
	return r
}

// recheck walks an order's recorded fills, oldest first: each still in a block
// of the active chain is measured again (and finalised, removing the order
// once a full fill or a refund is final); the first that is not undoes itself
// and every fill after it, and the order rests again where it rested before.
func (w *Watcher) recheck(k offerstore.Key, tr *track, a AnchorTip, anchorKnown bool, st *Stats) {
	for i, s := range tr.steps {
		ts, err := w.chain.TxStatus(s.spender)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s: status of %s: %v", k.MakerPubkey, k.OfferID, short(s.spender), err)
			return
		}
		if !ts.InChain {
			why := "its transaction left the active chain"
			if ts.InMempool {
				why = "its block left the active chain (its transaction is in the mempool again)"
			}
			w.undo(k, tr, i, why, st)
			return
		}
		depth, certified, final := finality(ts, a, anchorKnown, s.required)
		if !s.refund {
			rec := s.record
			rec.Block, rec.Height, rec.Certified, rec.AnchorDepth = ts.Block, ts.Height, certified, depth
			rec.State = offerstore.FillConfirmed
			if final || (s.final && !anchorKnown) {
				rec.State = offerstore.FillFinal
			}
			if rec.Block != s.record.Block || rec.Certified != s.record.Certified || rec.AnchorDepth != s.record.AnchorDepth || rec.State != s.record.State {
				w.book.UpdateFill(rec)
				s.record = rec
			}
		}
		if i == len(tr.steps)-1 && depth >= 0 {
			w.book.SetAnchorConfs(k, uint32(depth))
		}
		if final && !s.final {
			s.final = true
			st.Final++
			w.log.Printf("watcher: %s/%s fill %s:%d FINAL (certified, anchor depth %d >= %d)", k.MakerPubkey, k.OfferID, short(s.spender), s.k, depth, s.required)
			if i == len(tr.steps)-1 && !s.partial {
				var err error
				if s.refund {
					err = w.book.RemoveRefunded(k, s.spender)
				} else {
					err = w.book.RemoveFilled(k, s.spender)
				}
				if err != nil {
					st.Errors++
				}
				s.removed = true
			}
		}
	}
}

// undo marks steps i.. undone, newest first, and puts the order back where it
// rested before step i.
func (w *Watcher) undo(k offerstore.Key, tr *track, i int, why string, st *Stats) {
	removed := false
	for j := len(tr.steps) - 1; j >= i; j-- {
		s := tr.steps[j]
		removed = removed || s.removed
		st.Undone++
		if s.refund {
			w.log.Printf("watcher: %s/%s refund %s UNDONE: %s", k.MakerPubkey, k.OfferID, short(s.spender), why)
			continue
		}
		rec := s.record
		rec.State, rec.Reason = offerstore.FillUndone, why
		w.book.UpdateFill(rec)
		w.log.Printf("watcher: %s/%s fill %s:%d UNDONE: %s", k.MakerPubkey, k.OfferID, short(s.spender), s.k, why)
	}
	first := tr.steps[i]
	if removed && first.prevOffer != nil {
		if err := w.book.Reopen(first.prevOffer, first.prevSize); err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s reopen after undo: %v", k.MakerPubkey, k.OfferID, err)
		}
	}
	if err := w.book.Repoint(k, first.prevTxid, first.prevVout, first.prevSize, first.prevReRested); err != nil {
		st.Errors++
	}
	tr.steps = tr.steps[:i]
}

func (w *Watcher) apply(c CovEntry, v Verdict, state CovState, a AnchorTip, anchorKnown bool, st *Stats) {
	k := c.Key
	var err error
	switch v.State {
	case StateLive:
		st.Live++
		if v.LiveSize != c.Active {
			w.log.Printf("watcher: %s/%s LIVE; reconciling active %d -> %d (on-chain)", k.MakerPubkey, k.OfferID, c.Active, v.LiveSize)
		}
		err = w.book.ReconcileLive(k, v.LiveSize)
	case StatePartialFill, StateFilled:
		if v.State == StatePartialFill {
			st.PartialFill++
		} else {
			st.Filled++
		}
		err = w.recordSpend(c, state, a, anchorKnown)
	case StateUnconfirmedSpend:
		st.Pending++
		w.log.Printf("watcher: %s/%s spend %s unconfirmed; holding pending", k.MakerPubkey, k.OfferID, short(v.SpenderTxid))
		err = w.book.HoldForSpend(k, v.SpenderTxid)
	case StateGhost:
		st.Ghost++
		// F3 / anchoring supremacy: a Ghost is a funding UTXO gone at the tip. For an ORIGINAL funding
		// (never re-rested) that means it never really existed (never-confirmed / reorged-away before any
		// fill) — remove it. But for a RE-RESTED order the gone outpoint is a remainder from a prior
		// partial fill, and its disappearance most likely means a Bitcoin reorg UNDID that fill; the
		// earlier outpoint is (or will be) restored. Permanently invalidating it would violate Principle 1
		// (the trade the reorg erased should un-happen, not the order). So HOLD it (hidden, reversible):
		// if the reorg heals and the fill re-confirms, a later pass sees the remainder again and re-opens it.
		if c.ReRested {
			w.log.Printf("watcher: %s/%s GHOST of a re-rested remainder — %s; HOLDING (reorg may restore the fill), not invalidating", k.MakerPubkey, k.OfferID, v.Reason)
			err = w.book.HoldGhost(k, v.Reason)
		} else {
			w.log.Printf("watcher: %s/%s GHOST — %s; removing (anchoring supremacy: UTXO gone at tip)", k.MakerPubkey, k.OfferID, v.Reason)
			err = w.book.RemoveGhost(k, v.Reason)
		}
	}
	if err != nil {
		st.Errors++
		w.log.Printf("watcher: %s/%s apply %s: %v", k.MakerPubkey, k.OfferID, v.State, err)
	}
}

// recordSpend records a confirmed spend of a resting order's outpoint: a fill
// (its record read from the spender's outputs, its trade, and the order resting
// again at 2k+1 or held until the fill is final) or a refund (held, then
// removed, with no trade).
func (w *Watcher) recordSpend(c CovEntry, state CovState, a AnchorTip, anchorKnown bool) error {
	k := c.Key
	prevTxid, prevVout := c.outpoint()
	sp := state.Spender
	if sp == nil {
		return fmt.Errorf("spender %s of %s:%d not decoded", short(state.SpenderTxid), prevTxid, prevVout)
	}
	locked := sp.PrevValue
	r, err := c.readFill(sp, locked)
	if err != nil {
		return err
	}
	ts, err := w.chain.TxStatus(sp.Txid)
	if err != nil {
		return err
	}
	if !ts.InChain {
		return w.book.HoldForSpend(k, sp.Txid) // it left the block between two calls; next pass sees it
	}
	s := &step{
		prevTxid: prevTxid, prevVout: prevVout, prevSize: locked,
		prevReRested: c.ReRested, prevOffer: c.Offer,
		spender: sp.Txid, k: sp.Input, refund: r.Refund, partial: r.Partial, required: w.required(c.Offer),
	}
	tr := w.tracks[k]
	if tr == nil {
		tr = &track{}
		w.tracks[k] = tr
	}
	depth, certified, final := finality(ts, a, anchorKnown, s.required)
	if r.Refund {
		w.log.Printf("watcher: %s/%s taken back by its maker in %s (not its FILL leaf); held until final, then removed with no trade", k.MakerPubkey, k.OfferID, short(sp.Txid))
		tr.steps = append(tr.steps, s)
		if err := w.book.HoldForSpend(k, sp.Txid); err != nil {
			return err
		}
	} else {
		if r.Disagrees != "" {
			w.log.Printf("watcher: %s/%s fill %s: the chain took what the leaf, as this relay reads it, refuses (%s); recording the outputs as they are", k.MakerPubkey, k.OfferID, short(sp.Txid), r.Disagrees)
		}
		rec := offerstore.CovenantFill{
			Order: k, Pair: c.Offer.GetPair(), Txid: sp.Txid, Input: sp.Input,
			Spent:  fmt.Sprintf("%s:%d", prevTxid, prevVout),
			Locked: locked, Filled: r.Filled, Remainder: r.Remainder,
			Credit: r.Credit, Rest: r.Rest, Gap: r.Gap,
			Taker: offerstore.FillTaker{Via: offerstore.TakerChain},
			Block: ts.Block, Height: ts.Height, Certified: certified, AnchorDepth: depth, RequiredDepth: s.required,
			State: offerstore.FillConfirmed,
		}
		if final {
			rec.State = offerstore.FillFinal
		}
		if r.Clearing != nil {
			rec.Taker = offerstore.FillTaker{Via: offerstore.TakerClearingRound, RoundTxid: sp.Txid,
				Clearer: hex.EncodeToString(c.Round.GetClearer()), Price: r.Clearing,
				Note: "a clearing round: every round order in it is credited at the one price its last output publishes"}
		}
		if w.rounds != nil && r.Clearing == nil {
			ri, ok, rerr := w.rounds.Round(sp.Txid, sp.Outputs)
			if rerr != nil {
				w.log.Printf("watcher: %s/%s fill %s: ask the operators whether it is a round: %v", k.MakerPubkey, k.OfferID, short(sp.Txid), rerr)
			}
			if ok {
				rec.Taker = offerstore.FillTaker{
					Via: offerstore.TakerArcaRound, Operator: ri.Operator, RoundTxid: sp.Txid, BatchVouts: ri.BatchVouts,
					Note: "the taker's side is a leaf in the round's batch: no output of the round pays the taker",
				}
			}
		}
		s.record = rec
		tr.steps = append(tr.steps, s)
		w.book.RecordFill(rec, c.Offer)
		taker := rec.Taker.Via
		if rec.Taker.Via == offerstore.TakerArcaRound {
			taker = fmt.Sprintf("Arca round of operator %s, batch %v", short(rec.Taker.Operator), rec.Taker.BatchVouts)
		}
		w.log.Printf("watcher: %s/%s FILL %s at input %d: credit %d of %s at %d; %s; taker: %s; %s (anchor depth %d of %d, certified %v)",
			k.MakerPubkey, k.OfferID, short(sp.Txid), sp.Input, rec.Credit.Value, short(rec.Credit.Asset), rec.Credit.Vout,
			restNote(rec), taker, rec.State, depth, s.required, certified)
		if r.Partial {
			if err := w.book.Repoint(k, sp.Txid, sp.Input*2+1, r.Remainder, true); err != nil {
				return err
			}
		} else if err := w.book.HoldForSpend(k, sp.Txid); err != nil {
			return err
		}
	}
	w.book.SetAnchorConfs(k, uint32(depth))
	if final {
		s.final = true
		if !s.partial {
			s.removed = true
			if s.refund {
				return w.book.RemoveRefunded(k, sp.Txid)
			}
			return w.book.RemoveFilled(k, sp.Txid)
		}
	}
	return nil
}

func restNote(f offerstore.CovenantFill) string {
	switch {
	case f.Rest != nil:
		return fmt.Sprintf("rests again with %d at %d", f.Remainder, f.Rest.Vout)
	case f.Gap != nil:
		return fmt.Sprintf("filled whole (%d of %s at %d)", f.Gap.Value, short(f.Gap.Asset), f.Gap.Vout)
	default:
		return "filled whole"
	}
}

// prune drops the oldest final steps of orders that left the book once the
// watcher holds more than maxTrackedSteps.
func (w *Watcher) prune() {
	n := 0
	for _, tr := range w.tracks {
		n += len(tr.steps)
	}
	if n <= maxTrackedSteps {
		return
	}
	for k, tr := range w.tracks {
		if n <= maxTrackedSteps {
			return
		}
		if len(tr.steps) > 0 && tr.steps[len(tr.steps)-1].removed {
			n -= len(tr.steps)
			delete(w.tracks, k)
		}
	}
}

// expectFromTerms derives the covenant program (scriptPubKey + display asset id)
// from the advertised CovenantTerms using pkg/covenant — the golden-vector-proven
// module — rather than trusting the advertised spk.
func expectFromTerms(t *seqobv1.CovenantTerms) (Expect, error) {
	o, err := orderFromTerms(t)
	if err != nil {
		return Expect{}, err
	}
	tap, err := o.Derive()
	if err != nil {
		return Expect{}, fmt.Errorf("derive taptree: %w", err)
	}
	return Expect{
		OrderSPKHex:   hex.EncodeToString(tap.ScriptPubKey),
		AssetADisplay: reverseHex(t.GetAssetA()),
		MinLot:        t.GetMinLot(),
	}, nil
}

// orderFromTerms rebuilds a covenant.Order from CovenantTerms. Asset ids are
// INTERNAL-order hex (as on-chain introspection returns them); maker_prog,
// maker_x and internal_key are raw bytes. Mirrors settler.orderFromTerms.
func orderFromTerms(t *seqobv1.CovenantTerms) (covenant.Order, error) {
	var o covenant.Order
	a, err := hex32(t.GetAssetA())
	if err != nil {
		return o, fmt.Errorf("asset_a: %w", err)
	}
	b, err := hex32(t.GetAssetB())
	if err != nil {
		return o, fmt.Errorf("asset_b: %w", err)
	}
	if len(t.GetMakerProg()) != 32 {
		return o, fmt.Errorf("maker_prog must be 32 bytes")
	}
	if len(t.GetMakerX()) != 32 {
		return o, fmt.Errorf("maker_x must be 32 bytes")
	}
	var mx, ik [32]byte
	copy(mx[:], t.GetMakerX())
	if ikb := t.GetInternalKey(); len(ikb) == 32 {
		copy(ik[:], ikb)
	} else {
		ik = covenant.NUMS
	}
	return covenant.Order{
		AssetA:         a,
		AssetB:         b,
		RateNum:        t.GetRateNum(),
		RateDen:        t.GetRateDen(),
		MakerProg:      t.GetMakerProg(),
		MakerVer:       int(t.GetMakerProgVer()),
		MinLot:         t.GetMinLot(),
		ExpiryLocktime: int64(t.GetExpiryLocktime()),
		MakerX:         mx,
		InternalKey:    ik,
	}, nil
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("want 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// reverseHex reverses the byte order of a hex string (internal <-> display asset
// id). Returns "" on malformed input (caller compares, so a mismatch is safe).
func reverseHex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
