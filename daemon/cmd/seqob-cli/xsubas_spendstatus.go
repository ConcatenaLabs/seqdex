package main

// xsubas-htlc-spend-status: the LSP payer leg-bridge's AUTHORITATIVE recoup-vs-refund
// ORACLE. Given the LSP-funded BTC HTLC outpoint (txid,vout) + its redeem script, it
// reports the ON-CHAIN fate of that output — UNSPENT, SPENT_VIA_CLAIM (returns the
// revealed preimage P), or SPENT_VIA_REFUND — by reading the chain, NOT a persisted
// intent flag.
//
// This is the structural fix for the round-6 race: the recoup/refund decision keyed on
// s.refunded (a persisted INTENT), which is racy across the persist/broadcast window
// (crash-before-broadcast => stale true; RPC-error-after-broadcast => stale false).
// The LSP's BTC HTLC has EXACTLY ONE spend: the CLAIM branch (spender reveals P => the
// maker got paid => the LSP MUST recoup the taker's hold) or the REFUND/CLTV branch
// (the LSP reclaimed its BTC => the maker is unpaid => the hold MUST be released to the
// taker, never captured). This command reads which one happened, idempotently.
//
// INVARIANT the LSP enforces off the result:
//   SPENT_VIA_CLAIM  => recoup the taker's hold (settle with the returned preimage).
//   SPENT_VIA_REFUND => release the taker's hold (never capture).
//   UNSPENT          => decide by P-public / T_btc as before (no spend yet).
//
// UNCERTAIN handling (fail closed): if ANY chain/wallet lookup errored or a spend was
// seen but could not be classified, this exits NON-ZERO and prints nothing actionable.
// The caller treats a non-zero exit as RETRYABLE and re-drives — it must NEVER treat
// uncertainty as a fate (a stale "unspent"/"refund" could release a hold the maker
// already claimed against, or a stale "claim" could capture a hold the maker never got).
//
// NODE REQUIREMENT: reading a confirmed NON-wallet maker CLAIM (the sole on-chain source of
// P when the maker wins the T_btc race) needs txindex + an UNPRUNED node. The LSP's OWN
// refund is a wallet tx and readable on any node; the maker's claim is not. A one-time
// startup check WARNs (or, with -require-node-caps, errors) when the node lacks either — a
// misconfigured node still fails closed (RETRYABLE) rather than mis-settling, but can never
// recoup a maker-claim front until fixed.
//
//   seqob-cli xsubas-htlc-spend-status -btc-rpc <url> [-btc-wallet <w>] -btc-chain testnet4
//     -txid <funding txid> -vout <n> -redeem-script <hex> [-require-node-caps]

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsHtlcSpendStatus(args []string) {
	fs := newFlagSet("xsubas-htlc-spend-status")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet (the LSP's OWN; makes its own refund spend findable wallet-aware even on a pruned / non-txindex node)")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	txid := fs.String("txid", "", "the LSP-funded BTC HTLC funding txid (required)")
	vout := fs.Uint("vout", 0, "the LSP-funded BTC HTLC funding vout")
	script := fs.String("redeem-script", "", "HTLC redeem script hex (btc_htlc.redeem_script) — H is read from it (required)")
	requireNodeCaps := fs.Bool("require-node-caps", false, "fail hard (non-zero exit) if the BTC node lacks txindex/unpruned; default WARNs to stderr and proceeds (classification still fails closed on an unreadable non-wallet claim)")
	_ = fs.Parse(args)

	if *btcRPCURL == "" || *txid == "" || *script == "" {
		fatal("xsubas-htlc-spend-status requires -btc-rpc, -txid, -redeem-script")
	}
	scriptB, err := hex.DecodeString(*script)
	if err != nil || len(scriptB) == 0 {
		fatal("-redeem-script must be hex: %v", err)
	}
	btcRPC, err := xliftRPCFromURL(*btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(*btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)

	// One-time startup capability check: the classifier can read the LSP's OWN refund on ANY
	// node (it is a wallet tx), but a confirmed NON-wallet maker CLAIM — the sole on-chain
	// source of P when the maker wins the T_btc race — is readable only with txindex + an
	// unpruned node. Assert that up front so a misconfigured node is caught loudly instead of
	// silently stranding the front (classification would fail closed forever). Default: WARN
	// and proceed (a genuine LSP-own-refund fate still classifies fine, and an unreadable
	// claim already fails closed); -require-node-caps makes it a hard, non-zero exit.
	if _, capErr := btcChain.CheckClassifierNodeCapabilities(); capErr != nil {
		if *requireNodeCaps {
			fatal("xsubas-htlc-spend-status: %v (—require-node-caps set)", capErr)
		}
		fmt.Fprintf(os.Stderr, "WARNING: %v — a confirmed non-wallet maker claim may be unreadable here; classification will fail closed (RETRYABLE) rather than mis-settle, but fix the node (txindex=1, prune=0) to recoup such fronts\n", capErr)
	}

	status, preimage, spenderConfs, definitive, err := btcChain.ClassifyBTCHTLCSpend(*txid, uint32(*vout), scriptB)
	// Fail CLOSED on uncertainty: the recoup-vs-refund decision is IRREVERSIBLE, so an
	// UNCERTAIN fate must never be reported as a definitive status. Exit non-zero with a
	// RETRYABLE signal; nothing was moved, re-drive later.
	if !definitive {
		fatal("xsubas-htlc-spend-status: UNCERTAIN HTLC fate — a chain/wallet lookup failed or a spend was unclassifiable, failing closed (RETRYABLE; nothing moved): %v", err)
	}
	out := map[string]interface{}{
		"txid":   *txid,
		"vout":   uint32(*vout),
		"status": status.String(),
		// The BURIAL DEPTH of the spender (0 when UNSPENT, or the spender's confirmation
		// count when spent). The LSP payer bridge keys the "a refund is terminal only once
		// BURIED beyond a reorg margin" rule on this — a 0-conf refund can be replaced by a
		// conflicting maker claim (RBF/reorg), so the LSP keeps watching until it buries. A
		// CLAIM is acted on at any depth, so its confs are informational only.
		"spender_confirmations": spenderConfs,
	}
	if status == xchain.BTCHTLCSpendClaim {
		// The revealed preimage the LSP settles the taker's hold with (recoup). It is
		// present ONLY on a claim; a refund/unspent result carries none.
		out["preimage"] = hex.EncodeToString(preimage)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
