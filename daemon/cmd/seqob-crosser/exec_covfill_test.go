package main

// exec_covfill_test.go — the covenant leg's shell-out contract: legCommand must
// build the exact `seqob-cli covfill` invocation, size -amount in the covenant's
// SOLD-asset (asset A) atoms on both orientations, and carry no refund state
// (a covenant fill is one atomic tx).

import (
	"strings"
	"testing"
)

func covExecutor() *Executor {
	return &Executor{
		Caps: Caps{
			SeqRPC: "http://u:p@h:1", SeqWallet: "w",
			SeqobCli: "seqob-cli", StateDir: "/tmp/x", SpendFee: 2000,
		},
		Logf: noLog,
	}
}

func argsMap(t *testing.T, args []string) (cmd string, flags map[string]string) {
	t.Helper()
	if len(args) == 0 {
		t.Fatal("empty args")
	}
	flags = map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		if !strings.HasPrefix(args[i], "-") {
			t.Fatalf("expected flag at %d, got %q (args %v)", i, args[i], args)
		}
		flags[args[i]] = args[i+1]
	}
	return args[0], flags
}

// TestCovenantLegCommandAsk: an ask sells asset A as its own base, so the take
// passes through unchanged; a whole-offer take uses the 0=whole convention so
// covfill fills the authoritative on-chain locked amount.
func TestCovenantLegCommandAsk(t *testing.T) {
	e := covExecutor()
	n := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 4000), tBase, tQuote)

	args, state, resume, err := e.legCommand(n, 40) // partial slice
	if err != nil {
		t.Fatalf("legCommand: %v", err)
	}
	cmd, flags := argsMap(t, args)
	if cmd != "covfill" {
		t.Fatalf("cmd = %q, want covfill", cmd)
	}
	if flags["-amount"] != "40" {
		t.Fatalf("-amount = %s, want 40 (asset-A atoms == canonical base on an ask)", flags["-amount"])
	}
	if flags["-base"] != tBase || flags["-quote"] != tQuote {
		t.Fatalf("pair flags wrong: %v", flags)
	}
	if _, onCmdLine := flags["-seq-rpc"]; onCmdLine || flags["-seq-wallet"] != e.Caps.SeqWallet {
		t.Fatalf("seq node flags wrong (the node URL travels in the environment): %v", flags)
	}
	if !hasEnv(e.Caps.childEnv(), "SEQOB_SEQ_RPC="+e.Caps.SeqRPC) {
		t.Fatalf("child env lacks SEQOB_SEQ_RPC")
	}
	if flags["-offer-id"] != n.Offer.GetOfferId() || flags["-maker-pubkey"] != n.Offer.GetMakerPubkey() {
		t.Fatalf("offer identity flags wrong: %v", flags)
	}
	if state != "" {
		t.Fatalf("covenant leg minted a state file (%s); a single-tx fill has no refund state", state)
	}
	if !strings.Contains(resume, "atomic") {
		t.Fatalf("resume = %q, want the informational single-tx note", resume)
	}

	// Whole-offer take: the 0=whole convention.
	args, _, _, err = e.legCommand(n, n.BaseSize)
	if err != nil {
		t.Fatalf("legCommand whole: %v", err)
	}
	_, flags = argsMap(t, args)
	if flags["-amount"] != "0" {
		t.Fatalf("whole-offer -amount = %s, want 0", flags["-amount"])
	}
}

// TestCovenantLegCommandBid: a (non-flipped) bid rests asset A as our QUOTE, so
// the canonical-base take converts to asset-A atoms at the offer's own price,
// floored — exactly what RevenueFor books as this leg's proceeds.
func TestCovenantLegCommandBid(t *testing.T) {
	e := covExecutor()
	// Maker BUYS 100 base with 4000 quote: the covenant locks 4000 of asset A
	// (our quote). Selling 40 base into it should claim floor(40*4000/100) = 1600.
	n := norm(mkOffer(FamCovenant, SideBid, tBase, tQuote, 100, 4000), tBase, tQuote)

	args, _, _, err := e.legCommand(n, 40)
	if err != nil {
		t.Fatalf("legCommand: %v", err)
	}
	_, flags := argsMap(t, args)
	if flags["-amount"] != "1600" {
		t.Fatalf("-amount = %s, want 1600 (floor(40*4000/100) asset-A atoms)", flags["-amount"])
	}

	// Whole-bid take still maps to 0=whole.
	args, _, _, err = e.legCommand(n, n.BaseSize)
	if err != nil {
		t.Fatalf("legCommand whole: %v", err)
	}
	_, flags = argsMap(t, args)
	if flags["-amount"] != "0" {
		t.Fatalf("whole-bid -amount = %s, want 0", flags["-amount"])
	}
}
