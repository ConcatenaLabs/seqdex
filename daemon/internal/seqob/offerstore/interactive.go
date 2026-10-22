package offerstore

import (
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"google.golang.org/protobuf/proto"
)

// --- interactive fill records -------------------------------------------------
//
// A same-chain lift of an interactive maker's offer settles in one transaction
// the maker co-signed; its maker acknowledges it (SettleAck) with the settling
// txid, and the relay records the trade then. The chain watcher follows that
// transaction as it follows a covenant fill, under the same finality rule, and
// keeps a record of it beside the pair's covenant fills (Taker.Via
// "interactive"; no outputs are read):
//
//	pending   -> confirmed  when its transaction is in a block
//	confirmed -> final      when its block is certified and its anchor buried
//	                        the required depth
//	any       -> undone     when its block leaves the active chain (an anchor
//	                        reorg), with the reason; its trade is retracted
//
// An undone fill whose transaction is mined again is recorded again, with its
// trade. Records are served with the covenant fills at
// GET /v1/market/{base}/{quote}/fills, so a taker reads its fill's standing
// from the relay, the one place the rule is applied.

// FillPending is an interactive fill whose transaction the watcher has not yet
// seen in a block.
const FillPending = "pending"

// TakerInteractive: the fill is a same-chain lift its maker co-signed online.
const TakerInteractive = "interactive"

// InteractiveWatch is one interactive fill the watcher follows.
type InteractiveWatch struct {
	Order    Key
	Offer    *seqobv1.Offer // the offer as it rested when the lift settled
	Txid     string
	Filled   uint64
	Required int64 // the offer's min_anchor_depth (the watcher raises it to its own)
	Traded   bool  // its trade is recorded (false after an undo, until mined again)
}

// maxInteractiveWatches bounds the fills followed; past it the oldest go first.
const maxInteractiveWatches = 5000

// WatchInteractiveFill registers a settled lift for the watcher, with a pending
// record of it on the pair. Its trade is recorded by the caller.
func (s *Store) WatchInteractiveFill(k Key, resting *seqobv1.Offer, filled uint64, txid string) {
	if resting == nil || resting.GetPair() == nil || txid == "" {
		return
	}
	o := proto.Clone(resting).(*seqobv1.Offer)
	s.mu.Lock()
	for _, w := range s.interactive {
		if w.Order == k && w.Txid == txid {
			s.mu.Unlock()
			return
		}
	}
	s.interactive = append(s.interactive, &InteractiveWatch{Order: k, Offer: o, Txid: txid, Filled: filled,
		Required: int64(o.GetMinAnchorDepth()), Traded: true})
	if len(s.interactive) > maxInteractiveWatches {
		s.interactive = s.interactive[len(s.interactive)-maxInteractiveWatches:]
	}
	s.mu.Unlock()
	s.RecordCovenantFill(CovenantFill{Order: k, Pair: o.GetPair(), Txid: txid, Filled: filled,
		Taker: FillTaker{Via: TakerInteractive}, State: FillPending}, nil)
}

// InteractiveWatches returns the interactive fills the watcher follows.
func (s *Store) InteractiveWatches() []InteractiveWatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]InteractiveWatch, 0, len(s.interactive))
	for _, w := range s.interactive {
		out = append(out, *w)
	}
	return out
}

// InteractiveRecord returns the live (not undone) record of an interactive
// fill, if one is held.
func (s *Store) InteractiveRecord(k Key, pair *seqobv1.AssetPair, txid string) (CovenantFill, bool) {
	if pair == nil {
		return CovenantFill{}, false
	}
	id := fillID(k, txid, 0)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.fills[pairKey(pair)] {
		if fillID(f.Order, f.Txid, f.Input) == id && f.State != FillUndone {
			return *f, true
		}
	}
	return CovenantFill{}, false
}

// SetInteractiveTraded records whether a followed fill's trade stands, and
// records it again when a fill undone is mined again.
func (s *Store) SetInteractiveTraded(k Key, txid string, traded bool) {
	s.mu.Lock()
	var w *InteractiveWatch
	for _, x := range s.interactive {
		if x.Order == k && x.Txid == txid {
			w = x
			break
		}
	}
	if w == nil || w.Traded == traded {
		s.mu.Unlock()
		return
	}
	w.Traded = traded
	o, filled := w.Offer, w.Filled
	s.mu.Unlock()
	if traded {
		s.RecordTradeTx(o, filled, txid)
	}
}
