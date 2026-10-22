// Package api exposes the SeqOB relay over HTTP REST + WebSocket using the
// generated To/From envelope (relay.proto). Bodies are protojson so the Go CLI
// and a browser share one encoding.
//
// REST:
//
//	POST /v1/offers                              submit a signed Offer
//	POST /v1/offers/cancel                       submit a signed OfferCancel
//	GET  /v1/offers?maker_pubkey=...             a maker's own orders
//	GET  /v1/markets                             market summaries
//	GET  /v1/market/{base}/{quote}/orderbook     per-pair snapshot (PublicBook: the
//	                                             signed offers, and statuses[i], each
//	                                             one's live size, active_amount)
//	POST /v1/lift                                open a lift session (StartLift)
//	GET  /v1/ws                                  WebSocket (To/From)
//
// The relay is NON-CUSTODIAL: it stores signed text, serves the book, and
// couriers OPAQUE encrypted SwapMsg frames between peers. It never holds keys or
// funds and never decrypts the courier payload.
package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

// Server wires the store, validator, matcher, and session router behind HTTP.
type Server struct {
	store     *offerstore.Store
	validator *validator.Validator
	matcher   *matcher.Matcher
	sessions  *session.Router
	log       *log.Logger
	// crossDeadline, when set, replaces the router's default co-sign deadline
	// for sessions opened on CROSS-CHAIN offers (they span a real parent-chain
	// confirmation between courier rounds).
	crossDeadline time.Duration
	upgrader      websocket.Upgrader

	// makerConns tracks WS connections by registered maker_pubkey so a lift can be
	// routed to an online maker. Best-effort (Phase-1).
	makerConns *connRegistry

	// offerOwners tracks which CONNECTION posted each resting offer. Pubkey-scoped
	// eviction cannot tell two maker processes sharing one identity key apart (a
	// requote overlap, a stray sibling), so a dead process's offers ghosted while any
	// live sibling pinned the pubkey. Ownership transfers on a re-post of the same key.
	ownerMu     sync.Mutex
	offerOwners map[offerstore.Key]*wsConn

	// liftMu guards liftActive, the per-offer active-lift-session map (P3.10): one live
	// lift session per NON-covenant offer, so two takers cannot open concurrent sessions
	// on the same order (the 2nd would waste a session and race the maker's single coins).
	liftMu     sync.Mutex
	liftActive map[offerstore.Key]string // offer key -> live session id

	// takerConns tracks which connection currently holds the TAKER side of a lift
	// session, so a taker that goes away can have its session released instead of
	// pinning the offer's single lift slot until the co-sign deadline (hours, on the
	// cross rail). See releaseAbandonedLift.
	takerMu    sync.Mutex
	takerConns map[string]*wsConn // session id -> the taker's connection

	// pumped counts the LIVE inbox pumps per session role. A courier frame is
	// delivered by the pump attach() starts, and that pump exits when its
	// connection closes — but Router.Send still SUCCEEDS afterwards, because each
	// inbox is a buffered channel. A frame sent to a role with no pump is therefore
	// queued into a channel nobody will ever drain: the counterparty simply waits
	// until its deadline with no error anywhere. That silent stranding is worth
	// naming, so wsSwapMsg reports it instead of letting the frame vanish.
	pumpMu sync.Mutex
	pumped map[string]map[session.Role]int // session id -> role -> live pumps
}

// New builds a Server.
func New(store *offerstore.Store, v *validator.Validator, sessions *session.Router, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	srv := &Server{
		store:     store,
		validator: v,
		matcher:   matcher.New(store),
		sessions:  sessions,
		log:       logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		makerConns:  newConnRegistry(),
		offerOwners: make(map[offerstore.Key]*wsConn),
		liftActive:  make(map[offerstore.Key]string),
		takerConns:  make(map[string]*wsConn),
		pumped:      make(map[string]map[session.Role]int),
	}
	// The relay notifies an online maker (via From.lift_requested) whenever a taker
	// lifts one of its offers, so the maker can derive the E2E key and co-sign.
	sessions.SetNotifyMaker(srv.notifyMaker)
	// Wire the book into the validator so a byte-identical replay of a resting offer
	// is recognized and declined the victim maker's per-pubkey rate budget (ITEM A).
	v.SetBook(store)
	return srv
}

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// Handler returns the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/offers", s.handleOffers)
	mux.HandleFunc("/v1/offers/cancel", s.handleCancel)
	mux.HandleFunc("/v1/markets", s.handleMarkets)
	mux.HandleFunc("/v1/market/", s.handleOrderbook)
	mux.HandleFunc("/v1/lift", s.handleLift)
	mux.HandleFunc("/v1/ws", s.handleWS)
	return mux
}

func (s *Server) handleOffers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleOfferSubmit(w, r)
	case http.MethodGet:
		s.handleOwnOffers(w, r)
	default:
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleOfferSubmit(w http.ResponseWriter, r *http.Request) {
	var o seqobv1.Offer
	if err := readProto(r, &o); err != nil {
		httpErr(w, http.StatusBadRequest, "decode offer: "+err.Error())
		return
	}
	listed, err := s.validator.Admit(r.Context(), &o, clientIP(r))
	if err != nil {
		if errors.Is(err, validator.ErrReplay) {
			// ITEM A: byte-identical replay of an already-resting offer; a no-op that
			// consumed no rate budget. Echo the live status rather than re-submitting
			// (the store would reject the duplicate key anyway).
			st, ok := s.store.OrderStatusOf(offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()})
			if !ok {
				st = &seqobv1.OrderStatus{OfferId: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey(), Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount()}
			}
			writeProto(w, st)
			return
		}
		httpErr(w, http.StatusBadRequest, "invalid offer: "+err.Error())
		return
	}
	k, err := s.store.SubmitAt(&o, listed)
	if err != nil {
		httpErr(w, http.StatusConflict, "submit: "+err.Error())
		return
	}
	// Cross against the resting opposite side under the order's time-in-force to keep
	// the book consistent. The REST path has no persistent connection to push
	// From.matched to (WS clients receive it); the returned status reflects the
	// post-cross disposition (CANCELLED if the order was retired per IOC/FOK/POST_ONLY).
	res := s.matcher.CrossOrder(&o)
	s.routeMatches(nil, res.Matches)
	if s.applyDisposition(&o, res) {
		writeProto(w, cancelledStatus(&o))
		return
	}
	st, ok := s.store.OrderStatusOf(k)
	if !ok {
		st = &seqobv1.OrderStatus{
			OfferId: k.OfferID, MakerPubkey: k.MakerPubkey,
			Status: seqobv1.OfferStatus_OFFER_STATUS_FILLED, ActiveAmount: 0,
		}
	}
	writeProto(w, st)
}

func (s *Server) handleOwnOffers(w http.ResponseWriter, r *http.Request) {
	maker := r.URL.Query().Get("maker_pubkey")
	if maker == "" {
		httpErr(w, http.StatusBadRequest, "maker_pubkey required")
		return
	}
	entries := s.store.SnapshotMaker(maker)
	book := &seqobv1.PublicBook{}
	for _, e := range entries {
		book.Offers = append(book.Offers, e.Offer)
		book.Statuses = append(book.Statuses, e.OrderStatus())
	}
	writeProto(w, book)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var c seqobv1.OfferCancel
	if err := readProto(r, &c); err != nil {
		httpErr(w, http.StatusBadRequest, "decode cancel: "+err.Error())
		return
	}
	if err := s.validator.ValidateCancel(&c); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid cancel: "+err.Error())
		return
	}
	if err := s.store.Cancel(&c); err != nil {
		httpErr(w, http.StatusConflict, "cancel: "+err.Error())
		return
	}
	writeProto(w, &seqobv1.OrderStatus{
		OfferId:     c.GetOfferId(),
		MakerPubkey: c.GetMakerPubkey(),
		Status:      seqobv1.OfferStatus_OFFER_STATUS_CANCELLED,
	})
}

func (s *Server) handleMarkets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeProto(w, &seqobv1.MarketList{Markets: s.markets()})
}

// markets is the market summary filtered exactly like the orderbook: an offer the
// book would hide as an unliftable ghost must not count toward n_orders or set the
// best price, or a market reads as deep and quoted while its book comes back empty.
func (s *Server) markets() []*seqobv1.Market {
	now := time.Now().Unix()
	return s.store.MarketsFiltered(func(o *seqobv1.Offer) bool { return s.offerLiftable(o, now) })
}

// handleOrderbook serves /v1/market/{base}/{quote}/{orderbook|trades|candles|fills}.
func (s *Server) handleOrderbook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/market/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		httpErr(w, http.StatusNotFound, "expected /v1/market/{base}/{quote}/{orderbook|trades|candles|fills}")
		return
	}
	pair := &seqobv1.AssetPair{BaseAsset: parts[0], QuoteAsset: parts[1]}
	switch parts[2] {
	case "orderbook":
		// ?confidential=1 serves the separate BLINDED book for this pair; the default
		// (absent/0) serves the existing transparent book unchanged, so the live
		// unblinded DEX is byte-for-byte untouched.
		confidential := isTruthy(r.URL.Query().Get("confidential"))
		writeProto(w, s.publicBook(pair, s.store.SnapshotPairEntries(pair, confidential)))
	case "trades":
		s.handleTrades(w, r, pair)
	case "candles":
		s.handleCandles(w, r, pair)
	case "fills":
		s.handleFills(w, r, pair)
	default:
		httpErr(w, http.StatusNotFound, "expected /v1/market/{base}/{quote}/{orderbook|trades|candles|fills}")
	}
}

// handleTrades serves the recent-cross feed: GET /v1/market/{base}/{quote}/trades?limit=N.
// JSON (not proto — this is metadata the wallet renders, and there is no Trade proto).
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request, pair *seqobv1.AssetPair) {
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	type tradeJSON struct {
		Ts    int64   `json:"ts"`    // unix seconds
		Price float64 `json:"price"` // quote per base
		Size  uint64  `json:"size"`  // base atoms
	}
	trades := s.store.TradesFor(pair, limit)
	out := make([]tradeJSON, 0, len(trades))
	for _, t := range trades {
		out = append(out, tradeJSON{Ts: t.TS.Unix(), Price: t.Price, Size: t.Size})
	}
	writeJSON(w, map[string]interface{}{"base": pair.BaseAsset, "quote": pair.QuoteAsset, "trades": out})
}

// handleFills serves the records of the pair's covenant fills, oldest first:
// GET /v1/market/{base}/{quote}/fills?limit=N. Each is read from the filling
// transaction's outputs (the maker's credit at 2k, the order resting again or
// the gap at 2k+1), names its taker (an on-chain fill, or an Arca operator's
// round, whose takers hold leaves of its batch), and says where it stands:
// confirmed, final (certified and its anchor buried) or undone (its block left
// the chain), as the chain watcher last saw it.
func (s *Server) handleFills(w http.ResponseWriter, r *http.Request, pair *seqobv1.AssetPair) {
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	writeJSON(w, map[string]interface{}{"base": pair.BaseAsset, "quote": pair.QuoteAsset, "fills": s.store.CovenantFills(pair, limit)})
}

// handleCandles serves OHLCV bucketed from the trades: GET
// /v1/market/{base}/{quote}/candles?interval=<seconds>&limit=N. Buckets align to the epoch.
func (s *Server) handleCandles(w http.ResponseWriter, r *http.Request, pair *seqobv1.AssetPair) {
	interval := int64(3600) // 1h default
	if v, err := strconv.ParseInt(r.URL.Query().Get("interval"), 10, 64); err == nil && v >= 60 && v <= 86400*7 {
		interval = v
	}
	limit := 168 // ~1 week of hourly
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	type candle struct {
		T int64   `json:"t"` // bucket start, unix seconds
		O float64 `json:"o"`
		H float64 `json:"h"`
		L float64 `json:"l"`
		C float64 `json:"c"`
		V uint64  `json:"v"` // base volume
	}
	trades := s.store.TradesFor(pair, 0) // all held; bucket then trim
	buckets := make(map[int64]*candle)
	order := make([]int64, 0)
	for _, t := range trades {
		b := (t.TS.Unix() / interval) * interval
		c := buckets[b]
		if c == nil {
			c = &candle{T: b, O: t.Price, H: t.Price, L: t.Price, C: t.Price}
			buckets[b] = c
			order = append(order, b)
		}
		if t.Price > c.H {
			c.H = t.Price
		}
		if t.Price < c.L {
			c.L = t.Price
		}
		c.C = t.Price
		c.V += t.Size
	}
	// order is already ascending (trades are newest-last, buckets non-decreasing).
	out := make([]*candle, 0, len(order))
	for _, b := range order {
		out = append(out, buckets[b])
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	writeJSON(w, map[string]interface{}{"base": pair.BaseAsset, "quote": pair.QuoteAsset, "interval": interval, "candles": out})
}

func (s *Server) handleLift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var sl seqobv1.StartLift
	if err := readProto(r, &sl); err != nil {
		httpErr(w, http.StatusBadRequest, "decode start_lift: "+err.Error())
		return
	}
	sess, err := s.openLift(&sl)
	if err != nil {
		httpErr(w, http.StatusConflict, err.Error())
		return
	}
	s.expireUnattachedLift(sess.ID, offerstore.Key{MakerPubkey: sl.GetMakerPubkey(), OfferID: sl.GetOfferId()})
	writeProto(w, &seqobv1.LiftAccepted{
		SessionId:          sess.ID,
		MakerSessionPubkey: makerPubkeyBytes(sl.GetMakerPubkey()),
	})
}

// pruneDeadLifts drops lift-slot entries whose session is no longer live, so the
// map stays proportional to offers with a lift actually in progress rather than
// to every offer ever lifted.
func (s *Server) pruneDeadLifts() {
	s.liftMu.Lock()
	defer s.liftMu.Unlock()
	for k, sid := range s.liftActive {
		if sid == liftReserving {
			continue
		}
		if _, live := s.sessions.Get(sid); !live {
			delete(s.liftActive, k)
		}
	}
}

// restLiftAttachGrace is how long a lift opened over REST may sit with no taker socket
// attached before the relay aborts it. A REST lift holds the offer's single lift slot
// (for a cross offer, up to the multi-hour cross deadline) yet binds to no connection,
// so the abandoned-lift release that protects WS lifts never fires for it: one
// unauthenticated POST per offer would keep the whole interactive book "lift in
// progress". A real taker opens its socket and re-attaches within seconds.
var restLiftAttachGrace = newDurationVar(30 * time.Second)

// expireUnattachedLift aborts a REST-opened lift on a non-covenant offer if no taker
// has attached to the session by the grace deadline. Covenant lifts hold no slot.
func (s *Server) expireUnattachedLift(sid string, k offerstore.Key) {
	if e, ok := s.store.Get(k); !ok || offer.FilledWithoutMaker(e.Offer) {
		return
	}
	time.AfterFunc(restLiftAttachGrace.Get(), func() {
		s.takerMu.Lock()
		_, attached := s.takerConns[sid]
		s.takerMu.Unlock()
		if attached {
			return
		}
		if _, live := s.sessions.Get(sid); !live {
			return
		}
		s.sessions.Abort(sid)
		s.notifyMakerTakerGone(sid)
	})
}

// makerPubkeyBytes decodes a hex maker pubkey to the 33-byte compressed key the
// taker seals its E2E payload to (the maker's offer key doubles as its session
// key). Returns nil on a malformed key.
func makerPubkeyBytes(hexPub string) []byte {
	b, err := hex.DecodeString(hexPub)
	if err != nil {
		return nil
	}
	return b
}

// openLift validates the referenced offer is live and opens a session.
func (s *Server) openLift(sl *seqobv1.StartLift) (*session.Session, error) {
	// POST_ONLY is a rest-only maker option: it is meaningless on a lift (a lift is by
	// definition a take), so reject it rather than silently ignoring it. IOC/FOK on a
	// lift are additive/reserved (a single lift already takes a fixed amount).
	if sl.GetTimeInForce() == seqobv1.TimeInForce_TIME_IN_FORCE_POST_ONLY {
		return nil, errString("post_only is invalid on a lift (a lift takes liquidity)")
	}
	k := offerstore.Key{MakerPubkey: sl.GetMakerPubkey(), OfferID: sl.GetOfferId()}
	e, ok := s.store.Get(k)
	if !ok {
		return nil, errString("offer not found or not open")
	}
	if offer.IsResting(e.Offer) {
		return nil, errString("this order rests in an operator's tree: fill it through its operator (" +
			e.Offer.GetRestingLeaf().GetOperatorUrl() + "/v1/order_fill), not by a lift")
	}
	if sl.GetTakeAmount() > e.ActiveAmount {
		return nil, errString("take_amount exceeds active_amount")
	}
	// Serialize lifts per offer (P3.10): a NON-covenant offer settles against the
	// maker's single set of coins, so two concurrent takers racing one order means the
	// second wastes a session (and could double-request the maker's coins). Allow only
	// ONE live session per such offer; a second lift is refused until the first ends
	// (settle/abort/deadline), at which point the taker retries. A dropped participant
	// re-attaches to the live session instead (P3.8). Covenant offers are EXEMPT: they
	// are funded on-chain and first-spend-wins enforces a single fill, so concurrent
	// permissionless takers are safe.
	isCovenant := offer.IsCovenant(e.Offer)
	if !isCovenant {
		s.pruneDeadLifts()
		// Atomically RESERVE the offer's single lift slot with an in-progress sentinel,
		// so a concurrent openLift sees it taken even before StartLift returns. The lock
		// is NOT held across StartLift (which does a WS notify that could block on a slow
		// maker). A slot whose recorded session is no longer live (settled/aborted) is
		// treated as free.
		s.liftMu.Lock()
		if prev, ok := s.liftActive[k]; ok && prev != "" {
			_, live := s.sessions.Get(prev)
			if prev == liftReserving || live {
				s.liftMu.Unlock()
				return nil, errString("offer has a lift in progress; retry when it frees")
			}
		}
		s.liftActive[k] = liftReserving
		s.liftMu.Unlock()
	}
	sess, err := s.sessions.StartLift(session.OpenReq{
		OfferID:            sl.GetOfferId(),
		MakerPubkey:        sl.GetMakerPubkey(),
		TakeAmount:         sl.GetTakeAmount(),
		TakerSessionPubkey: sl.GetTakerSessionPubkey(),
	})
	if !isCovenant {
		s.liftMu.Lock()
		if err != nil {
			delete(s.liftActive, k) // release the reservation on failure
		} else {
			s.liftActive[k] = sess.ID
		}
		s.liftMu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	// A cross-chain lift spans a real parent-chain confirmation between courier
	// rounds; give it the longer settlement-type deadline. The relay stays blind
	// to the couriered bytes — this reads only the offer's settlement TYPE.
	if s.crossDeadline > 0 && e.Offer.GetCrossChain() != nil {
		s.sessions.ExtendDeadline(sess.ID, s.crossDeadline)
	}
	return sess, nil
}

// SetCrossSessionDeadline configures the courier deadline for sessions opened on
// cross-chain offers (0 keeps the router's same-chain default for them too).
func (s *Server) SetCrossSessionDeadline(d time.Duration) { s.crossDeadline = d }

// --- helpers ---

// liftReserving is the in-progress sentinel held in liftActive between reserving an
// offer's lift slot and StartLift returning its real session id (P3.10).
const liftReserving = "\x00reserving"

type errString string

func (e errString) Error() string { return string(e) }

// maxBodyBytes bounds a REST request body. Offers, cancels and lifts are each well
// under a kilobyte; the JSON decoder would otherwise buffer whatever a client sends.
const maxBodyBytes = 256 << 10

func readProto(r *http.Request, m proto.Message) error {
	defer r.Body.Close()
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	return jsonUnmarshal.Unmarshal(raw, m)
}

func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := jsonMarshal.Marshal(m)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// writeJSON emits a plain JSON body (for the trades/candles metadata endpoints, which have no proto).
func writeJSON(w http.ResponseWriter, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]interface{}{"code": code, "message": msg})
	w.Write(b)
}

// isTruthy reports whether a query-string flag reads as set (1/true/yes/on).
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.IndexByte(xf, ','); i >= 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// publicBook is the book served for a pair: its liftable offers (liftableOffers),
// each with its live state at the same index (statuses[i] is offers[i]'s), whose
// active_amount is what the order can be filled for now. A covenant order rests
// at whatever its outpoint holds, which after a part fill is less than the
// offer_amount it was signed for; a book that showed the signed size would
// overstate its depth.
func (s *Server) publicBook(pair *seqobv1.AssetPair, entries []offerstore.Entry) *seqobv1.PublicBook {
	now := time.Now().Unix()
	b := &seqobv1.PublicBook{Pair: pair}
	for i := range entries {
		e := &entries[i]
		if !s.offerLiftable(e.Offer, now) {
			continue
		}
		b.Offers = append(b.Offers, e.Offer)
		b.Statuses = append(b.Statuses, e.OrderStatus())
	}
	return b
}

// liftableOffers drops resting offers that NOTHING can currently fill: an interactive same-chain
// offer whose maker has no live connection to co-sign the lift.
//
// A book must only advertise prices a taker can actually take. These were being served at the TOP of
// book — better-priced than the real ones behind them — so every market order walked straight into
// them and came back "maker is not connected; this offer cannot be lifted right now". On one live
// pair that was 40 unfillable offers in front of 2 real ones: the DEX looked deep and could not trade.
//
// RemoveInteractiveByMaker already evicts these, but only when a maker DISCONNECTS. An offer posted
// by something that never held a connection at all — a seeding script, a maker that died before its
// socket registered — was never evicted by anything, because no disconnect ever happened. This closes
// that hole on the read side, so it holds regardless of how the ghost got there.
//
// DURABLE settlement types are deliberately kept, exactly as the eviction path keeps them:
//   - COVENANT offers are funded on-chain and fill permissionlessly with the maker offline — that is
//     the entire point of the passive CLOB.
//   - CROSS-CHAIN and LIGHTNING/sub-asset offers are serviced by an always-online agent that
//     reconnects; hiding them on a transient blip emptied the cross book once already.
func (s *Server) liftableOffers(offers []*seqobv1.Offer) []*seqobv1.Offer {
	if s.makerConns == nil {
		return offers
	}
	now := time.Now().Unix()
	out := offers[:0:0]
	for _, o := range offers {
		if s.offerLiftable(o, now) {
			out = append(out, o)
		}
	}
	return out
}

// offerLiftable is the per-offer rule behind liftableOffers.
func (s *Server) offerLiftable(o *seqobv1.Offer, now int64) bool {
	if s.makerConns == nil || !needsLiveMaker(o) {
		return true
	}
	if _, ok := s.makerConns.get(o.GetMakerPubkey()); ok {
		return true
	}
	// GRACE. Posting an offer and then connecting to serve it is a legitimate order: a maker may
	// REST-submit and open its socket a moment later. Only an offer that has sat unserved for
	// longer than any such gap is a ghost, so young ones are still shown.
	created := o.GetCreatedAtUnix()
	return created > 0 && now-int64(created) < ghostGraceSecs
}

// How long an interactive offer may rest with no connected maker before the book stops advertising
// it. Long enough to cover submit-then-connect and a socket blip; short enough that a book full of
// abandoned offers cannot keep quoting prices nobody can fill.
const ghostGraceSecs = 90

// needsLiveMaker reports whether filling this offer requires its maker to be connected right now.
// Only a plain interactive same-chain offer does: everything else either settles without the maker
// (covenant) or is served by an agent that reconnects (cross-chain, Lightning/sub-asset).
func needsLiveMaker(o *seqobv1.Offer) bool {
	return !offer.FilledWithoutMaker(o) && o.GetLightning() == nil && o.GetCrossChain() == nil
}
