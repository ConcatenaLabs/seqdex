package main

// restricted_serve.go is the event loop both restricted makers run: restricted.go
// (bitcoin as the quote) and restricted_dvp.go (an asset as the quote). One offer at a
// time, whole fills only, one lift in flight; the relay's frames are routed to the
// lift's goroutine, and a dropped connection is dialled again without stopping a lift
// that is moving coins.
//
// After a fill the maker retires the filled offer and posts its next quote on the same
// connection, after the same checks as at start. When it cannot quote (it holds too
// little), it says so and exits non-zero after a pause, so its supervisor retries: it
// never sits connected with no offer, which the book would show as no one buying or
// selling.
//
// Each quote has its own exit-for-requote, armed when it is posted and void once a fill
// has replaced it, and every exit withdraws the quote it leaves: a process that ends never
// leaves a quote on the relay that no process serves. At start the maker withdraws the
// quotes an earlier run of it left (same key, same pair, same side, not a gated order).

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
)

type restrictedServer struct {
	relay  string
	wsURL  string
	key    *btcec.PrivateKey
	expiry time.Duration
	whole  uint64 // the offer's size in the restricted asset: a lift takes all of it
	ws     *crossWS
	// quote checks the maker can fill a new offer now and returns it signed.
	quote func() (*seqobv1.Offer, error)
	// exit ends the process (os.Exit; a test replaces it).
	exit func(code int)
	// retryPause is how long the maker waits before exiting when it cannot quote again,
	// so a supervisor's restarts do not spin.
	retryPause time.Duration
	// lift runs one lift of offer o to its end: settled with the trade's txid, or not
	// (the offer stays).
	lift func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (txid string, settled bool, err error)
	// due says when offer o's exit-for-requote comes due (0: never); requoteDue unless a
	// test sets it.
	due func(o *seqobv1.Offer) (time.Duration, string)
}

// withdraw cancels offer o on the relay; an offer the relay no longer holds is withdrawn
// already.
func (s *restrictedServer) withdraw(o *seqobv1.Offer) {
	if err := postCancel(s.relay, o, s.key); err != nil && !cancelAlreadyGone(err) {
		fmt.Printf("withdraw offer %s: %v (the relay drops it a minute after this process ends)\n", o.GetOfferId(), err)
		return
	}
	fmt.Printf("offer %s withdrawn\n", o.GetOfferId())
}

// withdrawEarlier cancels the quotes an earlier run of this maker left on the relay: its
// key's restricted quotes on this pair and side, other than first. A maker killed, or
// ended before its exit could withdraw, leaves one; the relay would otherwise keep it
// beside the new quote while its key is connected, and no process would serve it. A
// gated order under the same key is a resting order, not a quote, and stays.
func (s *restrictedServer) withdrawEarlier(first *seqobv1.Offer) {
	pub := hex.EncodeToString(s.key.PubKey().SerializeCompressed())
	dir := first.GetTradeDir().String()
	only := func(b bookOffer) bool {
		return b.TradeDir == dir && present(b.Restricted) && !present(b.GatedOrder)
	}
	p := first.GetPair()
	if n := withdrawStale(s.relay, p.GetBaseAsset(), p.GetQuoteAsset(), pub, map[string]bool{first.GetOfferId(): true}, s.key, only); n > 0 {
		fmt.Printf("withdrew %d quote(s) an earlier run of this maker left on the relay\n", n)
	}
}

func (s *restrictedServer) serve(first *seqobv1.Offer) {
	if s.due == nil {
		s.due = requoteDue
	}
	var mu sync.Mutex
	o := first // the offer served now; replaced, under mu, by the next quote after a fill
	current := func() *seqobv1.Offer { mu.Lock(); defer mu.Unlock(); return o }
	inboxes := map[string]chan []byte{}
	inFlight, filled := 0, false
	retiring := false          // the process is withdrawing its quote to exit: no new lift
	var exitDue *seqobv1.Offer // the quote whose exit came due during a lift
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })
	// A shutdown by signal withdraws the quote too, once its drain is done.
	setExitHook(func() {
		mu.Lock()
		x, live := o, !filled && !retiring
		retiring = true
		mu.Unlock()
		if live {
			s.withdraw(x)
		}
	})
	// retire ends the process for a fresh quote at the current price, withdrawing quote x
	// first. It acts only while x is the quote served (a fill has not replaced it), and
	// waits for a lift in flight to end.
	var retire func(x *seqobv1.Offer, reason string)
	retire = func(x *seqobv1.Offer, reason string) {
		mu.Lock()
		if o != x || filled || retiring {
			mu.Unlock()
			return
		}
		if inFlight > 0 {
			exitDue = x
			mu.Unlock()
			fmt.Printf("%s; re-quote pending until the lift in flight ends\n", reason)
			time.AfterFunc(5*time.Minute, func() { retire(x, reason) })
			return
		}
		retiring = true
		mu.Unlock()
		s.withdraw(x)
		fmt.Printf("%s; exiting for a fresh re-quote (the supervisor runs this maker again at the current price)\n", reason)
		s.exit(0)
	}
	arm := func(x *seqobv1.Offer) {
		if d, reason := s.due(x); d > 0 {
			time.AfterFunc(d, func() { retire(x, reason) })
		}
	}
	arm(o)
	s.withdrawEarlier(first)
	refuse := func(sid string, cr *client.Crypter, code, msg string) {
		m := &client.XcMsg{Type: client.XcFail, Code: code, Message: msg}
		if sealed, err := m.Seal(cr); err == nil {
			_ = s.ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		}
	}
	for {
		_, data, err := s.ws.current().ReadMessage()
		if err != nil {
			mu.Lock()
			done := filled && inFlight == 0
			mu.Unlock()
			if done {
				fmt.Println("offer filled and no lift in flight; exiting (restart to quote again)")
				s.exit(0)
				return
			}
			var resubmit *seqobv1.Offer
			mu.Lock()
			if !filled {
				resubmit = o
				_ = refreshOfferForRequote(o, s.expiry, s.key)
			}
			mu.Unlock()
			fmt.Printf("ws read error: %v; reconnecting (lifts in flight continue on the chain)\n", err)
			s.ws.redialLoop(s.wsURL, resubmit)
			reattachSessions(s.ws, s.key, &mu, inboxes)
			continue
		}
		var from seqobv1.From
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			sid := lr.GetSessionId()
			cr, err := client.NewMakerCrypterFromLift(s.key, lr.GetTakerSessionPubkey())
			if err != nil {
				continue
			}
			served := current()
			if lr.GetOfferId() != served.GetOfferId() {
				refuse(sid, cr, "stale_offer", "this maker process serves offer "+served.GetOfferId()+", not "+lr.GetOfferId())
				continue
			}
			if lr.GetTakeAmount() != 0 && lr.GetTakeAmount() != s.whole {
				refuse(sid, cr, "whole_fill_only", fmt.Sprintf("this offer fills whole: %d", s.whole))
				continue
			}
			if refuseIfDraining(sid, cr, refuse) {
				continue
			}
			mu.Lock()
			if retiring {
				mu.Unlock()
				refuse(sid, cr, "draining", "this maker is withdrawing this quote for a fresh one; retry on another offer")
				continue
			}
			if inFlight > 0 || filled {
				mu.Unlock()
				refuse(sid, cr, "busy", "a lift of this offer is in flight")
				continue
			}
			inFlight++
			in := make(chan []byte, 16)
			inboxes[sid] = in
			mu.Unlock()
			send := func(sealed []byte) error {
				return s.ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			go func(sid string, lifted *seqobv1.Offer, in chan []byte) {
				txid, settled, err := s.lift(sid, lifted, cr, in, send)
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				if settled {
					filled = true
				}
				due := exitDue
				exitDue = nil
				mu.Unlock()
				if !settled {
					fmt.Printf("lift %s ended: %v (the offer stays)\n", sid, err)
					if due != nil {
						retire(due, "the lift in flight ended")
					}
					return
				}
				reportSettledTrade(s.ws, lifted, s.key, s.whole, txid)
				fmt.Printf("lift %s traded in %s; retiring the offer\n", sid, txid)
				cancelOffer(s.relay, lifted, s.key)
				s.quoteAgain(&mu, &o, &filled, arm)
			}(sid, served, in)
		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in != nil {
				select {
				case in <- sm.GetCiphertext():
				default:
				}
			}
		case from.GetError() != nil:
			releaseDetachedSession(from.GetError(), &mu, inboxes)
		}
	}
}

// quoteAgain posts the next quote after a fill, or exits non-zero when the maker cannot
// make one (its supervisor retries).
// The next quote's own exit-for-requote is armed once it is posted.
func (s *restrictedServer) quoteAgain(mu *sync.Mutex, o **seqobv1.Offer, filled *bool, arm func(*seqobv1.Offer)) {
	next, err := s.quote()
	if err != nil {
		fmt.Printf("cannot quote again: %v; exiting for the supervisor to retry\n", err)
		time.Sleep(s.retryPause)
		s.exit(1)
		return
	}
	mu.Lock()
	*o, *filled = next, false
	mu.Unlock()
	arm(next)
	if err := s.ws.write(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: next}}); err != nil {
		// The read loop meets the dropped connection and dials again with this offer.
		fmt.Printf("next quote %s not sent (%v); it goes with the reconnection\n", next.GetOfferId(), err)
		return
	}
	fmt.Printf("posted the next quote: offer %s\n", next.GetOfferId())
}
