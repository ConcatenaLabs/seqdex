package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// SetTradeReporters names the makers whose own trade reports the relay records
// (POST /v1/trades/report): compressed public keys, hex. A report from any other
// key is refused, so nobody else can print a trade on a market's chart.
func (s *Server) SetTradeReporters(keys []string) {
	m := make(map[string]bool)
	for _, k := range keys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			m[k] = true
		}
	}
	s.reportersMu.Lock()
	s.reporters = m
	s.reportersMu.Unlock()
}

func (s *Server) isReporter(k string) bool {
	s.reportersMu.Lock()
	defer s.reportersMu.Unlock()
	return s.reporters[strings.ToLower(k)]
}

// handleTradeReport records a trade a trusted maker settled on a rail the relay
// cannot watch (offer.TradeReport): POST /v1/trades/report. The answer says
// whether it was recorded now (false: the same fill was recorded before).
func (s *Server) handleTradeReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var rep offer.TradeReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&rep); err != nil {
		httpErr(w, http.StatusBadRequest, "decode trade report: "+err.Error())
		return
	}
	if !offerstore.ReportedRails[rep.Rail] {
		httpErr(w, http.StatusBadRequest, "a maker reports trades on "+offerstore.RailLeafMaker+" or "+offerstore.RailBTCGateway+" only; the relay records the others itself")
		return
	}
	give, want, err := offer.ParseTradeReport(&rep)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid trade report: "+err.Error())
		return
	}
	if !s.isReporter(rep.Reporter) {
		httpErr(w, http.StatusForbidden, "this relay records no trade reports from "+rep.Reporter)
		return
	}
	id := strings.ToLower(rep.Reporter) + ":" + rep.Rail + ":" + rep.ID
	recorded := s.store.RecordReported(rep.GiveAsset, give, rep.WantAsset, want, rep.Rail, id)
	if recorded {
		s.log.Printf("trade report %s: %s gave %d %s for %d %s", rep.Rail, short(rep.Reporter), give, short(rep.GiveAsset), want, short(rep.WantAsset))
	}
	writeJSON(w, map[string]interface{}{"recorded": recorded, "id": id})
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// tradeJSON is one trade as the market endpoints serve it, in the direction
// the reader asked for.
type tradeJSON struct {
	Ts    int64   `json:"ts"`         // unix seconds
	Price float64 `json:"price"`      // quote atoms per base atom
	Size  uint64  `json:"size"`       // base atoms
	Quote uint64  `json:"quote_size"` // quote atoms
	Side  string  `json:"side"`       // "buy": the taker bought the base; "sell": sold it
	Rail  string  `json:"rail,omitempty"`
	ID    string  `json:"id,omitempty"`
}

func tradeOut(t offerstore.Trade) tradeJSON {
	side := "sell"
	if t.TakerBuys {
		side = "buy"
	}
	return tradeJSON{Ts: t.TS.Unix(), Price: t.Price, Size: t.Size, Quote: t.Quote, Side: side, Rail: t.Rail, ID: t.ID}
}

// tickerWindow is the ticker's window.
const tickerWindow = 24 * time.Hour

// handleTicker serves a market's last day: GET /v1/market/{base}/{quote}/ticker.
// last and last_ts are the newest trade's, however old; the 24-hour figures
// (open, high, low, change_pct, volumes, trades) cover the trades of the last
// 24 hours and are absent when there were none. best_bid and best_ask are the
// liftable transparent book's, both orientations of the pair taken together.
func (s *Server) handleTicker(w http.ResponseWriter, _ *http.Request, pair *seqobv1.AssetPair) {
	out := map[string]interface{}{"base": pair.BaseAsset, "quote": pair.QuoteAsset}
	trades := s.store.TradesFor(pair, 0)
	if n := len(trades); n > 0 {
		out["last"] = trades[n-1].Price
		out["last_ts"] = trades[n-1].TS.Unix()
	}
	since := time.Now().Add(-tickerWindow)
	var open, hi, lo, last float64
	var vb, vq uint64
	cnt := 0
	for _, t := range trades {
		if t.TS.Before(since) {
			continue
		}
		if cnt == 0 {
			open, hi, lo = t.Price, t.Price, t.Price
		}
		cnt++
		if t.Price > hi {
			hi = t.Price
		}
		if t.Price < lo {
			lo = t.Price
		}
		last = t.Price
		vb += t.Size
		vq += t.Quote
	}
	out["trades_24h"] = cnt
	if cnt > 0 {
		out["open_24h"], out["high_24h"], out["low_24h"] = open, hi, lo
		out["volume_24h"], out["quote_volume_24h"] = vb, vq
		if open > 0 {
			out["change_pct_24h"] = (last - open) / open * 100
		}
	}
	bid, ask := s.bestOf(pair)
	if bid > 0 {
		out["best_bid"] = bid
	}
	if ask > 0 {
		out["best_ask"] = ask
	}
	writeJSON(w, out)
}

// bestOf is the best bid and ask of the pair's liftable transparent book, read
// from both orientations (an ask of B/A is a bid of A/B at the inverse price).
func (s *Server) bestOf(pair *seqobv1.AssetPair) (bid, ask float64) {
	for _, m := range s.markets() {
		p := m.GetPair()
		if p == nil || p.GetConfidential() {
			continue
		}
		b, a := m.GetBestBid(), m.GetBestAsk()
		switch {
		case p.GetBaseAsset() == pair.BaseAsset && p.GetQuoteAsset() == pair.QuoteAsset:
		case p.GetBaseAsset() == pair.QuoteAsset && p.GetQuoteAsset() == pair.BaseAsset:
			b, a = inv(a), inv(b)
		default:
			continue
		}
		if b > bid {
			bid = b
		}
		if a > 0 && (ask == 0 || a < ask) {
			ask = a
		}
	}
	return bid, ask
}

func inv(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return 1 / x
}

// followedTrade is one trade as GET /v1/trades serves it to a following relay:
// the market's canonical direction, its time to the nanosecond.
type followedTrade struct {
	TS    time.Time `json:"ts"`
	Price float64   `json:"price"`
	Size  uint64    `json:"size"`
	Quote uint64    `json:"quote"`
	Buy   bool      `json:"buy,omitempty"`
	Rail  string    `json:"rail,omitempty"`
	ID    string    `json:"id,omitempty"`
	Txid  string    `json:"txid,omitempty"`
}

// handleAllTrades serves every market's trades this relay recorded itself
// (none it follows), each market under its canonical key "base/quote": GET
// /v1/trades. A relay that serves one market view over several relays follows
// the others' with it (seqobd -trade-follow).
func (s *Server) handleAllTrades(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := make(map[string][]followedTrade)
	for pk, ring := range s.store.LocalTrades() {
		l := make([]followedTrade, 0, len(ring))
		for _, t := range ring {
			l = append(l, followedTrade{TS: t.TS, Price: t.Price, Size: t.Size, Quote: t.Quote, Buy: t.TakerBuys, Rail: t.Rail, ID: t.ID, Txid: t.Txid})
		}
		out[pk] = l
	}
	writeJSON(w, map[string]interface{}{"markets": out})
}

// FollowTrades keeps each relay's trades (GET <base>/v1/trades) in this one's
// market view, asked every interval until stop closes. A relay that does not
// answer keeps what it last answered; what it retracted is gone at its next
// answer.
func (s *Server) FollowTrades(bases []string, interval time.Duration, stop <-chan struct{}) {
	client := &http.Client{Timeout: 10 * time.Second}
	for _, b := range bases {
		base := strings.TrimRight(strings.TrimSpace(b), "/")
		if base == "" {
			continue
		}
		go func() {
			var failing bool
			for {
				if err := s.followOnce(client, base); err != nil {
					if !failing {
						s.log.Printf("trade-follow %s: %v (keeping its last answer)", base, err)
					}
					failing = true
				} else {
					failing = false
				}
				select {
				case <-stop:
					return
				case <-time.After(interval):
				}
			}
		}()
	}
}

func (s *Server) followOnce(client *http.Client, base string) error {
	resp, err := client.Get(base + "/v1/trades")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusErr{resp.StatusCode}
	}
	var body struct {
		Markets map[string][]followedTrade `json:"markets"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<20)).Decode(&body); err != nil {
		return err
	}
	m := make(map[string][]offerstore.Trade, len(body.Markets))
	for pk, l := range body.Markets {
		for _, t := range l {
			if t.Price <= 0 || t.Size == 0 {
				continue
			}
			m[pk] = append(m[pk], offerstore.Trade{TS: t.TS, Price: t.Price, Size: t.Size, Quote: t.Quote, TakerBuys: t.Buy, Rail: t.Rail, ID: t.ID, Txid: t.Txid})
		}
	}
	s.store.SetMirror(base, m)
	return nil
}

type httpStatusErr struct{ code int }

func (e *httpStatusErr) Error() string { return "answered " + http.StatusText(e.code) }
