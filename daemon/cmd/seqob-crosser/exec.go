package main

// exec.go — executing one crossing: two taker lifts, each driven on its offer's
// native rail with the crosser's OWN inventory.
//
// Driver embedding vs shelling out (deliberate, per family):
//
//   - pure-LN is EMBEDDED (internal/seqob/client.RunTakerPureLN, exec_pureln.go):
//     it has no on-chain leg, no refund state file and no recovery CLI, so
//     embedding costs nothing and avoids a subprocess on the hottest, fastest
//     rail.
//
//   - every family with an ON-CHAIN leg is SHELLED to its seqob-cli taker
//     command (xlift / xsell / xsublift / xsubbuy / xsubas / xsubas-sell / lift).
//     Those commands OWN the per-session refund state file and the matching
//     recovery subcommand (xrefund, xrefund-seq, xsubas-refund, …): re-embedding
//     the Run* drivers here would force this daemon to re-implement exactly that
//     recovery-critical persistence, duplicating it out of sync with the CLIs.
//     Shelling keeps one canonical owner of refund state per family; the crosser
//     journals WHICH state file each leg used and the exact resume command.
//
//   - COVENANT fills SHELL to `seqob-cli covfill` (internal/seqob/covfill, the
//     wallet-funded single-fill driver), consistent with the other on-chain
//     families. Unlike the HTLC families a covenant fill is ONE atomic FILL tx:
//     there is no counterparty leg, no refund branch and no session to resume,
//     so it keeps NO state file and its "resume" is informational only — on any
//     failure before broadcast nothing moved, and after broadcast the tx either
//     confirms or drops whole. It needs only the Sequentia node wallet
//     (-seq-rpc/-seq-wallet): the wallet pays asset B + the fee and receives
//     asset A.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// Caps is the crosser's configured inventory/endpoints — what decides which
// families it can execute.
type Caps struct {
	SeqRPC, SeqWallet          string // Sequentia (elements) node wallet: on-chain asset legs
	BtcRPC, BtcWallet          string // bitcoind wallet: on-chain BTC legs
	BtcChain                   string
	AssetLNSocket, BtcLNSocket string // CLN sockets: asset-LN and BTC-LN legs

	// Same-chain lift wiring (seqob-cli lift's real-taker seams).
	Esplora, TakerPriv, TakerBlinding string
	TakerKeyFile                      string // instead of the two keys: seqob-cli reads it (lift -taker-key-file)

	SeqobCli   string // path to the seqob-cli binary
	LNCli      string // lightning-cli, for the pure-Lightning preflight (hedge.go)
	StateDir   string // per-leg driver state files land here
	SpendFee   uint64
	BtcFeeRate float64
	MinBTCConf int
	LegTimeout time.Duration
}

// Executable implements the Executability check for the configured caps.
func (c *Caps) Executable(n *NormOrder) (bool, string) {
	if n.Flipped && n.Family != FamCovenant && n.Family != FamSameChain {
		return false, "flipped-orientation execution not wired for this family (its taker CLI sizes -amount in the offer's own base)"
	}
	need := func(ok bool, what string) (bool, string) {
		if !ok {
			return false, "missing " + what
		}
		return true, ""
	}
	switch n.Family {
	case FamPureLN:
		return need(c.AssetLNSocket != "" && c.BtcLNSocket != "", "-asset-ln-socket/-btc-ln-socket")
	case FamCross:
		if n.Quote != offer.BTCSentinel {
			return false, "cross family with a non-BTC quote"
		}
		return need(c.BtcRPC != "" && c.BtcWallet != "" && c.SeqRPC != "" && c.SeqWallet != "",
			"-btc-rpc/-btc-wallet/-seq-rpc/-seq-wallet")
	case FamSubmarine:
		if n.Quote != offer.BTCSentinel {
			return false, "submarine family with a non-BTC quote"
		}
		return need(c.SeqRPC != "" && c.SeqWallet != "" && c.BtcLNSocket != "",
			"-seq-rpc/-seq-wallet/-btc-ln-socket")
	case FamSubAsset:
		if c.AssetLNSocket == "" {
			return false, "missing -asset-ln-socket"
		}
		if n.Quote == offer.BTCSentinel {
			return need(c.BtcRPC != "" && c.BtcWallet != "", "-btc-rpc/-btc-wallet")
		}
		// Mixed same-chain (rails 7/8): the on-chain leg is a Sequentia-asset HTLC.
		return need(c.SeqRPC != "" && c.SeqWallet != "", "-seq-rpc/-seq-wallet (mixed same-chain quote)")
	case FamSameChain:
		return need(c.Esplora != "" && (c.TakerKeyFile != "" || (c.TakerPriv != "" && c.TakerBlinding != "")),
			"-esplora and -taker-key-file (or -taker-priv/-taker-blinding) (same-chain lift seams)")
	case FamCovenant:
		return need(c.SeqRPC != "" && c.SeqWallet != "", "-seq-rpc/-seq-wallet (covenant fill)")
	default:
		return false, "unknown family"
	}
}

// LegResult is one leg's terminal outcome.
type LegResult struct {
	Settled   bool
	Err       string
	StateFile string
	Resume    string // operator command that refunds/resumes a stranded leg
}

type Executor struct {
	Caps Caps
	Logf func(string, ...interface{})

	// liftMu serialises same-chain lifts. seqob-cli lift spends from the keyed
	// taker address, a single UTXO pool with no coin locking, so two lifts built
	// at the same moment pick the same coins and every broadcast after the first
	// fails bad-txns-inputs-missingorspent. The pairs run concurrently; only this
	// leg kind queues. Cross-chain and covenant legs draw on node wallets that
	// lock their own coins and stay parallel.
	liftMu sync.Mutex
}

// takeArg is the -amount value: the CLIs treat 0 as "the whole offer".
func takeArg(n *NormOrder, take uint64) uint64 {
	if n.Whole(take) {
		return 0
	}
	return take
}

// stateFile mints a unique per-leg driver state file.
func (e *Executor) stateFile(prefix string) string {
	return filepath.Join(e.Caps.StateDir, fmt.Sprintf("%s-%d.json", prefix, time.Now().UnixNano()))
}

// RunLeg executes one leg (a taker lift of `take` base atoms against n).
func (e *Executor) RunLeg(n *NormOrder, take uint64) LegResult {
	if n.Family == FamPureLN {
		return e.runPureLNLeg(n, take)
	}
	args, state, resume, err := e.legCommand(n, take)
	if err != nil {
		return LegResult{Err: err.Error()}
	}
	res := LegResult{StateFile: state, Resume: resume}
	if n.Family == FamSameChain {
		e.liftMu.Lock()
		defer e.liftMu.Unlock()
	}
	settled, runErr := e.runCLI(args)
	res.Settled = settled
	if runErr != nil {
		res.Err = runErr.Error()
	}
	return res
}

// legCommand builds the seqob-cli invocation for a shelled leg, plus the
// driver's state file and the operator resume command for a stranded outcome.
func (e *Executor) legCommand(n *NormOrder, take uint64) (args []string, stateFile, resume string, err error) {
	c := &e.Caps
	amt := takeArg(n, take)
	oid, mk := n.Offer.GetOfferId(), n.Offer.GetMakerPubkey()
	common := []string{"-relay", n.Relay, "-offer-id", oid, "-maker-pubkey", mk}

	switch {
	case n.Family == FamCross && n.Side == SideAsk: // buy the asset with on-chain BTC
		stateFile = e.stateFile("xlift")
		args = append([]string{"xlift", "-asset", n.Base,
			"-btc-wallet", c.BtcWallet, "-btc-chain", c.BtcChain,
			"-seq-wallet", c.SeqWallet,
			"-min-btc-conf", fmt.Sprint(c.MinBTCConf),
			"-amount", fmt.Sprint(amt), "-spend-fee", fmt.Sprint(c.SpendFee),
			"-btc-fee-rate", fmt.Sprint(c.BtcFeeRate),
			"-state-file", stateFile}, common...)
		resume = fmt.Sprintf("%s xrefund -state-file %s -btc-rpc <url> -btc-wallet %s -btc-chain %s -wait",
			c.SeqobCli, stateFile, c.BtcWallet, c.BtcChain)

	case n.Family == FamCross && n.Side == SideBid: // sell the asset for on-chain BTC
		stateFile = e.stateFile("xsell")
		args = append([]string{"xsell", "-asset", n.Base,
			"-btc-wallet", c.BtcWallet, "-btc-chain", c.BtcChain,
			"-seq-wallet", c.SeqWallet,
			"-min-btc-conf", fmt.Sprint(c.MinBTCConf),
			"-amount", fmt.Sprint(amt), "-spend-fee", fmt.Sprint(c.SpendFee),
			"-btc-fee-rate", fmt.Sprint(c.BtcFeeRate),
			"-state-file", stateFile}, common...)
		resume = fmt.Sprintf("%s xrefund-seq -state-file %s -seq-rpc <url> -seq-wallet %s -wait",
			c.SeqobCli, stateFile, c.SeqWallet)

	case n.Family == FamSubmarine && n.Side == SideBid: // sell the asset on-chain, receive BTC-LN
		stateFile = e.stateFile("xsublift")
		args = append([]string{"xsublift", "-asset", n.Base,
			"-seq-wallet", c.SeqWallet,
			"-ln-socket", c.BtcLNSocket,
			"-spend-fee", fmt.Sprint(c.SpendFee),
			"-state-file", stateFile}, common...)
		resume = fmt.Sprintf("asset refund path persists in %s (xsublift re-run refunds after T_seq)", stateFile)

	case n.Family == FamSubmarine && n.Side == SideAsk: // buy the asset, pay BTC-LN
		stateFile = e.stateFile("xsubbuy")
		args = append([]string{"xsubbuy", "-asset", n.Base,
			"-seq-wallet", c.SeqWallet,
			"-ln-socket", c.BtcLNSocket,
			"-spend-fee", fmt.Sprint(c.SpendFee),
			"-state-file", stateFile}, common...)
		resume = fmt.Sprintf("preimage persists in %s (re-run xsubbuy to retry the asset claim)", stateFile)

	case n.Family == FamSubAsset && n.Side == SideAsk: // buy: pay BTC (or quote asset) on-chain, receive asset over LN
		stateFile = e.stateFile("xsubas")
		args = append([]string{"xsubas", "-asset", n.Base,
			"-asset-ln-socket", c.AssetLNSocket,
			"-min-btc-conf", fmt.Sprint(c.MinBTCConf),
			"-amount", fmt.Sprint(amt), "-spend-fee", fmt.Sprint(c.SpendFee),
			"-state-file", stateFile}, common...)
		args = e.appendSubAssetChain(args, n)
		resume = fmt.Sprintf("%s xsubas-refund -state-file %s (after T_btc)", c.SeqobCli, stateFile)

	case n.Family == FamSubAsset && n.Side == SideBid: // sell: pay asset over LN, claim BTC (or quote asset) on-chain
		stateFile = e.stateFile("xsubas-sell")
		args = append([]string{"xsubas-sell", "-asset", n.Base,
			"-asset-ln-socket", c.AssetLNSocket,
			"-min-btc-conf", fmt.Sprint(c.MinBTCConf),
			"-amount", fmt.Sprint(amt), "-spend-fee", fmt.Sprint(c.SpendFee),
			"-state-file", stateFile}, common...)
		args = e.appendSubAssetChain(args, n)
		resume = fmt.Sprintf("claim material persists in %s (seqob-cli xsubas-claim-btc)", stateFile)

	case n.Family == FamCovenant:
		// covfill's -amount is in the covenant's SOLD-asset (asset A) atoms. A
		// non-flipped covenant offer advertises asset A as its own base, so an ASK
		// takes the canonical-base amount directly; on a BID (the covenant locks
		// our quote) the crosser receives asset A as the quote leg, so the take
		// converts at the offer's own price, floored — exactly RevenueFor, i.e.
		// what the plan already books as this leg's proceeds. A covenant posted on
		// the reverse orientation (Flipped) always lands here as a BID: it sells
		// its own base, which is our quote.
		covAmt := takeArg(n, take)
		if n.Side == SideBid && covAmt != 0 {
			covAmt = floorMulDiv(take, n.QuoteNum, n.BaseDen)
		}
		// What the fill pays is asset B: the quote on an ASK (priced by CostFor, the
		// same number the profitability gate used) and our own base on a BID. The
		// covenant's baked-in rate is the maker's, not the offer's; the ceiling makes
		// covfill refuse a covenant whose terms demand more than the offer advertised.
		maxPay := n.CostFor(take)
		if n.Side == SideBid {
			maxPay = take
		}
		args = append([]string{"covfill",
			"-base", n.Offer.GetPair().GetBaseAsset(), "-quote", n.Offer.GetPair().GetQuoteAsset(),
			"-seq-wallet", c.SeqWallet,
			"-spend-fee", fmt.Sprint(c.SpendFee),
			"-max-pay", fmt.Sprint(maxPay),
			"-amount", fmt.Sprint(covAmt)}, common...)
		// Single-tx atomic: no refund state, nothing to resume.
		resume = "covenant fill is one atomic FILL tx: nothing to refund on failure"

	case n.Family == FamSameChain:
		// seqob-cli lift is direction-agnostic (the taker pays want_asset), so one
		// command serves bid and ask; its base/quote are the OFFER's own pair, and
		// its -amount is in that pair's base (OwnAmount converts a flipped take).
		args = []string{"lift",
			"-relay", n.Relay,
			"-base", n.Offer.GetPair().GetBaseAsset(), "-quote", n.Offer.GetPair().GetQuoteAsset(),
			"-offer-id", oid, "-maker-pubkey", mk,
			"-amount", fmt.Sprint(n.OwnAmount(take)), // lift has no 0=whole convention; pass the size explicitly
			"-esplora", c.Esplora,
		}
		if c.TakerKeyFile != "" {
			args = append(args, "-taker-key-file", c.TakerKeyFile)
		} else {
			// The two keys reach seqob-cli in its environment (childEnv), never on its command line.
		}
		resume = "same-chain lift is a single co-signed tx: nothing to refund on failure"

	default:
		return nil, "", "", fmt.Errorf("no taker command for family %s side %s", n.Family, n.Side)
	}
	return args, stateFile, resume, nil
}

// appendSubAssetChain adds the on-chain-leg wiring for a sub-asset command:
// bitcoind for a BTC quote, the Sequentia node for a mixed same-chain quote.
func (e *Executor) appendSubAssetChain(args []string, n *NormOrder) []string {
	c := &e.Caps
	if n.Quote == offer.BTCSentinel {
		return append(args, "-btc-wallet", c.BtcWallet, "-btc-chain", c.BtcChain)
	}
	return append(args, "-quote-asset", n.Quote, "-xseq-wallet", c.SeqWallet)
}

// runCLI executes seqob-cli, streaming its output into the crosser log. Settled
// is decided by the CLIs' OWN contract: every taker command prints "SWAP
// SETTLED" exactly once on success — and some (xsubbuy) exit 0 on failure, so
// the exit code alone is NOT trusted.
// committedMarkers are the CLI log lines after which the leg has value on the
// line (an HTLC funded, a Lightning hold being paid). A CLI past one of these is
// never killed: SIGKILL mid-pay loses the preimage a settle would reveal, and
// mid-fund leaves a leg only the state file knows about. It gets an extended
// deadline and then a SIGTERM, which the CLIs handle by persisting and exiting.
var committedMarkers = []string{"leg funded", "HTLC funded", "funds now committed", "paying ", "PAID"}

// childEnv hands seqob-cli the node URLs (which carry their RPC passwords) and
// the taker's keys in its environment rather than on its command line, where
// `ps` and /proc/<pid>/cmdline show them to every user on the machine. seqob-cli
// reads each of these as the default of its flag of the same name.
func (c *Caps) childEnv() []string {
	env := os.Environ()
	for _, kv := range [][2]string{
		{"SEQOB_SEQ_RPC", c.SeqRPC}, {"SEQOB_XSEQ_RPC", c.SeqRPC}, {"SEQOB_BTC_RPC", c.BtcRPC},
		{"SEQOB_TAKER_PRIV", c.TakerPriv}, {"SEQOB_TAKER_BLINDING", c.TakerBlinding},
	} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}

func (e *Executor) runCLI(args []string) (settled bool, err error) {
	cmd := exec.Command(e.Caps.SeqobCli, args...)
	cmd.Env = e.Caps.childEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	cmd.Stderr = cmd.Stdout // one interleaved stream is enough for the log
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start %s %s: %w", e.Caps.SeqobCli, args[0], err)
	}
	var (
		mu        sync.Mutex
		committed bool
		timedOut  bool
	)
	done := make(chan struct{})
	go func() {
		// Soft deadline: a CLI that has committed nothing is told to stop (SIGTERM,
		// then SIGKILL after a grace); one that has committed funds gets four times
		// the leg timeout, then only a SIGTERM.
		soft := time.NewTimer(e.Caps.LegTimeout)
		defer soft.Stop()
		select {
		case <-done:
			return
		case <-soft.C:
		}
		mu.Lock()
		wasCommitted := committed
		timedOut = true
		mu.Unlock()
		if !wasCommitted {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
			}
			return
		}
		e.Logf("  [%s] past the %s leg timeout with funds committed; waiting for it to finish (no kill)", args[0], e.Caps.LegTimeout)
		select {
		case <-done:
		case <-time.After(4 * e.Caps.LegTimeout):
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastLines []string
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "SWAP SETTLED") {
			settled = true
		}
		for _, m := range committedMarkers {
			if strings.Contains(line, m) {
				mu.Lock()
				committed = true
				mu.Unlock()
				break
			}
		}
		lastLines = append(lastLines, line)
		if len(lastLines) > 20 {
			lastLines = lastLines[1:]
		}
		e.Logf("  [%s] %s", args[0], line)
	}
	waitErr := cmd.Wait()
	close(done)
	mu.Lock()
	wasTimeout := timedOut
	mu.Unlock()
	switch {
	case settled:
		return true, nil
	case wasTimeout:
		return false, fmt.Errorf("%s timed out after %s", args[0], e.Caps.LegTimeout)
	case waitErr != nil:
		return false, fmt.Errorf("%s: %v (tail: %s)", args[0], waitErr, strings.Join(lastLines, " | "))
	default:
		return false, fmt.Errorf("%s exited without SWAP SETTLED (tail: %s)", args[0], strings.Join(lastLines, " | "))
	}
}
