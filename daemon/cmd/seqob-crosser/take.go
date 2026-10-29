// take.go — the one-shot taker (-take): one trade against the book, then exit.
//
// A market taker that trades against the makers the way a person's wallet does:
// it reads the pair's book from every relay (both orientations, liftable offers
// only, sized to what each relay says is left), picks the best offer on the side
// it trades whose rail is one it may use, sizes the take to that one offer, runs
// the same leg the crossing engine runs (RunLeg), and prints one JSON line:
//
//	{"settled":true,"family":"pureln","side":"buy","offer":"<maker>:<id>","relay":"…",
//	 "base_atoms":123,"quote_atoms":456,"ms":231}
//
// It is how a load driver (sequentia-testnet-ops makers/volume-takers.mjs) turns
// a schedule of trades into real, settled trades on the rails it names: every
// trade it makes is a lift, fill or swap the makers' own code settles and the
// relays record. Nothing is crossed and nothing is journaled: a failed take has
// moved nothing of the taker's, except a leg that reports a state file, which it
// prints for a resume.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TakeResult is the one line a -take run prints.
type TakeResult struct {
	Settled    bool   `json:"settled"`
	Family     string `json:"family,omitempty"`
	Side       string `json:"side"`
	Offer      string `json:"offer,omitempty"`
	Relay      string `json:"relay,omitempty"`
	BaseAtoms  uint64 `json:"base_atoms,omitempty"`
	QuoteAtoms uint64 `json:"quote_atoms,omitempty"`
	Ms         int64  `json:"ms"`
	Err        string `json:"error,omitempty"`
	StateFile  string `json:"state_file,omitempty"`
	Resume     string `json:"resume,omitempty"`
}

// SizeOne sizes a take of about `want` canonical-base atoms against one offer,
// the rules ClampOverlap applies to a leg: the whole offer is always legal; a
// slice needs a sliceable driver, allow_partial, min_fill and, for a covenant,
// min_lot and a remainder that is 0 or at least min_lot. Where the legal size
// differs from `want`, it may grow to at most twice `want` (a whole small offer,
// a min_fill) or shrink; 0 with a reason when no legal size is near `want`.
func SizeOne(n *NormOrder, want uint64) (uint64, string) {
	if want == 0 || n.BaseSize == 0 {
		return 0, "nothing to take"
	}
	limit := want * 2
	if n.BaseSize <= want || (n.BaseSize <= limit && (!n.Sliceable || !n.AllowPartial)) {
		return n.BaseSize, "" // the whole offer
	}
	if !n.Sliceable || !n.AllowPartial {
		return 0, fmt.Sprintf("whole-take only, size %d is over twice %d", n.BaseSize, want)
	}
	take := want
	if n.MinFill > take {
		if n.MinFill > limit {
			return 0, fmt.Sprintf("min_fill %d is over twice %d", n.MinFill, want)
		}
		take = n.MinFill
	}
	if n.Family == FamCovenant && n.MinLot > 0 {
		if take < n.MinLot {
			if n.MinLot > limit {
				return 0, fmt.Sprintf("min_lot %d is over twice %d", n.MinLot, want)
			}
			take = n.MinLot
		}
		if rem := n.BaseSize - take; rem > 0 && rem < n.MinLot {
			if n.BaseSize <= limit {
				return n.BaseSize, ""
			}
			if n.BaseSize-n.MinLot < n.MinLot {
				return 0, "covenant leg cannot leave a valid remainder"
			}
			take = n.BaseSize - n.MinLot
		}
	}
	if take > n.BaseSize {
		take = n.BaseSize
	}
	return take, ""
}

// PickTake chooses the offer a taker buying (side "buy": it takes asks) or
// selling ("sell": it takes bids) about `want` base atoms trades against: the
// best-priced executable offer of an allowed family that can be sized near
// `want`, price-time priority as the crossing engine ranks them. `avoid` names
// makers it never takes from (its own).
func PickTake(orders []*NormOrder, side string, want uint64, families map[string]bool, canExec Executability, avoid map[string]bool) (*NormOrder, uint64, string) {
	wantSide := SideAsk
	if side == "sell" {
		wantSide = SideBid
	}
	var cands []*NormOrder
	var why []string
	for _, n := range orders {
		if n.Side != wantSide || (len(families) > 0 && !families[n.Family.String()]) || avoid[n.Offer.GetMakerPubkey()] {
			continue
		}
		if ok, r := canExec(n); !ok {
			why = append(why, n.Family.String()+": "+r)
			continue
		}
		cands = append(cands, n)
	}
	SortBook(cands)
	for _, n := range cands {
		if t, r := SizeOne(n, want); t > 0 {
			return n, t, ""
		} else {
			why = append(why, n.Family.String()+": "+r)
		}
	}
	if len(why) > 3 {
		why = append(why[:3], fmt.Sprintf("and %d more", len(why)-3))
	}
	return nil, 0, fmt.Sprintf("no %s offer on the allowed rails to take %d base atoms from (%d candidates; %s)",
		wantSide, want, len(cands), strings.Join(why, "; "))
}

// RunTake is the -take run: one trade, its result.
func RunTake(ex *Executor, relays []string, base, quote, side string, want uint64, families map[string]bool, avoid map[string]bool,
	minTTL time.Duration, logf func(string, ...interface{})) TakeResult {
	t0 := time.Now()
	res := TakeResult{Side: side}
	if side != "buy" && side != "sell" {
		res.Err = "-take-side must be buy or sell"
		return res
	}
	raw, statuses := FetchPairOffers(relays, base, quote, logf)
	union := ApplyStatuses(BuildUnion(raw, base, quote, time.Now(), minTTL, logf), statuses)
	n, take, why := PickTake(union, side, want, families, ex.Caps.Executable, avoid)
	if n == nil {
		res.Err = why
		res.Ms = time.Since(t0).Milliseconds()
		return res
	}
	res.Family, res.Offer, res.Relay, res.BaseAtoms = n.Family.String(), n.ID(), n.Relay, take
	if why := ex.Preflight(n, take); why != "" {
		res.Err = "preflight: " + why
		res.Ms = time.Since(t0).Milliseconds()
		return res
	}
	if side == "buy" {
		res.QuoteAtoms = n.CostFor(take)
	} else {
		res.QuoteAtoms = n.RevenueFor(take)
	}
	r := ex.RunLeg(n, take)
	res.Settled, res.Err, res.StateFile, res.Resume = r.Settled, r.Err, r.StateFile, r.Resume
	res.Ms = time.Since(t0).Milliseconds()
	return res
}

// printTake writes the result as one JSON line.
func printTake(r TakeResult) string {
	b, _ := json.Marshal(r)
	return string(b)
}
