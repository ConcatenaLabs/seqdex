package main

// restricted_serve.go is the event loop both restricted makers run: restricted.go
// (bitcoin as the quote) and restricted_dvp.go (an asset as the quote). One offer at a
// time, whole fills only, one lift in flight; the relay's frames are routed to the
// lift's goroutine, and a dropped connection is dialled again without stopping a lift
// that is moving coins.

import (
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
	// lift runs one lift of offer o to its end: settled with the trade's txid, or not
	// (the offer stays).
	lift func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (txid string, settled bool, err error)
}

func (s *restrictedServer) serve(o *seqobv1.Offer) {
	var mu sync.Mutex
	inboxes := map[string]chan []byte{}
	inFlight, filled := 0, false
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })
	armRequoteExit(o, func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 })
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
			if !filled {
				resubmit = o
				_ = refreshOfferForRequote(o, s.expiry, s.key)
			}
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
			if lr.GetOfferId() != o.GetOfferId() {
				refuse(sid, cr, "stale_offer", "this maker process serves offer "+o.GetOfferId()+", not "+lr.GetOfferId())
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
			go func(sid string, o *seqobv1.Offer, in chan []byte) {
				txid, settled, err := s.lift(sid, o, cr, in, send)
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				if settled {
					filled = true
				}
				mu.Unlock()
				if !settled {
					fmt.Printf("lift %s ended: %v (the offer stays)\n", sid, err)
					return
				}
				reportSettledTrade(s.ws, o, s.key, s.whole, txid)
				fmt.Printf("lift %s traded in %s; retiring the offer\n", sid, txid)
				cancelOffer(s.relay, o, s.key)
			}(sid, o, in)
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
