// Command seqobd is the SeqOB order-book relay daemon.
//
// It is NON-CUSTODIAL: it stores signed offers, serves the per-pair book
// (snapshot + deltas), and couriers OPAQUE end-to-end-encrypted swap-session
// messages between two peers. It holds NO wallet, NO keys, and NO funds, and it
// never decrypts the courier payload.
//
// Phase 1 wires: offerstore + validator (no-op liveness probe) + session router
// (no-op reorg watcher) + the REST/WS API.
//
// Chain watcher: when a read-only node RPC is configured (-node-host/-node-port
// with cookie creds, or -node-rpc host:port), seqobd starts the covenant
// chain-watcher (internal/seqob/watcher) as a goroutine holding the same book.
// It reconciles resting COVENANT orders to the node's CURRENT tip — removing
// ghosts whose funding was undone by a Bitcoin-driven reorg (anchoring
// supremacy), re-resting partial-fill remainders, holding unconfirmed spends,
// and re-opening fills that never confirmed. The relay itself stays keyless and
// fundless; the watcher only READS the node and drives the covenant-aware book
// ops.
//
// Covenant book: with -covenant-book, the relay keeps its covenant orders in a
// file and lists them again when it starts, each checked again (signature,
// terms, expiry, its outpoint on the chain). Interactive offers are never kept.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/api"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/reorg"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
)

// splitHostPort parses "host:port" for the node RPC endpoint.
func splitHostPort(hp string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		listen         = flag.String("listen", env("SEQOB_LISTEN", ":9955"), "HTTP listen address (env SEQOB_LISTEN)")
		nodeRPC        = flag.String("node-rpc", env("SEQOB_NODE_RPC", ""), "read-only Sequentia node RPC host:port for the covenant chain-watcher (env SEQOB_NODE_RPC; empty disables the watcher)")
		nodeUser       = flag.String("node-rpc-user", env("SEQOB_NODE_RPC_USER", ""), "node JSON-RPC username / cookie user (env SEQOB_NODE_RPC_USER)")
		nodePass       = flag.String("node-rpc-pass", env("SEQOB_NODE_RPC_PASS", ""), "node JSON-RPC password / cookie pass (env SEQOB_NODE_RPC_PASS)")
		watchInterval  = flag.Duration("watch-interval", 5*time.Second, "covenant chain-watcher reconcile interval")
		sessionTTL     = flag.Duration("session-deadline", 2*time.Minute, "lift session co-sign deadline")
		xsessionTTL    = flag.Duration("xsession-deadline", 3*time.Hour, "courier deadline for CROSS-CHAIN lift sessions (they span a real parent-chain confirmation; 0 = use -session-deadline)")
		expirySweep    = flag.Duration("expiry-sweep", 15*time.Second, "offer expiry sweep interval")
		sessionSweep   = flag.Duration("session-sweep", 10*time.Second, "lift-session deadline sweep interval")
		minExpiry      = flag.Duration("min-expiry", 30*time.Second, "minimum offer expiry horizon")
		maxExpiry      = flag.Duration("max-expiry", 7*24*time.Hour, "maximum offer expiry horizon")
		offersPerMin   = flag.Int("offers-per-min", 60, "max offers/min per maker_pubkey")
		offersPerMinI  = flag.Int("offers-per-min-ip", 120, "max offers/min per IP")
		tradeLog       = flag.String("trade-log", env("SEQOB_TRADE_LOG", ""), "path to an append-only JSONL trade log so last_price/trades/candles survive a relay restart (env SEQOB_TRADE_LOG; empty = in-memory only)")
		covenantBook   = flag.String("covenant-book", env("SEQOB_COVENANT_BOOK", ""), "path to a file keeping the covenant orders across a relay restart, each checked again against the chain when read back (env SEQOB_COVENANT_BOOK; empty = in memory only)")
		finalDepth     = flag.Int64("fill-anchor-depth", watcher.DefaultFinalAnchorDepth, "Bitcoin blocks that must bury a covenant fill's anchor, its block certified, before the relay records it final (an offer's min_anchor_depth raises it)")
		arcaOperators  = flag.String("arca-operator", env("SEQOB_ARCA_OPERATORS", ""), "comma-separated base URLs of Arca operators the relay follows: their rounds' fills of covenant orders are recorded as theirs, and orders resting in their trees are listed, followed and delisted from their state views (env SEQOB_ARCA_OPERATORS; needs -node-rpc)")
		restingEvery   = flag.Duration("resting-interval", 3*time.Second, "how often the relay asks each operator how the orders resting in its tree stand")
		interactiveCap = flag.Uint64("interactive-cap", 0, "P3.10 soft per-maker committed-capital cap per offer_asset (atoms): an interactive offer that pushes a maker's cumulative committed capital past this is flagged 'demoted' (phantom depth); 0 disables")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "seqobd ", log.LstdFlags|log.Lmsgprefix)

	store := offerstore.New(nil)
	store.SetInteractiveCap(*interactiveCap)
	if *interactiveCap > 0 {
		logger.Printf("interactive oversell soft cap: %d atoms/maker/asset (offers past it are demoted)", *interactiveCap)
	}
	if *tradeLog != "" {
		if err := store.EnableTradeLog(*tradeLog); err != nil {
			logger.Printf("trade-log %s: %v (continuing in-memory)", *tradeLog, err)
		} else {
			logger.Printf("durable trade log: %s", *tradeLog)
		}
	}

	vcfg := validator.DefaultConfig()
	vcfg.MinExpiry = *minExpiry
	vcfg.MaxExpiry = *maxExpiry
	vcfg.MaxOffersPerMinPerPubkey = *offersPerMin
	vcfg.MaxOffersPerMinPerIP = *offersPerMinI
	// A node RPC (parsed once here) powers the covenant submit probe, the covenant
	// chain-watcher and the interactive-fill reorg watcher (P3.9).
	var nodeHost string
	var nodePort int
	var probe validator.LivenessProbe = validator.NoopLivenessProbe{}
	if *nodeRPC != "" {
		h, p, err := splitHostPort(*nodeRPC)
		if err != nil {
			logger.Fatalf("bad -node-rpc %q: %v", *nodeRPC, err)
		}
		nodeHost, nodePort = h, p
		// A covenant offer is admitted only if its outpoint is funded as advertised.
		probe = watcher.SubmitProbe{Chain: watcher.NewRPCChain(nodeHost, nodePort, *nodeUser, *nodePass)}
	}
	// The Arca operators the relay follows: an order resting in one's tree is
	// admitted only when the operator's state view shows it at its locator.
	var arcaOps *watcher.ArcaRounds
	if ops := strings.TrimSpace(*arcaOperators); ops != "" {
		if *nodeRPC == "" {
			logger.Fatalf("-arca-operator needs -node-rpc: the relay checks each operator's chain and measures fills on its own node")
		}
		chain := watcher.NewRPCChain(nodeHost, nodePort, *nodeUser, *nodePass)
		genesis, err := watcher.GenesisHash(chain)
		if err != nil {
			logger.Fatalf("-arca-operator needs the node's genesis block: %v", err)
		}
		arcaOps = watcher.NewArcaRounds(strings.Split(ops, ","), genesis)
		vcfg.ListResting = true
		probe = watcher.Probes{Chain: chain, Resting: &watcher.RestingProbe{Ops: arcaOps, Chain: chain}}
	}
	v := validator.New(vcfg, probe)

	// onReopen returns an order to the book. It fires in two cases (Principle 1):
	//   - a lift ABORTS/times out before settlement: the order was never removed, so
	//     this is a no-op log (it is still resting).
	//   - a settled interactive lift's Bitcoin anchor is ORPHANED (reorg-undo, P3.9):
	//     the session carries the cached offer + fill, so re-insert the exact order
	//     (restoring the filled size on top of whatever still rests) AND retract the
	//     phantom trade from the ring + JSONL + last_price. No confirmation wait (§5b).
	onReopen := func(s *session.Session) {
		k := offerstore.Key{MakerPubkey: s.MakerPubkey, OfferID: s.OfferID}
		o := s.SettledOffer()
		if o == nil {
			if _, ok := store.Get(k); ok {
				logger.Printf("session %s ended; order %s/%s still resting", s.ID, s.MakerPubkey, s.OfferID)
			}
			return
		}
		// Restore the amount the reorged fill consumed on top of whatever still rests
		// (0 if a full fill had removed it), so the order returns to its pre-fill size.
		cur := uint64(0)
		if e, ok := store.Get(k); ok {
			cur = e.ActiveAmount
		}
		restore := cur + s.SettledFill()
		if err := store.Reopen(o, restore); err != nil {
			logger.Printf("session %s reorg re-open %s/%s: %v", s.ID, s.MakerPubkey, s.OfferID, err)
		}
		n := 0
		if txid := s.SettleTxid(); txid != "" {
			n = store.RetractTrade(o.GetPair(), txid)
		}
		logger.Printf("session %s REORG-UNDO: re-opened %s/%s to %d and retracted %d trade(s) for %s",
			s.ID, s.MakerPubkey, s.OfferID, restore, n, s.SettleTxid())
	}

	// Interactive-fill reorg watcher: watch each interactive settlement tx and, on a
	// Bitcoin reorg that orphans it, fire onReopen. Enabled only with a node RPC (it
	// needs the chain); otherwise the Phase-1 no-op leaves settled orders as-is.
	var reorgWatcher session.ReorgWatcher = session.NoopReorgWatcher{}
	var reorgRun *reorg.Watcher
	if *nodeRPC != "" {
		reorgRun = reorg.New(reorg.NewRPCProbe(nodeHost, nodePort, *nodeUser, *nodePass), logger)
		reorgWatcher = reorgRun
	}

	router := session.NewRouter(session.Options{
		Deadline: *sessionTTL,
		Reorg:    reorgWatcher,
		OnReopen: onReopen,
	})

	// The covenant orders kept across the last restart are listed again before the
	// relay answers anything, and before the watcher's first pass follows them.
	var keptDone chan struct{}
	keepStop := make(chan struct{})
	if *covenantBook != "" {
		n, f, d := restoreCovenantBook(*covenantBook, store, v, logger)
		logger.Printf("covenant book %s: %d covenant orders listed again, %d held for the watcher to follow a spend, %d dropped", *covenantBook, n, f, d)
		keptDone = make(chan struct{})
		go keepCovenantBook(&offerstore.CovenantBookWriter{Path: *covenantBook}, store, 2*time.Second, keepStop, keptDone, logger)
	}

	srv := api.New(store, v, router, logger)
	srv.SetCrossSessionDeadline(*xsessionTTL)

	stop := make(chan struct{})
	go store.RunExpirySweeper(*expirySweep, stop)
	go router.RunDeadlineSweeper(*sessionSweep, stop)

	// Covenant chain-watcher + interactive-fill reorg watcher: both reconcile to the
	// node's current tip. Enabled only when a node RPC is configured.
	if *nodeRPC != "" {
		chain := watcher.NewRPCChain(nodeHost, nodePort, *nodeUser, *nodePass)
		w := watcher.New(chain, watcher.NewStoreBook(store), logger)
		w.SetFinalAnchorDepth(*finalDepth)
		if arcaOps != nil {
			w.SetRounds(arcaOps)
			logger.Printf("covenant fills by the rounds of the Arca operators %s are recorded as theirs", *arcaOperators)
			rf := watcher.NewResting(arcaOps, chain, store, logger)
			rf.SetFinalAnchorDepth(*finalDepth)
			go rf.Run(*restingEvery, stop)
			logger.Printf("orders resting in the trees of %s are followed every %s (a fill is final once the round its order rests on is final at anchor depth %d)", *arcaOperators, *restingEvery, *finalDepth)
		}
		go w.Run(*watchInterval, stop)
		logger.Printf("covenant chain-watcher enabled (node %s, every %s; a fill is final once its block is certified and its anchor buried %d)", *nodeRPC, *watchInterval, *finalDepth)
		if reorgRun != nil {
			go reorgRun.Run(*watchInterval, stop)
			logger.Printf("interactive-fill reorg watcher enabled (node %s, every %s)", *nodeRPC, *watchInterval)
		}
	} else {
		logger.Printf("covenant chain-watcher + reorg watcher DISABLED (no -node-rpc): resting covenant orders are not reconciled and interactive fills are not reorg-undone")
	}

	// Bound every phase of a plain HTTP exchange so a slow or silent peer cannot hold a
	// handler goroutine open indefinitely. The WebSocket upgrade hijacks the connection
	// and runs its own ping/pong deadline, so these only govern REST.
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Printf("listening on %s (non-custodial relay; no wallet, no keys)", *listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Printf("shutting down...")
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Printf("graceful shutdown error: %v", err)
	}
	close(keepStop)
	if keptDone != nil {
		<-keptDone // the covenant book written as the relay stops
	}
}
