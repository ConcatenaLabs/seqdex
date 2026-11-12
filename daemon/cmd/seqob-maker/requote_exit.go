// Exit-for-requote: keep the book from silently losing this maker's offer.
//
// A resting offer has a hard expiry; the relay's sweep evicts it at that
// moment. Before this file, the maker noticed only on its next WS drop, then
// re-posted the ORIGINAL (already-expired) copy, which the validator rejected
// with "offer already expired" — so on a calm network the whole book emptied
// at expiry and stayed empty until someone manually restarted the fleet (288
// such rejections in the fleet logs on 2026-07-19).
//
// The uniform cure across every mode: shortly BEFORE expiry, the maker exits
// cleanly. Every maker runs under a supervisor (supervise-*.sh loop or a
// systemd unit) that immediately re-runs it, which re-reads the price oracle —
// so a re-quote is also a re-PRICE, and the book never rests a stale quote.
// The exit waits for any in-flight swap: sessions carry funds mid-HTLC, so we
// only retire when idle and otherwise exit the moment the session finishes.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// requoteExitMargin is how long before offer expiry the maker retires itself.
// Must exceed the relay's MinExpiry (30s) so a re-run always posts a valid window.
const requoteExitMargin = 60 * time.Second

// maxQuoteAge, when set, retires the maker for a re-price no later than this after
// it posted, regardless of the offer's expiry. The maker has no price feed; its
// quote is the flag pair for the whole -expiry window (an hour by default), and
// the supervisor re-runs it at the current price on exit. A one-hour-old quote is
// a free option on a moving market.
var maxQuoteAge time.Duration

// requotePending is set when the re-quote came due while a swap was in flight;
// the session's completion path checks it and exits then. One offer per
// process, so package scope is exact.
var requotePending atomic.Bool

// requoteDelay returns how long to wait before the exit-for-requote: margin
// before expiry, floored so an already-near-expired offer still gets a moment
// to serve. Pure, for tests.
func requoteDelay(expiresAtUnix uint64, now time.Time) time.Duration {
	if expiresAtUnix == 0 {
		return 0 // no expiry -> no requote exit
	}
	d := time.Unix(int64(expiresAtUnix), 0).Add(-requoteExitMargin).Sub(now)
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// offerRejected reports a relay error frame that means our offer is NOT
// resting (validator rejection: expired, bad window, ...). A maker whose offer
// was rejected must re-quote; sitting on a healthy socket with no resting
// offer is exactly the silent-empty-book failure mode.
func offerRejected(code uint32, msg string) bool {
	// A rate-limit refusal means the relay would not take MORE, not that the resting
	// offer is gone: exiting on it made a ladder re-post storm into a crash loop
	// (restart, same pubkey, same exhausted budget, rejected again).
	if strings.Contains(msg, "rate limit") {
		return false
	}
	return code == 400 && strings.Contains(msg, "invalid offer")
}

// armRequoteExit schedules the exit-for-requote for o. idle must report
// whether no swap/lift session is in flight (called from the timer goroutine —
// take the loop's own lock inside).
func armRequoteExit(o *seqobv1.Offer, idle func() bool) {
	d := requoteDelay(o.GetExpiresAtUnix(), time.Now())
	reason := "offer expiring"
	if maxQuoteAge > 0 && (d == 0 || maxQuoteAge < d) {
		d, reason = maxQuoteAge, "quote older than -max-quote-age"
	}
	if d == 0 {
		return
	}
	time.AfterFunc(d, func() { requoteExitIfIdle(reason, idle) })
}

// requoteExitIfIdle exits 0 (supervisor re-runs at a fresh price) when idle;
// otherwise marks the exit pending and re-checks periodically in case the
// in-flight completion path never fires (defense in depth — the session's own
// timeouts should get there first).
func requoteExitIfIdle(reason string, idle func() bool) {
	if idle() {
		fmt.Printf("%s; exiting for a fresh re-quote (supervisor re-runs at the current price)\n", reason)
		os.Exit(0)
	}
	if requotePending.CompareAndSwap(false, true) {
		fmt.Printf("%s; re-quote pending until the in-flight swap completes\n", reason)
	}
	time.AfterFunc(5*time.Minute, func() { requoteExitIfIdle(reason, idle) })
}

// requoteExitIfPending is called when a session finishes: if the re-quote came
// due mid-swap, retire now that we are idle.
func requoteExitIfPending(idle func() bool) {
	if requotePending.Load() && idle() {
		fmt.Println("in-flight swap done; exiting for a fresh re-quote (supervisor re-runs)")
		os.Exit(0)
	}
}

// exitAfterFill ends a maker whose offer was filled whole and is not quoted again in the
// process (no -requote): its supervisor runs it again at the current price. Such a maker
// used to wait for its websocket to fail before exiting, and on a healthy connection sat
// with no offer, which the book shows as no one buying or selling (seqdex #165).
func exitAfterFill(idle func() bool) {
	requoteExitIfIdle("offer filled", idle)
}
