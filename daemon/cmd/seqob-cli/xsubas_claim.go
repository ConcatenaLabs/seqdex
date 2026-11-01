package main

// xsubas-claim-btc.go: the WALLET/device-side BTC claim for a sub-asset SELL. After
// POST /swap {side:sell} returns P + the BTC HTLC terms (the LSP paid the asset over LN
// on the user's node but did NOT claim — it holds no claim key), the wallet claims the
// maker's BTC HTLC on-chain itself with its DEVICE claim privkey. This is that step,
// runnable stand-alone from just (preimage, HTLC terms, claim privkey) — no relay, no LN.

import (
	"encoding/hex"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsClaimBTC(args []string) {
	fs := newFlagSet("xsubas-claim-btc")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet that RECEIVES the claimed BTC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	txid := fs.String("txid", "", "maker BTC HTLC funding txid (from /swap btc_htlc.txid) (required)")
	vout := fs.Uint("vout", 0, "maker BTC HTLC funding vout (btc_htlc.vout)")
	amount := fs.Uint64("amount", 0, "HTLC amount in sats (btc_htlc.amount) (required)")
	script := fs.String("redeem-script", "", "HTLC redeem script hex (btc_htlc.redeem_script) (required)")
	tBtc := fs.Uint("t-btc", 0, "HTLC refund locktime T_btc (btc_htlc.t_btc) (required)")
	refundPub := fs.String("refund-pub", "", "maker refund pubkey hex (btc_htlc.maker_refund_pubkey) (required)")
	preimage := fs.String("preimage", "", "the preimage P learned from /swap (hex) (required)")
	claimPriv := fs.String("claim-priv", "", "the DEVICE claim privkey (32-byte hex) matching btc_htlc.taker_claim_pubkey (required; never given to the LSP)")
	spendFee := fs.Uint64("spend-fee", 1000, "claim tx fee target (sats)")
	_ = fs.Parse(args)

	if *btcRPCURL == "" || *txid == "" || *amount == 0 || *script == "" || *tBtc == 0 || *refundPub == "" || *preimage == "" || *claimPriv == "" {
		fatal("xsubas-claim-btc requires -btc-rpc, -txid, -amount, -redeem-script, -t-btc, -refund-pub, -preimage, -claim-priv")
	}
	pre, err := hex.DecodeString(*preimage)
	if err != nil || len(pre) != 32 {
		fatal("-preimage must be 32-byte hex")
	}
	kb, err := hex.DecodeString(*claimPriv)
	if err != nil || len(kb) != 32 {
		fatal("-claim-priv must be 32-byte hex")
	}
	claimKey := xchain.KeyFromBytes(kb)
	scriptB, err := hex.DecodeString(*script)
	if err != nil {
		fatal("-redeem-script must be hex: %v", err)
	}
	refundB, err := hex.DecodeString(*refundPub)
	if err != nil || len(refundB) == 0 {
		fatal("-refund-pub must be hex")
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

	// Bind a hash-only swap to H = sha256(P), inject P, reconstruct + verify the HTLC leg,
	// then claim it with the device key. VerifyBTCLeg recomputes the script from H +
	// claim/refund pubkeys and matches it against the on-chain output (0 confs accepted).
	sw := xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashOf(pre)))
	if err := sw.InjectSecret(pre); err != nil {
		fatal("inject secret: %v", err)
	}
	verified, err := sw.VerifyBTCLeg(hashOf(pre), claimKey.PubKey(), refundB, scriptB,
		uint32(*tBtc), *txid, uint32(*vout), *amount, "", 0)
	if err != nil {
		fatal("verify HTLC (claim pub must match btc_htlc.taker_claim_pubkey): %v", err)
	}
	// Keep the fee below the HTLC value (leave at least dust for the claim output).
	fee := *spendFee
	if fee >= *amount {
		if *amount <= 300 {
			fatal("HTLC amount %d too small to cover a claim fee", *amount)
		}
		fee = *amount - 300
	}
	claimTxid, err := sw.ClaimBTCLeg(verified.Leg, claimKey, fee)
	if err != nil {
		fatal("claim HTLC (RETRYABLE, you hold P): %v", err)
	}
	fmt.Printf("CLAIMED %d BTC sats on-chain in %s (device-signed; the LSP never held this key)\n", *amount, claimTxid)
}

// hashOf returns sha256(preimage) as the hashlock H.
func hashOf(pre []byte) []byte {
	h := xchain.NewHashLock(pre)
	return h.Hash
}
