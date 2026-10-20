// Command seqob-watcher is the SeqOB chain-watcher: the safety layer that keeps
// the keyless relay's resting COVENANT order book consistent with actual
// Sequentia chain state.
//
// Why a distinct component. A covenant order is FUNDED on-chain (placing it
// broadcasts the taproot UTXO; the signed offer on the relay only ADVERTISES
// that outpoint), and the relay (seqobd) is keyless and fundless and never
// watches the chain. So the book drifts: a covenant gets FILLED with the maker
// offline, a PARTIAL fill re-creates a smaller remainder covenant at a new
// outpoint, a fill is broadcast then dropped/RBF'd, or — because every Sequentia
// block references a Bitcoin header (Bitcoin anchoring is SUPREME consensus law)
// — a Bitcoin reorg makes the funding UTXO VANISH in real time. The watcher
// reconciles the book to the node's CURRENT tip: it records each fill from the
// filling transaction's outputs (one per covenant input, k its index), re-rests
// partial-fill remainders at (txid, 2k+1), removes a filled order once its fill
// is final (certified, anchor buried), undoes a fill whose block leaves the
// chain, removes orders whose funding is gone (ghosts), holds unconfirmed spends
// pending, and re-opens fills that never confirmed. Its only powers over the
// book are the covenant-aware reconcile operations; it holds no keys and no funds.
//
// The reconciliation core (internal/seqob/watcher) is agnostic to how it reaches
// the node and the book. In production the watcher runs as a goroutine inside
// seqobd (wired when seqobd is given -node-rpc), holding the same in-memory book
// (BookController == offerstore.Store). This standalone binary exposes the same
// core two ways:
//
//	classify   (reads a JSON CovState on stdin)
//	    Emits the reconciliation VERDICT (LIVE / PARTIAL_FILL / FILLED /
//	    UNCONFIRMED_SPEND / GHOST) the classifier produces for that on-chain
//	    state. This is the pure, node-independent core the regtest proof and the
//	    Go unit tests exercise (mirrors seqob-settler's `plan`).
//
//	run    (documented) the always-online loop: connect to a node's read-only
//	    JSON-RPC and reconcile a book exposed over a relay admin endpoint. The
//	    load-bearing integration is the goroutine embedded in seqobd; a separate
//	    process needs the relay to expose an admin book surface (not built here).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
)

func die(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "seqob-watcher: "+format+"\n", a...)
	os.Exit(1)
}

// classifyInput is a CovState plus the expected covenant program, as JSON. It is
// the same shape the RPC ChainView produces, so the regtest harness can feed a
// real on-chain state and assert the verdict.
type classifyInput struct {
	Unspent          bool   `json:"unspent"`
	Value            uint64 `json:"value"`
	AssetDisplay     string `json:"asset_display"`
	SPKHex           string `json:"spk_hex"`
	ConfirmedUTXO    bool   `json:"confirmed_utxo"`
	SpenderTxid      string `json:"spender_txid"`
	SpenderConfirmed bool   `json:"spender_confirmed"`
	RemainderPresent bool   `json:"remainder_present"`
	RemainderVout    uint32 `json:"remainder_vout"`
	RemainderValue   uint64 `json:"remainder_value"`

	// Expected program.
	ExpectSPKHex        string `json:"expect_spk_hex"`
	ExpectAssetADisplay string `json:"expect_asset_a_display"`
	ExpectMinLot        uint64 `json:"expect_min_lot"`
}

func main() {
	if len(os.Args) < 2 {
		die("usage: seqob-watcher <classify|run>")
	}
	switch os.Args[1] {
	case "classify":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			die("read stdin: %v", err)
		}
		var in classifyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			die("parse classify json: %v", err)
		}
		state := watcher.CovState{
			Unspent:          in.Unspent,
			Value:            in.Value,
			AssetDisplay:     in.AssetDisplay,
			SPKHex:           in.SPKHex,
			ConfirmedUTXO:    in.ConfirmedUTXO,
			SpenderTxid:      in.SpenderTxid,
			SpenderConfirmed: in.SpenderConfirmed,
			Remainder: watcher.RemainderOut{
				Present: in.RemainderPresent,
				Vout:    in.RemainderVout,
				Value:   in.RemainderValue,
			},
		}
		exp := watcher.Expect{
			OrderSPKHex:   in.ExpectSPKHex,
			AssetADisplay: in.ExpectAssetADisplay,
			MinLot:        in.ExpectMinLot,
		}
		v := watcher.Classify(state, exp)
		out, err := json.Marshal(map[string]interface{}{
			"state":          v.State.String(),
			"reason":         v.Reason,
			"live_size":      v.LiveSize,
			"remainder_txid": v.RemainderTxid,
			"remainder_vout": v.RemainderVout,
			"remainder_size": v.RemainderSize,
			"spender_txid":   v.SpenderTxid,
		})
		if err != nil {
			die("marshal: %v", err)
		}
		fmt.Println(string(out))
	case "run":
		die("run: the load-bearing watcher runs inside seqobd (start seqobd with -node-rpc); " +
			"a standalone run loop needs a relay admin book endpoint, not built here")
	default:
		die("unknown subcommand %q (want classify or run)", os.Args[1])
	}
}
