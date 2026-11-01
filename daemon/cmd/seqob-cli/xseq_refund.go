package main

// xseq-refund.go: reclaim an on-chain SEQUENTIA asset HTLC by its OUTPOINT, with
// no session state file.
//
// xrefund-seq already recovers an asset leg, but only the one an xsell session
// wrote to disk. The LSP's bridged-rail ASSET FRONTING funds an asset HTLC of its
// own (see xfund-seq, and tooling/lsp/lsp-server.mjs frontAssetLeg) so a taker
// paying BTC over Lightning receives the asset IMMEDIATELY instead of waiting for
// the maker's anchor gate. That HTLC is tracked in the LSP's job store, not in an
// xsell state file — so without an outpoint-addressable refund, a taker who never
// claims would leave the LSP's fronted inventory locked with no tool able to
// reclaim it after T_seq. Fronting must never be able to strand its own capital,
// so this is the mirror of xsubas-refund-btc for the asset side.
//
// Same shape as that mirror: (-txid -vout -amount -asset -redeem-script -t-seq
// -refund-priv), spending the REFUND / CLTV branch back to the caller's own
// wallet with the funder's key. The refund branch needs only that signature once
// T_seq has matured. VERIFY-NOT-TRUST before spending, and fail CLOSED on any
// mismatch: the HTLC is left untouched and the refund stays retryable.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSeqRefund(args []string) {
	fs := newFlagSet("xseq-refund")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet that RECEIVES the reclaimed asset (the funder's own wallet)")
	txid := fs.String("txid", "", "the asset HTLC funding txid (required)")
	vout := fs.Uint("vout", 0, "the asset HTLC funding vout")
	amount := fs.Uint64("amount", 0, "HTLC amount in asset atoms (required)")
	asset := fs.String("asset", "", "Sequentia asset id (hex) locked in the HTLC (required)")
	script := fs.String("redeem-script", "", "HTLC redeem script hex (required)")
	tSeq := fs.Uint("t-seq", 0, "HTLC refund locktime T_seq — the CLTV height the refund branch matures at (required)")
	refundPriv := fs.String("refund-priv", "", "the funder's refund privkey (32-byte hex) matching the HTLC's refund/ELSE-branch pubkey (required)")
	spendFee := fs.Uint64("spend-fee", 1000, "refund tx fee target in native sats")
	wait := fs.Bool("wait", false, "poll until T_seq passes instead of failing when early")
	_ = fs.Parse(args)

	if *seqRPCURL == "" || *txid == "" || *amount == 0 || *asset == "" || *script == "" || *tSeq == 0 || *refundPriv == "" {
		fatal("xseq-refund requires -seq-rpc, -txid, -amount, -asset, -redeem-script, -t-seq, -refund-priv")
	}
	kb, err := hex.DecodeString(*refundPriv)
	if err != nil || len(kb) != 32 {
		fatal("-refund-priv must be 32-byte hex")
	}
	scriptB, err := hex.DecodeString(*script)
	if err != nil || len(scriptB) == 0 {
		fatal("-redeem-script must be hex: %v", err)
	}

	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, *seqWallet)
	if _, err := seqChain.BlockCount(); err != nil {
		fatal("Sequentia node unreachable: %v", err)
	}

	leg := &xchain.LegLock{
		Script: scriptB,
		Funded: &xchain.FundedHTLC{
			TxID:    *txid,
			Vout:    uint32(*vout),
			Amount:  *amount,
			AssetID: *asset,
		},
		Locktime: uint32(*tSeq),
	}
	// The refund path only touches the Sequentia side, but NewSwapBitcoin builds its
	// BTC backend eagerly and dereferences the chain's params, so a nil BTC chain
	// segfaults at construction rather than staying unused. Hand it the same inert
	// stand-in xfund-seq uses: never dialled, only its params are read. The zero hash
	// is likewise inert — the refund branch is a CLTV+signature spend and never
	// evaluates the hashlock.
	btcParams, err := xchain.BitcoinChainParams("testnet4")
	if err != nil {
		fatal("chain params: %v", err)
	}
	dummyBtc := xchain.NewBitcoinChain(xchain.NewRPC("127.0.0.1", 1, "x", "x"), "", btcParams)
	ops := &client.LiveXcOps{
		Swap: xchain.NewSwapBitcoin(dummyBtc, seqChain, xchain.NewHashLockFromHash(make([]byte, 32))),
		SEQ:  seqChain,
	}
	refundTxid, err := client.RefundTakerSEQ(ops, leg, xchain.KeyFromBytes(kb), uint32(*tSeq), *asset, *spendFee, *wait, 15*time.Second)
	if err != nil {
		if errors.Is(err, client.ErrXcRefundNotDue) {
			fatal("%v (re-run with -wait to poll until due)", err)
		}
		fatal("refund: %v", err)
	}
	fmt.Printf("{\"ok\":true,\"refund_txid\":%q,\"asset\":%q,\"amount\":%d}\n", refundTxid, *asset, *amount)
}
