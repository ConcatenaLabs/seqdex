package main

// xsubas-refund-btc.go: the LSP-side REFUND of the payer-direction leg-bridge's
// on-chain BTC HTLC. In the forward (payer) bridge the LSP FUNDS a BTC HTLC to
// the maker — claim = maker-with-P, refund = LSP after T_btc. If the maker never
// claims before T_btc (a no-reveal), the LSP reclaims that BTC by spending the
// REFUND (ELSE / CLTV) branch back to its OWN wallet with its refund key.
//
// This is the exact MIRROR of xsubas-claim-btc (which spends the claim / IF
// branch by revealing a preimage): both compose the SAME xchain HTLC-spend
// primitives, this one via xchain.RefundBTCLeg. It takes the same param
// interface as the claim CLI (-btc-rpc -btc-wallet -btc-chain -txid -vout
// -amount -redeem-script -t-btc), swapping the claim's -preimage/-claim-priv/
// -refund-pub for a single -refund-priv (the refund branch needs only the
// funder's own signature after CLTV). Runnable stand-alone from (HTLC terms,
// refund privkey) — no relay, no LN. The LSP's refundBridgeHtlcBtc shells to
// exactly this (see tooling/lsp/lsp-server.mjs).
//
// The refund fee is caller-controlled for the LSP's RBF fee-bumping: -fee (absolute
// sats) or -feerate (sat/vB, sized from the actual refund-tx vsize) or the back-compat
// -spend-fee default. The builder emits an RBF-signalling refund (BuildRefundTx sets
// sequence 0xfffffffd, BIP125), so re-running with a higher fee REPLACES a stalled
// 0-conf refund — it must confirm inside the taker hold's remaining life.

import (
	"encoding/hex"
	"fmt"
	"math"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsRefundBTC(args []string) {
	fs := newFlagSet("xsubas-refund-btc")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet that RECEIVES the reclaimed BTC (the LSP's own recoup wallet)")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	txid := fs.String("txid", "", "the LSP-funded BTC HTLC funding txid (required)")
	vout := fs.Uint("vout", 0, "the LSP-funded BTC HTLC funding vout")
	amount := fs.Uint64("amount", 0, "HTLC amount in sats (required)")
	script := fs.String("redeem-script", "", "HTLC redeem script hex (btc_htlc.redeem_script) (required)")
	tBtc := fs.Uint("t-btc", 0, "HTLC refund locktime T_btc — the CLTV height the refund branch matures at (required)")
	refundPriv := fs.String("refund-priv", "", "the LSP refund privkey (32-byte hex) matching the HTLC's refund/ELSE-branch pubkey (required; the LSP's own key)")
	// Fee control for the LSP's RBF fee-bumping (three ways, most-explicit wins). The
	// refund tx the builder produces is RBF-signalling (sequence 0xfffffffd, BIP125),
	// so the LSP can re-run this CLI with a HIGHER fee to REPLACE a stalled 0-conf
	// refund — it must confirm inside the taker hold's remaining life or a maker claim
	// takes the LSP's BTC HTLC (front loss). Precedence: -fee > -feerate > -spend-fee.
	spendFee := fs.Uint64("spend-fee", 1000, "refund tx fee in sats (absolute) — the back-compat default; used only when neither -fee nor -feerate is given")
	absFee := fs.Uint64("fee", 0, "refund tx fee in sats (absolute); overrides -spend-fee and -feerate (0 => unset)")
	feeRate := fs.Float64("feerate", 0, "refund tx fee rate in sat/vB; sizes the absolute fee from the ACTUAL refund-tx vsize (for RBF bumps); overrides -spend-fee, overridden by -fee (0 => unset)")
	_ = fs.Parse(args)

	if *btcRPCURL == "" || *txid == "" || *amount == 0 || *script == "" || *tBtc == 0 || *refundPriv == "" {
		fatal("xsubas-refund-btc requires -btc-rpc, -txid, -amount, -redeem-script, -t-btc, -refund-priv")
	}
	kb, err := hex.DecodeString(*refundPriv)
	if err != nil || len(kb) != 32 {
		fatal("-refund-priv must be 32-byte hex")
	}
	refundKey := xchain.KeyFromBytes(kb)
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
	if _, err := btcChain.BlockCount(); err != nil {
		fatal("bitcoind unreachable: %v", err)
	}

	// Verify-not-trust BEFORE spending: fetch the funding tx and confirm the EXACT
	// redeem script we hold is genuinely funded at (txid, vout) with `amount`. The
	// refund path already possesses the redeemScript it funded, so — unlike the
	// claim path, which reconstructs the script from H + the counterparty pubkey —
	// it locates its own HTLC output by that script alone. A mismatch fails CLOSED
	// (nothing is broadcast): the HTLC stays put and the refund is retryable, never
	// a half-settle.
	rawHex, _, err := btcChain.RawTxAndConfirmations(*txid)
	if err != nil {
		fatal("fetch funding tx %s (RETRYABLE, the HTLC is untouched): %v", *txid, err)
	}
	foundVout, foundVal, err := xchain.LocateHTLCOutputByScript(params, rawHex, scriptB)
	if err != nil {
		fatal("locate HTLC output by redeem script (refusing to refund a script tx %s does not fund): %v", *txid, err)
	}
	if foundVout != uint32(*vout) {
		fatal("HTLC output at vout %d, caller passed -vout %d — refusing to spend the wrong outpoint", foundVout, *vout)
	}
	if foundVal != *amount {
		fatal("HTLC output value %d != -amount %d — refusing to refund on a mismatched amount", foundVal, *amount)
	}

	// Resolve the refund fee (sats) from the three fee flags, most-explicit first.
	// -fee is an absolute override; -feerate sizes the fee from the ACTUAL refund-tx
	// vsize; otherwise the back-compat -spend-fee (default 1000). RefundBTCLeg only
	// takes an absolute fee, so -feerate is converted HERE: build the exact refund
	// spend once WITHOUT broadcasting (the same zero-hash HashLock prim + redeemScript
	// + refund key the real RefundBTCLeg uses, so the probe is byte-identical in shape)
	// and measure its serialized size. The BTC refund is a legacy P2SH spend (no
	// witness), so vsize == byte size = len(hex)/2. Round the fee UP so a rounding
	// error never UNDER-pays — an under-fee refund can stall past the hold and cost the
	// LSP its BTC HTLC. The probe consumes one fresh recoup-wallet address (harmless,
	// the LSP's own wallet) and is reached only when -feerate is set.
	var fee uint64
	switch {
	case *absFee > 0:
		fee = *absFee
	case *feeRate > 0:
		dest, derr := btcChain.NewDestScript()
		if derr != nil {
			fatal("size refund fee from -feerate (RETRYABLE, the HTLC is untouched): dest script: %v", derr)
		}
		probeLeg := xchain.NewBitcoinLeg(xchain.NewHashLockFromHash(make([]byte, 32)), params)
		probeHex, berr := probeLeg.BuildRefundTx(scriptB, xchain.BitcoinSpendInput{
			TxID: *txid, Vout: foundVout, Amount: foundVal, DestPK: dest, Fee: 0,
		}, uint32(*tBtc), refundKey)
		if berr != nil {
			fatal("size refund fee from -feerate (RETRYABLE, the HTLC is untouched): build probe: %v", berr)
		}
		vsize := len(probeHex) / 2
		f := math.Ceil(*feeRate * float64(vsize))
		if f < 1 {
			f = 1 // never a zero-fee refund
		}
		fee = uint64(f)
	default:
		fee = *spendFee
	}

	// Keep the fee below the HTLC value (leave at least dust for the refund output).
	if fee >= *amount {
		if *amount <= 300 {
			fatal("HTLC amount %d too small to cover a refund fee", *amount)
		}
		fee = *amount - 300
	}

	// Reconstruct the funded leg from the VERIFIED on-chain outpoint + the provided
	// script/locktime and spend the REFUND (ELSE / CLTV) branch back to the LSP's
	// own wallet at nLockTime = T_btc. RefundBTCLeg enforces the CLTV + the
	// refund-key signature at broadcast: a wrong key or a not-yet-matured T_btc is
	// REJECTED by the node (fail closed, retryable), never a half-settle. The
	// hashlock is unused on the refund branch, so a zero-hash primitive suffices.
	leg := &xchain.LegLock{
		Script:   scriptB,
		Funded:   &xchain.FundedHTLC{TxID: *txid, Vout: foundVout, Amount: foundVal},
		Locktime: uint32(*tBtc),
	}
	sw := xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(make([]byte, 32)))
	refundTxid, err := sw.RefundBTCLeg(leg, refundKey, uint32(*tBtc), fee)
	if err != nil {
		fatal("refund HTLC (RETRYABLE once T_btc %d matures; the HTLC is untouched on failure): %v", *tBtc, err)
	}
	fmt.Printf("REFUNDED %d BTC sats on-chain in %s (LSP recoup via the CLTV/ELSE branch at T_btc=%d)\n", *amount, refundTxid, *tBtc)
}
