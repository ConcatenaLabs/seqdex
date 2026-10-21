package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

// wsConn wraps a websocket connection with a single-writer lock and its
// per-connection relay state (subscriptions, joined sessions, registered pubkey).
type wsConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu          sync.Mutex
	subs        map[string]func() // pairKey -> stop forwarder
	roles       map[string]session.Role
	makerPubkey string

	// closed is closed exactly once when this connection is torn down, so its session
	// pumps and ping loop stop promptly (a lingering pump would steal a re-attached
	// session's courier frames, P3.8).
	closed    chan struct{}
	closeOnce sync.Once
}

// stop signals every goroutine bound to this connection (session pumps, ping loop)
// to exit. Idempotent.
func (c *wsConn) stop() { c.closeOnce.Do(func() { close(c.closed) }) }

// writeWait bounds a single WS write. Without it, a maker whose OS receive buffer is full
// blocks WriteMessage forever while holding writeMu — and because routeMatches/notifyMaker
// call another maker's send() synchronously from the SUBMITTER's read loop, one slow maker
// could wedge a submitter (head-of-line blocking). A deadline turns that into a fast error
// the caller cleans up instead of a silent hang.
const writeWait = 10 * time.Second

// P3.8 keepalive: the relay pings every pingPeriod and expects a pong (or any read)
// within pongWait, else the read deadline fires and the dead connection is reaped.
// Without this a half-open TCP connection (peer vanished, no FIN) would pin the
// maker's offers as unliftable ghosts until their TTL. pingPeriod < pongWait.
const (
	pongWait   = 60 * time.Second
	pingPeriod = 50 * time.Second
)

// maxWSFrameBytes bounds one inbound WebSocket frame. Every legitimate To message is
// a signed offer, a lift, or a sealed courier frame of a few KiB; gorilla's default
// is unlimited, so without this one frame could allocate whatever the peer declares.
const maxWSFrameBytes = 256 << 10

// maxSubsPerConn bounds the per-pair market subscriptions one connection may hold.
// Each subscription is a forwarder goroutine plus a buffered delta channel, keyed by an
// arbitrary pair string the subscriber chooses, so without a cap one socket could hold
// an unbounded number and make every book event O(n) in its forwarders.
const maxSubsPerConn = 64

func (c *wsConn) send(from *seqobv1.From) error {
	b, err := jsonMarshal.Marshal(from)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *wsConn) sendErr(code uint32, msg string) {
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_Error{Error: &seqobv1.GenericError{Code: code, Message: msg}}})
}

// connRegistry maps a maker_pubkey to its (single, latest) live WS connection so
// a lift can be routed to an online maker. Best-effort (Phase-1).
type connRegistry struct {
	mu sync.Mutex
	m  map[string]*wsConn
}

func newConnRegistry() *connRegistry { return &connRegistry{m: make(map[string]*wsConn)} }

func (r *connRegistry) set(pubkey string, c *wsConn) {
	r.mu.Lock()
	r.m[pubkey] = c
	r.mu.Unlock()
}

func (r *connRegistry) get(pubkey string) (*wsConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[pubkey]
	return c, ok
}

// drop unregisters the connection and returns the maker pubkey(s) it was serving, so
// the caller can evict their now-unreachable offers.
func (r *connRegistry) drop(c *wsConn) []string {
	r.mu.Lock()
	dropped := make([]string, 0)
	for k, v := range r.m {
		if v == c {
			delete(r.m, k)
			dropped = append(dropped, k)
		}
	}
	r.mu.Unlock()
	return dropped
}

// handleWS upgrades to a websocket and serves the To/From envelope.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsConn{
		conn:   conn,
		subs:   make(map[string]func()),
		roles:  make(map[string]session.Role),
		closed: make(chan struct{}),
	}
	defer s.closeConn(c)

	// P3.8 keepalive: bound reads by a deadline the pong handler (and any inbound
	// frame) extends, and ping on a ticker so a half-open peer is reaped instead of
	// pinning its offers forever.
	conn.SetReadLimit(maxWSFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	go s.pingLoop(c)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Log the concrete close/read error (previously swallowed). A WS-drop storm is
			// undiagnosable without this — it distinguishes a stale-binary protocol EOF from a
			// write-deadline timeout, a peer close, or a genuine network error.
			if s.log != nil {
				c.mu.Lock()
				mp := c.makerPubkey
				c.mu.Unlock()
				s.log.Printf("ws closed (maker %.16s): %v", mp, err)
			}
			return
		}
		// Any inbound frame proves the peer is alive: push the read deadline out (a
		// chatty client that never pongs stays connected).
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		var to seqobv1.To
		if err := jsonUnmarshal.Unmarshal(data, &to); err != nil {
			c.sendErr(400, "decode To: "+err.Error())
			continue
		}
		s.dispatch(c, &to, clientIP(r))
	}
}

// pingLoop pings the peer every pingPeriod until the connection is torn down, so a
// half-open TCP connection (peer gone with no FIN) trips the read deadline and is
// reaped. WriteControl is safe to call concurrently with the single-writer send().
func (s *Server) pingLoop(c *wsConn) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

func (s *Server) closeConn(c *wsConn) {
	c.stop() // stop session pumps + ping loop first so they don't steal re-attach frames
	c.mu.Lock()
	for _, stop := range c.subs {
		stop()
	}
	c.subs = map[string]func(){}
	c.mu.Unlock()
	// Evict this maker's resting offers: in an interactive book the maker must be online
	// to co-sign a lift, so once its connection drops its offers are unliftable and would
	// otherwise linger as un-fillable "ghosts" until their TTL.
	for _, pubkey := range s.makerConns.drop(c) {
		s.store.RemoveByMaker(pubkey)
		s.evictGhostOffersAfterGrace(pubkey)
	}
	// Per-CONNECTION eviction: the pubkey paths above only fire when this connection was
	// the pubkey's registered endpoint at close time. A sibling under the same identity
	// key overwrites that registration, so the offers THIS connection posted are evicted
	// by ownership instead — after the same grace, sparing anything re-posted since.
	ownKeys := make([]offerstore.Key, 0)
	s.ownerMu.Lock()
	for k, owner := range s.offerOwners {
		if owner == c {
			ownKeys = append(ownKeys, k)
		}
	}
	s.ownerMu.Unlock()
	s.evictConnOffersAfterGrace(c, ownKeys)
	s.releaseAbandonedLifts(c)
	c.conn.Close()
}

// makerReconnectGrace is how long a departed maker's still-resting interactive offers
// are held before being evicted, giving a reconnecting settlement agent time to re-post.
var makerReconnectGrace = 60 * time.Second

// evictGhostOffersAfterGrace removes a maker's interactive offers once it is clear the
// maker is really gone rather than blipping.
//
// RemoveByMaker (called immediately above) deliberately spares cross-chain and Lightning
// offers: they are durable across a reconnect, and evicting on every transient WS drop
// emptied the cross book and vanished BTC from the DEX. But that left the opposite
// failure wide open — a maker that is genuinely gone leaves its offers resting, served
// at top of book, until their TTL. A fleet that restarts its makers fills the book with
// them, and takers walk the whole book collecting "maker is not connected", "offer not
// found or not open" and "another lift is in flight" from offers that will never settle.
//
// A grace period separates the two cases honestly: a maker that comes back re-registers
// (makerConns.get finds a live connection) and keeps everything; one that does not has
// its unliftable offers cleared.
func (s *Server) evictGhostOffersAfterGrace(pubkey string) {
	go func() {
		time.Sleep(makerReconnectGrace)
		if _, live := s.makerConns.get(pubkey); live {
			return // it came back; its offers are real again
		}
		if n := s.store.RemoveInteractiveByMaker(pubkey); n > 0 {
			short := pubkey
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Printf("[book] evicted %d unliftable offer(s) from departed maker %s\n", n, short)
		}
	}()
}

// takerReattachGrace is how long an abandoned lift session is held open for the taker
// to come back (P3.8 re-attach) before it is aborted and the offer's lift slot freed.
// 15s: a genuine P3.8 re-attach happens within a second or two of the socket dropping,
// while every second beyond that is one where the offer answers other takers with
// "another lift is in flight". 45s was long enough that a taker retrying down the book
// collided with offers its own previous attempt had wedged.
var takerReattachGrace = newDurationVar(15 * time.Second)

// durationVar is a duration that background goroutines read while tests (and a
// future flag) set it: a plain variable written in a test's cleanup while the
// abandoned-lift goroutine still reads it was a data race under -race.
type durationVar struct{ ns atomic.Int64 }

func newDurationVar(d time.Duration) *durationVar {
	v := &durationVar{}
	v.ns.Store(int64(d))
	return v
}
func (v *durationVar) Get() time.Duration  { return time.Duration(v.ns.Load()) }
func (v *durationVar) Set(d time.Duration) { v.ns.Store(int64(d)) }

// releaseAbandonedLifts aborts the lift sessions whose TAKER just disconnected and did
// not come back.
//
// An offer has ONE lift slot, held for as long as its session is live, and a cross
// session's co-sign deadline is measured in HOURS because it spans real parent-chain
// confirmations. Nothing released that slot when the taker simply went away — a failed
// take, a closed tab, a page reload — so one abandoned attempt made the offer answer
// every subsequent taker with "offer has a lift in progress; retry when it frees" for
// the rest of that deadline. In a book being probed by real users that poisons offers
// faster than the makers re-quote them, and the book looks full while being unusable.
//
// Router.Abort already exists for exactly this ("maker refused / taker bailed") and
// re-opens the order; it was simply never called on a disconnect.
//
// The grace period is what keeps this compatible with re-attach: a taker mid-swap with
// funds committed MUST be able to reconnect, and attach() overwrites the recorded
// connection when it does, so a returning taker is never aborted out from under.
func (s *Server) releaseAbandonedLifts(c *wsConn) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.roles))
	for sid, role := range c.roles {
		if role == session.RoleTaker {
			ids = append(ids, sid)
		}
	}
	c.mu.Unlock()
	for _, sid := range ids {
		go func(sid string) {
			time.Sleep(takerReattachGrace.Get())
			s.takerMu.Lock()
			cur, ok := s.takerConns[sid]
			if ok && cur == c {
				delete(s.takerConns, sid)
			}
			s.takerMu.Unlock()
			if !ok || cur != c {
				return // the taker re-attached on a new connection; leave it alone
			}
			if _, live := s.sessions.Get(sid); !live {
				return
			}
			s.sessions.Abort(sid)
			// AND TELL THE MAKER, or it waits out a deadline measured in hours.
			//
			// Abort tears down the relay's side of the session, but the maker learns nothing from that:
			// it is BLOCKED on a receive (waiting for the taker's funded-BTC announcement, up to 2h on
			// the cross rail) and only ever discovers a dead counterparty by trying to SEND. So the one
			// lift slot that offer has stayed held long after the taker was gone, and every other taker
			// was refused "busy, another lift is in flight" for the rest of it.
			//
			// This is the same error the maker already acts on when its own send fails, carrying the
			// session id so it can free exactly that slot. Push it rather than waiting for the maker to
			// speak first.
			s.notifyMakerTakerGone(sid)
		}(sid)
	}
}

func (s *Server) dispatch(c *wsConn, to *seqobv1.To, ip string) {
	switch m := to.GetMsg().(type) {
	case *seqobv1.To_MarketSubscribe:
		s.wsSubscribe(c, m.MarketSubscribe)
	case *seqobv1.To_MarketUnsubscribe:
		s.wsUnsubscribe(c, m.MarketUnsubscribe)
	case *seqobv1.To_OfferSubmit:
		s.wsOfferSubmit(c, m.OfferSubmit, ip)
	case *seqobv1.To_OfferEdit:
		s.wsOfferEdit(c, m.OfferEdit, ip)
	case *seqobv1.To_OfferCancel:
		s.wsOfferCancel(c, m.OfferCancel)
	case *seqobv1.To_StartLift:
		s.wsStartLift(c, m.StartLift)
	case *seqobv1.To_SessionReattach:
		s.wsSessionReattach(c, m.SessionReattach)
	case *seqobv1.To_SettleAck:
		s.wsSettleAck(c, m.SettleAck)
	case *seqobv1.To_SettledTrade:
		s.wsSettledTrade(c, m.SettledTrade)
	case *seqobv1.To_SwapMsg:
		s.wsSwapMsg(c, m.SwapMsg)
	case *seqobv1.To_ListMarkets:
		_ = c.send(&seqobv1.From{Msg: &seqobv1.From_MarketList{MarketList: &seqobv1.MarketList{Markets: s.markets()}}})
	default:
		c.sendErr(400, "unknown or empty To.msg")
	}
}

// subKey is the per-connection subscription key. It includes the book NAMESPACE
// (transparent vs confidential) so a client may hold BOTH a transparent and a
// confidential subscription to the same pair without one replacing the other.
func subKey(pair *seqobv1.AssetPair) string {
	k := pair.GetBaseAsset() + "/" + pair.GetQuoteAsset()
	if pair.GetConfidential() {
		k += "|conf"
	}
	return k
}

func (s *Server) wsSubscribe(c *wsConn, pair *seqobv1.AssetPair) {
	if pair == nil {
		c.sendErr(400, "market_subscribe missing pair")
		return
	}
	// The subscribe request carries the book namespace (AssetPair.confidential): false
	// (default) is the transparent book, true the separate blinded book. The snapshot and
	// every delta are filtered to it, matching REST (?confidential=) and the matcher, so a
	// transparent-book UI never renders a confidential offer and vice-versa.
	confidential := pair.GetConfidential()
	pk := subKey(pair)
	c.mu.Lock()
	_, replacing := c.subs[pk]
	full := !replacing && len(c.subs) >= maxSubsPerConn
	c.mu.Unlock()
	if full {
		c.sendErr(429, fmt.Sprintf("market_subscribe: at most %d subscriptions per connection", maxSubsPerConn))
		return
	}
	snap, id, ch := s.store.SubscribeEntries(pair, confidential)
	stop := make(chan struct{})

	c.mu.Lock()
	if prev, ok := c.subs[pk]; ok {
		prev()
	}
	c.subs[pk] = func() {
		close(stop)
		s.store.Unsubscribe(id)
	}
	c.mu.Unlock()

	// Snapshot first, then stream deltas. The snapshot passes the SAME liftability filter
	// the REST orderbook applies: serving the raw store here while REST hides unliftable
	// ghosts made the two sources disagree about the same market, and a wallet's ladder
	// showed whichever painted last (seen live: ~40 dead seeded offers "deep" over the WS,
	// 2 fillable ones over REST). One market, one book.
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_PublicBook{PublicBook: s.publicBook(pair, snap)}})
	go s.forwardDeltas(c, ch, stop)
}

func (s *Server) forwardDeltas(c *wsConn, ch <-chan offerstore.Event, stop <-chan struct{}) {
	var lastSeq uint64
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			var from *seqobv1.From
			switch ev.Type {
			case offerstore.EventCreated:
				from = &seqobv1.From{Msg: &seqobv1.From_PublicOrderCreated{PublicOrderCreated: ev.Offer}}
			case offerstore.EventStatus:
				from = &seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: ev.Status}}
			case offerstore.EventRemoved:
				from = &seqobv1.From{Msg: &seqobv1.From_PublicOrderRemoved{PublicOrderRemoved: ev.Ref}}
			default:
				continue
			}
			// P3.8 delta sequencing: the store stamps a monotonic per-subscription Seq and
			// increments it for every routed event (delivered OR dropped), so a hole here means
			// the relay dropped a delta on a buffer overflow and this subscriber's view is
			// stale. Tell it to re-snapshot (Resync) BEFORE the out-of-order delta, then carry
			// the seq on the delta so the client can also self-detect.
			if lastSeq != 0 && ev.Seq != lastSeq+1 {
				_ = c.send(&seqobv1.From{Msg: &seqobv1.From_Resync{Resync: &seqobv1.Resync{Pair: ev.Pair, NextSeq: ev.Seq}}})
			}
			lastSeq = ev.Seq
			from.Seq = ev.Seq
			if err := c.send(from); err != nil {
				return
			}
		}
	}
}

func (s *Server) wsUnsubscribe(c *wsConn, pair *seqobv1.AssetPair) {
	if pair == nil {
		return
	}
	pk := subKey(pair) // namespace-scoped, matching wsSubscribe
	c.mu.Lock()
	if stop, ok := c.subs[pk]; ok {
		stop()
		delete(c.subs, pk)
	}
	c.mu.Unlock()
}

func (s *Server) wsOfferSubmit(c *wsConn, o *seqobv1.Offer, ip string) {
	listed, err := s.validator.Admit(context.Background(), o, ip)
	if err != nil {
		if errors.Is(err, validator.ErrReplay) {
			// ITEM A: byte-identical replay of an already-resting offer. It cost the
			// maker no rate budget; do not re-submit (the store would reject the
			// duplicate key). Re-ack the live status and nothing else: the signed bytes
			// are public (the orderbook serves them), so a replay proves nothing about
			// who is on this socket. Registering it as the maker's endpoint would hand any
			// client the maker's lift routing and, on disconnect, the eviction of the
			// maker's whole book. A maker that reconnects re-signs (fresh created_at /
			// expiry) and registers through the edit path below.
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(400, "invalid offer: "+err.Error())
		return
	}
	if _, err := s.store.SubmitAt(o, listed); err != nil {
		// A DURABLE offer (cross-chain / lightning / covenant) survives its maker's WS drop, so a
		// reconnecting settlement agent re-posts a REFRESHED (re-signed, new expiry) copy against the
		// still-resting key. Treat that as an EDIT + re-register (so lifts route to the reconnected
		// agent) rather than a 409 — this is what keeps the durable book populated across WS churn.
		if errors.Is(err, offerstore.ErrExists) {
			// Edit demands a signature over DIFFERENT canonical bytes than the resting
			// copy (a byte-identical one is caught as ErrReplay above, and signatures are
			// canonical low-S DER so no second encoding of the same bytes verifies); only
			// the key holder can produce that. Registration additionally requires the copy
			// to be STRICTLY newer than the resting one: an earlier variant the maker
			// itself signed in the same second (and a third party captured from the
			// public book before it was replaced) still carries a real signature but
			// proves nothing about who holds this socket. A reconnecting maker re-signs
			// with the current time, which is newer.
			prev, _ := s.store.Get(offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()})
			if eerr := s.store.EditAt(o, listed); eerr != nil {
				c.sendErr(409, "submit(refresh): "+eerr.Error())
				return
			}
			if prev == nil || o.GetCreatedAtUnix() > prev.Offer.GetCreatedAtUnix() {
				s.registerMaker(c, o.GetMakerPubkey())
				s.ownOffer(c, o)
			}
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(409, "submit: "+err.Error())
		return
	}
	// Register this connection as the maker's reachable endpoint for live lifts.
	s.registerMaker(c, o.GetMakerPubkey())
	s.ownOffer(c, o)

	// Continuous matching under the order's time-in-force: cross the freshly-rested
	// order against the resting opposite side, routing From.matched to the
	// counterparties. For a covenant resting order the offline maker needs no
	// round-trip; the taker (this connection) is told the covenant terms and settles
	// the FILL spend itself. The disposition retires the incoming per IOC/FOK/POST_ONLY.
	res := s.matcher.CrossOrder(o)
	s.routeMatches(c, res.Matches)
	if s.applyDisposition(o, res) {
		_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: cancelledStatus(o)}})
		return
	}

	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
}

func (s *Server) wsOfferEdit(c *wsConn, o *seqobv1.Offer, ip string) {
	listed, err := s.validator.Admit(context.Background(), o, ip)
	if err != nil {
		if errors.Is(err, validator.ErrReplay) {
			// No-op edit identical to the resting offer (ITEM A): no budget charged,
			// nothing to change; re-ack the live status.
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(400, "invalid offer: "+err.Error())
		return
	}
	if err := s.store.EditAt(o, listed); err != nil {
		c.sendErr(409, "edit: "+err.Error())
		return
	}
	s.ownOffer(c, o)
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
}

// registerMaker binds this connection as the reachable endpoint for makerPubkey
// so a lift can be routed to it.
func (s *Server) registerMaker(c *wsConn, makerPubkey string) {
	c.mu.Lock()
	c.makerPubkey = makerPubkey
	c.mu.Unlock()
	s.makerConns.set(makerPubkey, c)
}

// ownOffer records that THIS connection posted (or re-posted) the offer, so eviction
// can be scoped to the connection rather than the pubkey. A re-post of the same key by
// a sibling or a reconnected agent transfers ownership, which is exactly what spares
// it at grace time.
func (s *Server) ownOffer(c *wsConn, o *seqobv1.Offer) {
	k := offerstore.Key{OfferID: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey()}
	s.ownerMu.Lock()
	s.offerOwners[k] = c
	s.ownerMu.Unlock()
}

// evictConnOffersAfterGrace removes the interactive offers a departed connection still
// OWNS once the reconnect grace passes. Ownership — not the pubkey — decides: a live
// sibling process under the same identity key keeps the pubkey registered forever, so
// the pubkey-scoped evictGhostOffersAfterGrace never fired and the dead sibling's
// offers ghosted at top of book until TTL (seen live: two prices resting under one
// key, takers routed to the process that would not honor the one they took).
func (s *Server) evictConnOffersAfterGrace(c *wsConn, keys []offerstore.Key) {
	if len(keys) == 0 {
		return
	}
	go func() {
		time.Sleep(makerReconnectGrace)
		stale := make([]offerstore.Key, 0, len(keys))
		s.ownerMu.Lock()
		for _, k := range keys {
			if s.offerOwners[k] == c { // not re-posted by anyone since; still the dead conn's
				delete(s.offerOwners, k)
				stale = append(stale, k)
			}
		}
		s.ownerMu.Unlock()
		if n := s.store.RemoveInteractiveByKeys(stale); n > 0 {
			fmt.Printf("[book] evicted %d unliftable offer(s) posted by a departed maker connection\n", n)
		}
	}()
}

// restingStatus returns the live OrderStatus for o's key, falling back to a fresh
// OPEN status if the offer raced out of the book between validate and lookup.
func (s *Server) restingStatus(o *seqobv1.Offer) *seqobv1.OrderStatus {
	if st, ok := s.store.OrderStatusOf(offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}); ok {
		return st
	}
	return &seqobv1.OrderStatus{
		OfferId: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount(),
	}
}

func (s *Server) wsOfferCancel(c *wsConn, cancel *seqobv1.OfferCancel) {
	if err := s.validator.ValidateCancel(cancel); err != nil {
		c.sendErr(400, "invalid cancel: "+err.Error())
		return
	}
	if err := s.store.Cancel(cancel); err != nil {
		c.sendErr(409, "cancel: "+err.Error())
		return
	}
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: &seqobv1.OrderStatus{
		OfferId: cancel.GetOfferId(), MakerPubkey: cancel.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_CANCELLED,
	}}})
}

// liftGuardExempts reports whether an offer may be lifted with NO maker connection
// present. Only a covenant offer qualifies: the taker settles the fill spend itself, so
// there is nobody to wait for. Every other settlement needs the maker live to send
// terms. A nil offer is exempt so the guard never pre-empts openLift's own reporting of
// an unknown/closed offer.
func liftGuardExempts(o *seqobv1.Offer) bool {
	return o == nil || offer.IsCovenant(o)
}

func (s *Server) wsStartLift(c *wsConn, sl *seqobv1.StartLift) {
	// openLift -> Router.StartLift fires the notifyMaker hook, which binds the
	// maker connection and delivers From.lift_requested (with the taker's session
	// pubkey) BEFORE we reply to the taker. Bind the taker here so it can receive
	// the maker's couriered reply.
	// REFUSE A LIFT AGAINST A MAKER THAT IS NOT HERE.
	//
	// A cross-chain or Lightning offer survives its maker's WS drop on purpose: the
	// settlement agent reconnects after a blip, and evicting on every drop emptied the
	// cross book (see TestCrossSurvivesMakerDisconnect). But surviving a reconnect is not
	// the same as being liftable while the maker is GONE. Those rails need the maker live
	// to send terms, so a lift against a departed maker used to be ACCEPTED and then
	// simply go quiet — the taker waited out its terms timeout, and the trade sat there
	// while blocks went by, with nothing to retry against because no error was ever sent.
	//
	// A covenant offer is genuinely offline-liftable (the taker settles the fill spend
	// itself), so it is exempt. For everything else, say so immediately: an instant,
	// explicit refusal is retryable down the book, where silence is not.
	if o, ok := s.store.Get(offerstore.Key{MakerPubkey: sl.GetMakerPubkey(), OfferID: sl.GetOfferId()}); ok && !liftGuardExempts(o.Offer) {
		if _, live := s.makerConns.get(sl.GetMakerPubkey()); !live {
			c.sendErr(409, "maker is not connected; this offer cannot be lifted right now")
			return
		}
	}
	sess, err := s.openLift(sl)
	if err != nil {
		c.sendErr(409, err.Error())
		return
	}
	s.attach(c, sess, session.RoleTaker)
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_LiftAccepted{LiftAccepted: &seqobv1.LiftAccepted{
		SessionId: sess.ID, MakerSessionPubkey: makerPubkeyBytes(sl.GetMakerPubkey()),
	}}})
}

// wsSessionReattach re-binds a RECONNECTED websocket to a still-live lift session
// after a WS drop (P3.8). The role was bound to the OLD socket, so without this the
// fresh WS 403s on the first swap_msg. The participant re-proves control of the
// session key the relay already holds for the claimed role (the maker's offer key,
// or the taker's ephemeral session pubkey), so no new trust is introduced.
func (s *Server) wsSessionReattach(c *wsConn, ra *seqobv1.SessionReattach) {
	if ra == nil || ra.GetSessionId() == "" {
		c.sendErr(400, "session_reattach missing session_id")
		return
	}
	sid, role := ra.GetSessionId(), ra.GetRole()
	sess, ok := s.sessions.Get(sid)
	if !ok {
		c.sendErr(409, "unknown or closed session")
		return
	}
	var sessRole session.Role
	var keyBytes []byte
	switch role {
	case "maker":
		sessRole = session.RoleMaker
		keyBytes = makerPubkeyBytes(sess.MakerPubkey) // the maker's offer key doubles as its session key
	case "taker":
		sessRole = session.RoleTaker
		keyBytes = sess.TakerSessionPubkey
	default:
		c.sendErr(400, "session_reattach role must be \"taker\" or \"maker\"")
		return
	}
	if len(keyBytes) == 0 {
		c.sendErr(409, "no bound session key for role "+role)
		return
	}
	if err := offer.VerifyReattach(sid, role, keyBytes, ra.GetSig()); err != nil {
		c.sendErr(403, "reattach auth: "+err.Error())
		return
	}
	// Re-bind: attach the role so this connection again receives couriered frames, and
	// (for a maker) re-register it as the maker's reachable endpoint for further lifts.
	s.attach(c, sess, sessRole)
	if sessRole == session.RoleMaker {
		s.registerMaker(c, sess.MakerPubkey)
	}
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_SessionReattached{SessionReattached: &seqobv1.SessionReattached{
		SessionId: sid, Role: role,
	}}})
}

// notifyMaker is the session.Router hook: when a taker lifts an offer, deliver a
// From.lift_requested to the maker's live connection (if any) carrying the
// taker's session pubkey, and bind the maker so it receives couriered frames.
// The relay never decrypts; it only routes.
// notifyMakerTakerGone tells the maker that a session's taker has gone, so it can release that
// offer's single lift slot instead of blocking on a receive until its deadline expires.
//
// It reuses the SAME error the maker already handles when one of its own sends is refused, id and
// all, so there is one shape to understand on the maker side rather than two.
//
// Best-effort by design: the session is already aborted, so a maker that is itself offline simply
// finds the session gone when it returns. Called after Abort, when the session is no longer in the
// router, so the maker pubkey is read from the connection registry via the recorded lift.
func (s *Server) notifyMakerTakerGone(sid string) {
	s.liftMu.Lock()
	var maker string
	for k, live := range s.liftActive {
		if live == sid {
			maker = k.MakerPubkey
			break
		}
	}
	s.liftMu.Unlock()
	if maker == "" {
		return
	}
	if mc, ok := s.makerConns.get(maker); ok {
		mc.sendErr(409, "courier: the taker is not attached to session "+sid)
	}
}

func (s *Server) notifyMaker(sess *session.Session) {
	mc, ok := s.makerConns.get(sess.MakerPubkey)
	if !ok {
		return
	}
	s.attach(mc, sess, session.RoleMaker)
	_ = mc.send(&seqobv1.From{Msg: &seqobv1.From_LiftRequested{LiftRequested: &seqobv1.LiftRequested{
		SessionId:          sess.ID,
		OfferId:            sess.OfferID,
		MakerPubkey:        sess.MakerPubkey,
		TakeAmount:         sess.TakeAmount,
		TakerSessionPubkey: sess.TakerSessionPubkey,
	}}})
}

// attach records the connection's role in a session and starts a pump that
// forwards inbound courier frames for that role to the socket.
func (s *Server) attach(c *wsConn, sess *session.Session, role session.Role) {
	c.mu.Lock()
	c.roles[sess.ID] = role
	c.mu.Unlock()
	if role == session.RoleTaker {
		// Remember who holds the taker side, so closeConn can tell an ABANDONED lift
		// from a taker that merely reconnected (P3.8 re-attach overwrites this).
		s.takerMu.Lock()
		s.takerConns[sess.ID] = c
		s.takerMu.Unlock()
	}
	s.pumpAdd(sess.ID, role, 1)
	go func() {
		defer s.pumpAdd(sess.ID, role, -1)
		inbox := sess.Inbox(role)
		for {
			select {
			case <-sess.Done():
				return
			case <-c.closed:
				// This connection dropped: stop pumping so a reconnected+re-attached
				// connection (P3.8) receives the session's frames instead of this dead pump
				// silently consuming them.
				return
			case msg, ok := <-inbox:
				if !ok {
					return
				}
				if err := c.send(&seqobv1.From{Msg: &seqobv1.From_SwapMsg{SwapMsg: msg}}); err != nil {
					return
				}
			}
		}
	}()
}

func (s *Server) wsSwapMsg(c *wsConn, msg *seqobv1.SwapMsg) {
	if msg == nil || msg.GetSessionId() == "" {
		c.sendErr(400, "swap_msg missing session_id")
		return
	}
	c.mu.Lock()
	role, ok := c.roles[msg.GetSessionId()]
	c.mu.Unlock()
	if !ok {
		c.sendErr(403, "not a participant in this session")
		return
	}
	// A frame for a role with no live pump is queued into a buffered inbox nobody
	// will drain: the counterparty waits out its deadline and NOTHING anywhere says
	// why. Say it, on both sides — the sender learns its frame is stranded instead
	// of blaming a slow counterparty, and the log names the session.
	dst := session.RoleMaker
	if role == session.RoleMaker {
		dst = session.RoleTaker
	}
	if s.pumpCount(msg.GetSessionId(), dst) == 0 {
		s.log.Printf("courier: session %s: %s sent a frame but the %s side has no attached"+
			" connection — it is queued and undeliverable until that side re-attaches",
			msg.GetSessionId(), roleName(role), roleName(dst))
		// NAME THE SESSION IN THE ERROR. A maker serves ONE lift at a time and holds that slot for the
		// whole driver run, so it has to know WHICH session died in order to free it. Unattributable,
		// the maker just waits out its 2-minute TermsRequest deadline — while the wallet gives up after
		// 30s — so every abandoned take parks a maker on "busy". A handful of them wedged the fleet.
		c.sendErr(409, "courier: the "+roleName(dst)+" is not attached to session "+msg.GetSessionId())
		return
	}
	// OPAQUE courier: route ciphertext only, never decrypt or parse it.
	if err := s.sessions.Send(msg.GetSessionId(), role, msg.GetCiphertext()); err != nil {
		s.log.Printf("courier: session %s: %s send failed: %v", msg.GetSessionId(), roleName(role), err)
		c.sendErr(409, "courier: "+err.Error())
	}
}

func roleName(r session.Role) string {
	if r == session.RoleMaker {
		return "maker"
	}
	return "taker"
}

// pumpAdd adjusts the live-pump count for a session role (+1 on attach, -1 when
// the pump exits).
func (s *Server) pumpAdd(sessionID string, role session.Role, delta int) {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	m := s.pumped[sessionID]
	if m == nil {
		if delta <= 0 {
			return
		}
		m = make(map[session.Role]int)
		s.pumped[sessionID] = m
	}
	m[role] += delta
	if m[role] <= 0 {
		delete(m, role)
	}
	if len(m) == 0 {
		delete(s.pumped, sessionID)
	}
}

// pumpCount reports how many live pumps are draining a session role's inbox.
func (s *Server) pumpCount(sessionID string, role session.Role) int {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	return s.pumped[sessionID][role]
}

// wsSettleAck is the interactive-lift settlement commit point. A same-chain lift settles
// via an opaque SwapMsg courier the relay never parses, so it otherwise never learns the
// lift settled — leaving the resting order a ghost at full size and the trades feed empty
// by construction. The MAKER, having co-signed the exact settling PSET, sends this ack
// after broadcast; on it the relay records the trade at the resting order's price,
// decrements/finalizes the resting order, arms reorg re-open with the settling txid, and
// closes the session. MAKER-authenticated: only the session's maker may send it, so a
// taker cannot grief a live resting order by forging a decrement. The decrement size is
// the relay's own authoritative TakeAmount (from StartLift), not the maker-reported
// fill_base, clamped to what is actually resting.
func (s *Server) wsSettleAck(c *wsConn, sa *seqobv1.SettleAck) {
	if sa == nil || sa.GetSessionId() == "" {
		c.sendErr(400, "settle_ack missing session_id")
		return
	}
	sid := sa.GetSessionId()
	c.mu.Lock()
	role, ok := c.roles[sid]
	c.mu.Unlock()
	if !ok || role != session.RoleMaker {
		c.sendErr(403, "only the session maker may ack settlement")
		return
	}
	sess, ok := s.sessions.Get(sid)
	if !ok {
		c.sendErr(409, "unknown or closed session")
		return
	}
	k := offerstore.Key{MakerPubkey: sess.MakerPubkey, OfferID: sess.OfferID}
	// Record + decrement only if the resting order is still present. If a prior fill already
	// removed it, there is nothing to record/decrement — but still close the session so it does
	// not linger and get deadline-aborted (which would try to re-open an already-settled order).
	if e, ok := s.store.Get(k); ok {
		fill := sess.TakeAmount
		if fill > e.ActiveAmount {
			fill = e.ActiveAmount // never decrement below the resting size (it shrank via another fill)
		}
		if fill > 0 {
			// Record BEFORE ApplyPartialFill: the latter may remove the entry at full fill, after
			// which e.Offer (needed for the trade's pair + execution price) is no longer fetchable.
			// Record WITH the settle txid so a reorg can retract this exact print (P3.9).
			s.store.RecordTradeTx(e.Offer, fill, sa.GetSettleTxid())
			// anchor_confs the maker declares for the settling tx (0 keeps the 0-conf-tolerant
			// default). A fill with anchor_confs >= min_anchor_depth reaches FILLED; previously
			// this was hard-coded 0, so a min_anchor_depth>0 offer could never finalize.
			if err := s.store.ApplyPartialFill(k, fill, sa.GetSettleTxid(), sa.GetAnchorConfs()); err != nil {
				c.sendErr(409, "apply fill: "+err.Error())
				return
			}
			// Arm reorg re-open with the settled offer + fill: if this tx's Bitcoin anchor is
			// later orphaned, onReopen re-inserts the exact order (restoring `fill` atoms) and
			// retracts the phantom trade (Principle 1 / P3.9). Cache the offer here because a full
			// fill REMOVED it from the store.
			_ = s.sessions.ArmReorgReopen(sid, sa.GetSettleTxid(), e.Offer, fill)
			s.sessions.Close(sid)
			return
		}
	}
	_ = s.sessions.SetSettleTxid(sid, sa.GetSettleTxid()) // arms reorg re-open (Principle 1)
	s.sessions.Close(sid)
}

// wsSettledTrade is the durable-settlement commit point for CROSS-CHAIN and LIGHTNING
// lifts. Unlike a same-chain lift (fast, its co-sign session still live at settlement),
// a cross/LN swap can finish long after the courier session was swept (its on-chain /
// Lightning leg outlives the co-sign deadline) or after the maker reconnected (dropping
// the in-memory session role) — so a session-scoped SettleAck is unreachable. Instead the
// maker sends this MAKER-SIGNED, offer-keyed record: the relay verifies the signature
// against the resting offer's maker_pubkey, records the trade at the resting order's price,
// and decrements/finalizes the order (advancing anchor_confs). Without it a BTC/LN-only pair
// reports last_price 0 with an empty chart — every cross/LN fill went unrecorded. It leaks
// nothing the chain does not (settle_txid is public), and the maker has no incentive to
// falsely decrement its own resting order. Reorg retraction is a separate concern.
func (s *Server) wsSettledTrade(c *wsConn, st *seqobv1.SettledTrade) {
	if st == nil || st.GetOfferId() == "" || st.GetMakerPubkey() == "" {
		c.sendErr(400, "settled_trade missing offer_id/maker_pubkey")
		return
	}
	// Maker-signed and self-standing: the signature over {offer_id, maker_pubkey, fill_base,
	// settle_txid, anchor_confs, nonce} authenticates the report without a live session, and
	// keying the store op to (maker_pubkey, offer_id) means only the offer's own maker can
	// record/decrement its fill (a forged report for another maker's offer needs that maker's key).
	if err := offer.VerifySettledTrade(st); err != nil {
		c.sendErr(400, "invalid settled_trade: "+err.Error())
		return
	}
	k := offerstore.Key{MakerPubkey: st.GetMakerPubkey(), OfferID: st.GetOfferId()}
	if err := s.store.RecordSettledFill(k, st.GetFillBase(), st.GetSettleTxid(), st.GetAnchorConfs(), st.GetNonce()); err != nil {
		c.sendErr(409, "record settled trade: "+err.Error())
		return
	}
}
