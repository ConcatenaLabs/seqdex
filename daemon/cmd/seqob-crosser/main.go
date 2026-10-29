// Command seqob-crosser is the REST-vs-REST crossing agent for the SeqOB DEX.
//
// The relays are non-custodial and cannot settle, so when two RESTING orders
// cross in price across offer families, nothing in the book itself can execute
// them against each other. This agent closes that gap: it polls every relay's
// REST orderbook, builds the per-pair union book across all families, and when
// the best executable bid meets or exceeds the best executable ask it TAKES BOTH
// sides through their native rails with its own inventory, keeping the spread as
// compensation. Because every family is taker-liftable by construction, the
// agent is universal across family pairs — a future family joins by normalizing
// its price fields (norm.go) and naming its taker driver (exec.go).
//
// It is one participant, not infrastructure: anyone can run it, several can run
// concurrently (first lift wins; the loser's leg simply fails to open), and it
// can never take user funds — both legs are ordinary taker lifts protected by
// the same co-sign/HTLC/hold atomicity every taker gets. The only capital at
// risk is the agent's own inventory drift when leg 2 fails after leg 1 settled
// (exposure.go bounds it; legorder.go minimizes the window).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type pairState struct {
	inFlight     bool
	failStreak   int
	backoffUntil time.Time
}

func main() {
	relaysFlag := flag.String("relays", "http://127.0.0.1:9955,http://127.0.0.1:9965,http://127.0.0.1:9966,http://127.0.0.1:9971",
		"comma-separated relay base URLs (the box fleet: cross 9955, pure-LN+submarine 9965, sub-asset buy 9966, sub-asset sell 9971; add the covenant relay URL to include the passive CLOB book)")
	pairsFlag := flag.String("pairs", "", `markets to watch, "BASEHEX/QUOTEHEX,..." (quote may be the BTC sentinel "BTC")`)
	auto := flag.Bool("auto", false, "discover markets from the relays' market lists instead of -pairs")
	poll := flag.Duration("poll", 12*time.Second, "book polling cadence (10-15s)")
	minTTL := flag.Duration("min-ttl", 5*time.Minute, "drop offers expiring sooner than this (a leg must outlive its settlement)")

	// Inventory / node wiring (the maker fleet's flag idiom).
	seqRPC := flag.String("seq-rpc", "", "Sequentia node RPC URL http://user:pass@host:port (on-chain asset legs)")
	seqWallet := flag.String("seq-wallet", "", "Sequentia node wallet holding the agent's on-chain asset inventory")
	btcRPC := flag.String("btc-rpc", "", "bitcoind RPC URL (on-chain BTC legs)")
	btcWallet := flag.String("btc-wallet", "", "bitcoind wallet holding the agent's BTC inventory")
	btcChain := flag.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	assetLN := flag.String("asset-ln-socket", "", "the agent's SeqLN-on-Sequentia lightning-rpc unix socket (asset-LN legs)")
	btcLN := flag.String("btc-ln-socket", "", "the agent's SeqLN-on-Bitcoin lightning-rpc unix socket (BTC-LN legs)")
	esplora := flag.String("esplora", "", "esplora API URL (enables same-chain lift legs)")
	takerPriv := flag.String("taker-priv", "", "same-chain lift on-chain signing key (32-byte hex)")
	takerBlinding := flag.String("taker-blinding", "", "same-chain lift blinding key (32-byte hex)")

	// Economics.
	minProfitBps := flag.Uint64("min-profit-bps", 10, "minimum net profit on deployed quote, basis points; never execute at a loss regardless")
	spendFeeAtoms := flag.Uint64("spend-fee-atoms", 1000, "estimated cost of ONE on-chain spend, in QUOTE atoms (per-family spend counts in detect.go); conservative operator estimate for the profitability gate")
	maxExposure := flag.Uint64("max-exposure-atoms", 1_000_000, "per-asset cap on |inventory drift| from failed second legs (0 = uncapped)")

	// Robustness / execution.
	stateDir := flag.String("state-dir", "crosser-state", "journal + per-leg driver state files")
	seqobCli := flag.String("seqob-cli", "seqob-cli", "path to the seqob-cli binary (shelled on-chain-leg takers)")
	legTimeout := flag.Duration("leg-timeout", 2*time.Hour, "hard cap on one shelled leg (cross legs wait out parent-chain confirmations)")
	spendFee := flag.Uint64("spend-fee", 1000, "HTLC spend fee target passed to the drivers (native sats / per-asset sized)")
	btcFeeRate := flag.Float64("btc-fee-rate", 2, "sat/vB for funding BTC HTLC legs")
	minBTCConf := flag.Int("min-btc-conf", 1, "confirmations required on counterparty BTC legs")
	maxBackoff := flag.Duration("max-backoff", 30*time.Minute, "cap on the per-pair failure backoff")
	clearStranded := flag.Bool("clear-stranded", false, "drop the journal's stranded records (AFTER resuming/refunding their driver state files) and exit")
	logToStderr := flag.Bool("logtostderr", true, "log to stderr (the fleet idiom); false = stdout")
	takeFlag := flag.String("take", "", `one trade and exit: "BASEHEX/QUOTEHEX" (quote may be "BTC"); prints one JSON line (take.go)`)
	takeSide := flag.String("take-side", "buy", "with -take: buy (take the best ask) or sell (take the best bid)")
	takeAmount := flag.Uint64("take-amount", 0, "with -take: about this many base atoms (the one offer taken may make it up to twice that, or less)")
	takeFamilies := flag.String("take-families", "", "with -take: the rails it may use, e.g. pureln,covenant,samechain (empty: any it can execute)")
	takeAvoid := flag.String("take-avoid", "", "with -take: maker pubkeys never taken from, comma-separated")
	flag.Parse()

	out := os.Stderr
	if !*logToStderr {
		out = os.Stdout
	}
	lg := log.New(out, "", log.LstdFlags|log.Lmsgprefix)
	logf := func(f string, a ...interface{}) { lg.Printf(f, a...) }

	journal, stranded, err := OpenJournal(*stateDir + "/journal.json")
	if err != nil {
		lg.Fatalf("journal: %v", err)
	}
	if *clearStranded {
		n := journal.ClearStranded()
		logf("cleared %d stranded record(s)", n)
		return
	}
	for _, r := range stranded {
		logf("STRANDED execution from a previous run on %s (leg1 %s/%s %s, leg2 %s/%s %s) — this pair stays BLOCKED until -clear-stranded;",
			r.Pair, r.First.Family, r.First.Side, r.First.Status, r.Second.Family, r.Second.Side, r.Second.Status)
		for _, leg := range []LegRecord{r.First, r.Second} {
			if leg.StateFile != "" {
				logf("  resume/refund leg via its driver state: %s  (%s)", leg.StateFile, leg.Resume)
			}
		}
	}

	caps := Caps{
		SeqRPC: *seqRPC, SeqWallet: *seqWallet,
		BtcRPC: *btcRPC, BtcWallet: *btcWallet, BtcChain: *btcChain,
		AssetLNSocket: *assetLN, BtcLNSocket: *btcLN,
		Esplora: *esplora, TakerPriv: *takerPriv, TakerBlinding: *takerBlinding,
		SeqobCli: *seqobCli, StateDir: *stateDir,
		SpendFee: *spendFee, BtcFeeRate: *btcFeeRate, MinBTCConf: *minBTCConf,
		LegTimeout: *legTimeout,
	}
	exec := &Executor{Caps: caps, Logf: logf}
	if *takeFlag != "" {
		bq := strings.SplitN(*takeFlag, "/", 2)
		if len(bq) != 2 || bq[0] == "" || bq[1] == "" {
			lg.Fatalf(`bad -take %q (want BASEHEX/QUOTEHEX)`, *takeFlag)
		}
		fams, avoid := map[string]bool{}, map[string]bool{}
		for _, f := range splitNonEmpty(*takeFamilies) {
			fams[f] = true
		}
		for _, k := range splitNonEmpty(*takeAvoid) {
			avoid[k] = true
		}
		r := RunTake(exec, splitNonEmpty(*relaysFlag), bq[0], bq[1], *takeSide, *takeAmount, fams, avoid, *minTTL, logf)
		fmt.Println(printTake(r))
		if !r.Settled {
			os.Exit(1)
		}
		return
	}
	ledger := NewLedger(*maxExposure, journal.SeedDrift())
	gate := GateConfig{MinProfitBps: *minProfitBps, SpendFeeAtoms: *spendFeeAtoms}
	relays := splitNonEmpty(*relaysFlag)

	var pairs [][2]string
	if *auto {
		pairs = DiscoverPairs(relays, logf)
		logf("discovered %d market(s) from %d relay(s)", len(pairs), len(relays))
	} else {
		for _, p := range splitNonEmpty(*pairsFlag) {
			bq := strings.SplitN(p, "/", 2)
			if len(bq) != 2 || bq[0] == "" || bq[1] == "" {
				lg.Fatalf(`bad -pairs entry %q (want BASEHEX/QUOTEHEX)`, p)
			}
			pairs = append(pairs, [2]string{bq[0], bq[1]})
		}
	}
	if len(pairs) == 0 {
		lg.Fatalf("no markets: pass -pairs or -auto")
	}
	for _, p := range pairs {
		logf("watching %s/%s", short(p[0]), short(p[1]))
	}

	var mu sync.Mutex
	states := map[string]*pairState{}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(*poll)
	defer ticker.Stop()

	for {
		if *auto {
			// Refresh discovery lazily so new markets join without a restart.
			if fresh := DiscoverPairs(relays, func(string, ...interface{}) {}); len(fresh) > 0 {
				pairs = fresh
			}
		}
		for _, p := range pairs {
			base, quote := p[0], p[1]
			pairKey := base + "/" + quote

			mu.Lock()
			st := states[pairKey]
			if st == nil {
				st = &pairState{}
				states[pairKey] = st
			}
			busy := st.inFlight || time.Now().Before(st.backoffUntil) || journal.StrandedFor(pairKey)
			mu.Unlock()
			if busy {
				continue
			}

			raw, statuses := FetchPairOffers(relays, base, quote, logf)
			union := ApplyStatuses(BuildUnion(raw, base, quote, time.Now(), *minTTL, logf), statuses)
			if len(union) == 0 {
				continue
			}
			d := Detect(union, caps.Executable, gate)
			logDetection(logf, pairKey, d)
			if d.Plan == nil {
				continue
			}
			plan := d.Plan
			if err := ledger.Reserve(plan); err != nil {
				logf("[%s] SKIP %v", pairKey, err)
				continue
			}
			mu.Lock()
			st.inFlight = true
			mu.Unlock()
			go func(pairKey string, plan *CrossPlan, st *pairState) {
				ok := executeCrossing(pairKey, plan, exec, ledger, journal, logf)
				mu.Lock()
				st.inFlight = false
				if ok {
					st.failStreak = 0
				} else {
					st.failStreak++
					back := time.Duration(1<<uint(min(st.failStreak, 10))) * 30 * time.Second
					if back > *maxBackoff {
						back = *maxBackoff
					}
					st.backoffUntil = time.Now().Add(back)
					logf("[%s] backing off %s after %d consecutive failure(s)", pairKey, back, st.failStreak)
				}
				mu.Unlock()
			}(pairKey, plan, st)
		}
		select {
		case <-ticker.C:
		case sig := <-stop:
			logf("%v: exiting (in-flight legs keep their driver state files; see %s/journal.json)", sig, *stateDir)
			return
		}
	}
}

func logDetection(logf func(string, ...interface{}), pair string, d Detection) {
	bb, ba := "-", "-"
	if d.BestBid != nil {
		bb = fmt.Sprintf("%s@%d/%d(%s)", d.BestBid.Family, d.BestBid.QuoteNum, d.BestBid.BaseDen, short(d.BestBid.ID()))
	}
	if d.BestAsk != nil {
		ba = fmt.Sprintf("%s@%d/%d(%s)", d.BestAsk.Family, d.BestAsk.QuoteNum, d.BestAsk.BaseDen, short(d.BestAsk.ID()))
	}
	if d.Plan != nil {
		p := d.Plan
		logf("[%s] CROSS bid %s >= ask %s: take %d base, cost %d, revenue %d, gross %d (est costs %d) — %s first (%s)",
			pair, bb, ba, p.Take, p.Cost, p.Revenue, p.Gross, p.EstCost, p.First.Family, p.OrderReason)
		return
	}
	logf("[%s] no execution: %s (best bid %s | best ask %s)", pair, d.Skip, bb, ba)
}

// executeCrossing runs both legs in the planned order, journaling each phase.
// Returns true when both legs settled.
func executeCrossing(pair string, p *CrossPlan, ex *Executor, ledger *Ledger, journal *Journal, logf func(string, ...interface{})) (ok bool) {
	defer ledger.Release(p)
	rec := &ExecRecord{
		Pair: pair, StartedAt: time.Now(), Take: p.Take, Cost: p.Cost, Revenue: p.Revenue,
		First:  legRecord(p.First, "pending"),
		Second: legRecord(p.Second, "pending"),
	}
	if err := journal.Begin(pair, rec); err != nil {
		logf("[%s] ABORT: journal write failed: %v (refusing to execute unjournaled)", pair, err)
		return false
	}

	_ = journal.SetLeg(pair, false, "running", "", "")
	logf("[%s] leg 1/2: %s %s %d base against %s on %s", pair, p.First.Side, p.First.Family, p.Take, short(p.First.ID()), p.First.Relay)
	r1 := ex.RunLeg(p.First, p.Take)
	if !r1.Settled {
		_ = journal.SetLeg(pair, false, "failed", r1.StateFile, r1.Resume)
		_ = journal.End(pair, nil) // nothing executed: zero drift
		logf("[%s] leg 1 FAILED (nothing executed, no exposure): %s", pair, r1.Err)
		return false
	}
	_ = journal.SetLeg(pair, false, "settled", r1.StateFile, r1.Resume)
	_ = journal.SetLeg(pair, true, "running", "", "")
	logf("[%s] leg 1 settled; leg 2/2: %s %s %d base against %s on %s", pair, p.Second.Side, p.Second.Family, p.Take, short(p.Second.ID()), p.Second.Relay)

	r2 := ex.RunLeg(p.Second, p.Take)
	if !r2.Settled {
		_ = journal.SetLeg(pair, true, "failed", r2.StateFile, r2.Resume)
		ledger.RecordResidue(p)
		bd, qd := worstCase(p)
		_ = journal.End(pair, map[string]int64{p.Bid.Base: bd, p.Bid.Quote: qd})
		logf("[%s] leg 2 FAILED AFTER leg 1 settled: %s", pair, r2.Err)
		logf("[%s] INVENTORY DRIFT booked: %+d %s base, %+d %s quote (running drift: %v; capped at -max-exposure-atoms)",
			pair, bd, short(p.Bid.Base), qd, short(p.Bid.Quote), ledger.Drift())
		if r2.Resume != "" {
			logf("[%s]   leg-2 recovery: %s", pair, r2.Resume)
		}
		return false
	}
	_ = journal.SetLeg(pair, true, "settled", r2.StateFile, r2.Resume)
	_ = journal.End(pair, nil)
	logf("[%s] CROSSING SETTLED: took %d base both ways, kept %d quote gross (est costs %d)", pair, p.Take, p.Gross, p.EstCost)
	return true
}

func legRecord(n *NormOrder, status string) LegRecord {
	return LegRecord{
		Family: n.Family.String(), Side: n.Side.String(),
		OfferID: n.Offer.GetOfferId(), Maker: n.Offer.GetMakerPubkey(),
		Relay: n.Relay, Status: status,
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
