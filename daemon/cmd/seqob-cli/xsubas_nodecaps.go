package main

// xsubas-node-caps: the LSP payer leg-bridge's BRING-UP capability gate. It runs
// xchain.CheckClassifierNodeCapabilities against the LSP's OWN bitcoind and reports
// whether that node can locate + read a NON-wallet maker CLAIM confirmed in an older
// block — the SOLE on-chain source of the preimage P when the maker wins the T_btc
// race. That read REQUIRES an UNPRUNED node with a SYNCED txindex; without it the
// HTLC-spend classifier fails CLOSED (UNCERTAIN) forever and the LSP can NEVER recoup
// a maker-claim front (full front loss risk).
//
// Unlike xsubas-htlc-spend-status (which only WARNs on a per-call basis unless
// -require-node-caps), this is a standalone, HTLC-independent probe the LSP calls
// ONCE at bring-up (and again per payer-bridge origination) to REFUSE payer bridges
// on a misconfigured node instead of silently stranding fronts. Exit code:
//   0 => capable (unpruned + synced txindex). JSON on stdout with pruned/txindex flags.
//   1 => NOT capable (pruned OR missing/unsynced txindex) — the reason names which.
//   2 => usage / node unreachable.
//
//   seqob-cli xsubas-node-caps -btc-rpc <url> [-btc-wallet <w>] -btc-chain testnet4

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsNodeCaps(args []string) {
	fs := newFlagSet("xsubas-node-caps")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet (optional; the capability check is node-level, not wallet-level)")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	_ = fs.Parse(args)

	if *btcRPCURL == "" {
		fatal("xsubas-node-caps requires -btc-rpc")
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

	caps, capErr := btcChain.CheckClassifierNodeCapabilities()
	out := map[string]interface{}{
		"pruned":         caps.Pruned,
		"txindex_known":  caps.TxindexKnown,
		"txindex_synced": caps.TxindexSynced,
		"ok":             caps.OK(),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if capErr != nil || !caps.OK() {
		// NOT capable: print the JSON (so the caller can log the specifics) AND a reason to
		// stderr, then exit 1. The LSP treats a non-zero exit as "refuse payer bridges".
		fmt.Println(string(b))
		if capErr != nil {
			fmt.Fprintf(os.Stderr, "xsubas-node-caps: node NOT capable for payer-bridge recoup: %v\n", capErr)
		} else {
			fmt.Fprintln(os.Stderr, "xsubas-node-caps: node NOT capable for payer-bridge recoup (pruned or txindex missing/unsynced)")
		}
		os.Exit(1)
	}
	fmt.Println(string(b))
}
