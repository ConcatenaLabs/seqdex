package main

// xsubas-fund-btc: the DEVICE side of a fully non-custodial sub-asset buy — build,
// sign, and broadcast the BTC on-chain HTLC from the USER's OWN Bitcoin, so the LSP
// never fronts the BTC. In the browser this is btc.js signing with the device key;
// this CLI reproduces it for headless/box proofs.
//
// It builds the Design-A HTLC (claim = the maker's stable claim pubkey with P;
// refund = a device refund key after T_btc) on hash H, funds it from -btc-wallet
// (the user's bitcoind wallet), broadcasts it, and prints the HTLC params the
// wallet passes to POST /swap PLUS the device refund PRIVATE key (kept device-side,
// used only to reclaim the HTLC at CLTV if the maker never pays).
//
//   seqob-cli xsubas-fund-btc -maker-claim-pub <hex> -hash <H hex> -btc-amount <sats>
//     -btc-locktime <T_btc> -btc-rpc <url> -btc-wallet <user wallet> -btc-chain testnet4
//     [-refund-priv <hex>]   (a device refund key is generated + printed if omitted)
//
// LSP payer-bridge (fund-safety) mode: pass -refund-pub <hex compressed pubkey> to
// embed the CALLER's OWN refund pubkey in the redeem script INSTEAD of minting an
// ephemeral one here. The LSP mints its refund keypair and PERSISTS the private key
// to durable storage BEFORE calling this CLI, so the funding broadcast can never run
// ahead of a persisted refund key — closing the crash window (mint-here → broadcast →
// crash-before-persist) that would otherwise strand a funded HTLC (fund-loss). No
// private key is minted or returned in this mode; the caller already holds it and
// refunds via xsubas-refund-btc using the returned on-chain facts.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
	"github.com/btcsuite/btcd/btcec/v2"
)

func cmdXSubAsFundBtc(args []string) {
	fs := newFlagSet("xsubas-fund-btc")
	makerClaimPubHex := fs.String("maker-claim-pub", "", "the maker's STABLE BTC claim pubkey (hex) from the offer's LightningTerms.maker_claim_pub (required)")
	hashHex := fs.String("hash", "", "the swap hash H (hex) = SHA256(device preimage); the HTLC is locked on it (required)")
	btcAmount := fs.Uint64("btc-amount", 0, "BTC sats to lock in the HTLC (required)")
	btcLocktime := fs.Uint("btc-locktime", 0, "T_btc: the CLTV height for the device refund branch (required; = btc_tip + offer OnchainCltv)")
	btcRPCURL := fs.String("btc-rpc", "", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet holding the USER's BTC that funds the HTLC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	refundPubHex := fs.String("refund-pub", "", "LSP payer-bridge: the CALLER's OWN refund pubkey (33-byte compressed hex) to embed in the redeem script; when set, NO refund key is minted or returned here (the caller persisted its private key BEFORE broadcast). Mutually exclusive with -refund-priv.")
	refundPrivHex := fs.String("refund-priv", "", "device BTC refund privkey (32-byte hex); a fresh one is generated + printed if empty (ignored when -refund-pub is set)")
	feeRate := fs.Float64("btc-fee-rate", 0, "the least sat/vB funding the HTLC pays; it pays the live going rate, never under this (0: the chain's floor)")
	locateOnly := fs.Bool("locate-only", false, "DRY-LOCATE (broadcast NOTHING): compute the deterministic Design-A redeem script from -maker-claim-pub/-hash/-btc-locktime/-refund-pub and scan the chain+wallet for an output ALREADY paying its P2SH. Prints {funded:true,...outpoint} if found, {funded:false,...} otherwise. Makes the LSP payer-bridge funding crash-IDEMPOTENT: scan-before-broadcast + recover-on-resume so a crash between a prior broadcast and the caller persisting the funding facts never double-funds. Requires -refund-pub (or -refund-priv to derive it) so the P2SH is deterministic.")
	_ = fs.Parse(args)

	if *makerClaimPubHex == "" || *hashHex == "" || *btcAmount == 0 || *btcLocktime == 0 || *btcRPCURL == "" {
		fatal("xsubas-fund-btc requires -maker-claim-pub, -hash, -btc-amount, -btc-locktime, -btc-rpc")
	}
	// Locate-only cannot mint a refund key: the P2SH it scans for must be the SAME
	// deterministic script a real fund would produce, which requires the caller's
	// OWN refund pubkey. Refuse rather than silently scan for a script nobody funded.
	if *locateOnly && *refundPubHex == "" && *refundPrivHex == "" {
		fatal("xsubas-fund-btc -locate-only requires -refund-pub (or -refund-priv) so the redeem script's P2SH is deterministic")
	}
	makerClaimPub, err := hex.DecodeString(*makerClaimPubHex)
	if err != nil || len(makerClaimPub) == 0 {
		fatal("bad -maker-claim-pub")
	}
	hashH, err := hex.DecodeString(*hashHex)
	if err != nil || len(hashH) != 32 {
		fatal("-hash must be 32-byte hex H")
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
	if *feeRate > 0 {
		btcChain.SetFeeRate(*feeRate)
	}
	// The refund key that guards the HTLC's CLTV/ELSE branch. Two modes:
	//
	//   (a) -refund-pub supplied (LSP payer-bridge, fund-safe): the CALLER already
	//       minted the refund keypair and PERSISTED its private key to durable storage
	//       BEFORE invoking this CLI. We embed the supplied pubkey in the redeem script
	//       and NEVER mint or return a private key — closing the crash window where an
	//       ephemeral refund key minted here and broadcast before the caller could
	//       persist it would strand the funded HTLC (fund-loss). The caller persists the
	//       returned on-chain facts (txid/vout/amount/redeem_script/locktime) and
	//       reclaims via xsubas-refund-btc with the key it already holds.
	//
	//   (b) -refund-pub absent (device self-custody, back-compat): mint (or load from
	//       -refund-priv) the refund key here and RETURN the private key so the device
	//       keeps it — only it can reclaim the HTLC at CLTV; the LSP never sees it.
	var (
		refundKey *xchain.Key // nil in mode (a): the caller holds the private key
		refundPub []byte      // the compressed pubkey embedded in the redeem script
	)
	switch {
	case *refundPubHex != "":
		if *refundPrivHex != "" {
			fatal("-refund-pub and -refund-priv are mutually exclusive: pass -refund-pub when the CALLER holds the refund private key, -refund-priv (or neither) for device self-custody")
		}
		pkb, derr := hex.DecodeString(*refundPubHex)
		if derr != nil {
			fatal("-refund-pub must be hex: %v", derr)
		}
		pk, derr := btcec.ParsePubKey(pkb)
		if derr != nil {
			fatal("-refund-pub is not a valid secp256k1 public key: %v", derr)
		}
		// Embed the exact 33-byte compressed encoding the redeemScript uses; a
		// mismatched (e.g. uncompressed) encoding would fund an HTLC whose CLTV branch
		// the caller's persisted key could not spend.
		refundPub = pk.SerializeCompressed()
	case *refundPrivHex != "":
		kb, derr := hex.DecodeString(*refundPrivHex)
		if derr != nil || len(kb) != 32 {
			fatal("-refund-priv must be 32-byte hex")
		}
		refundKey = xchain.KeyFromBytes(kb)
		refundPub = refundKey.PubKey()
	default:
		refundKey, err = xchain.NewKey()
		if err != nil {
			fatal("mint refund key: %v", err)
		}
		refundPub = refundKey.PubKey()
	}

	// LOCATE-ONLY (dry-locate, broadcast NOTHING): recompute the deterministic
	// Design-A redeem script from the caller's own params + refund pubkey, then scan
	// the chain + wallet for an output ALREADY paying its P2SH. This is the
	// scan-before-broadcast + recover-on-resume primitive that makes the LSP payer
	// bridge's BTC funding crash-IDEMPOTENT: a crash between a prior broadcast and the
	// caller persisting the funding facts is recovered here (the already-funded output
	// is found and adopted) instead of double-funding a second output to the SAME
	// deterministic P2SH (which would strand the first). Prints funded:true + the
	// outpoint if present, funded:false otherwise; exits 0 either way (a clean "not
	// funded yet" is not an error — the caller then broadcasts).
	if *locateOnly {
		leg := xchain.NewBitcoinLeg(xchain.NewHashLockFromHash(hashH), params)
		script, err := leg.HTLCScript(makerClaimPub, refundPub, uint32(*btcLocktime))
		if err != nil {
			fatal("locate-only: build redeem script: %v", err)
		}
		loc, definitive, err := btcChain.LocateFundedHTLC(script)
		// Fail CLOSED on uncertainty: a locate is only actionable when it DEFINITIVELY
		// answered. If any chain/wallet lookup errored (definitive=false), we cannot
		// rule out a prior self-broadcast funding, so we must NOT report not-funded
		// (which the caller would take as license to broadcast and could double-fund).
		// Exit non-zero with a RETRYABLE signal; nothing was broadcast, re-drive later.
		if !definitive {
			fatal("locate-only: UNCERTAIN whether the HTLC is funded — a chain/wallet lookup failed, failing closed (RETRYABLE; nothing broadcast): %v", err)
		}
		out := map[string]interface{}{
			"btc_htlc_script": hex.EncodeToString(script),
			"btc_locktime":    uint32(*btcLocktime),
			"btc_refund_pub":  hex.EncodeToString(refundPub),
			"hash_h":          hex.EncodeToString(hashH),
			"funded":          loc != nil,
		}
		if loc != nil {
			// Verify-not-trust the value before reporting funded: refuse to let the
			// caller adopt an output of the WRONG amount (fail closed on a mismatch;
			// the caller then treats it as not-yet-funded and can re-fund cleanly).
			if loc.Amount != *btcAmount {
				fatal("locate-only: found HTLC output %s:%d pays %d sats, expected %d — refusing to report a mismatched-amount HTLC as funded", loc.TxID, loc.Vout, loc.Amount, *btcAmount)
			}
			out["btc_htlc_txid"] = loc.TxID
			out["btc_htlc_vout"] = loc.Vout
			out["btc_htlc_amount"] = loc.Amount
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}

	// Build the HTLC on H (claim=maker with P, refund branch after T_btc) and fund it
	// from the funder's wallet (device- or LSP-signed). This is the on-chain payment;
	// the maker claims it later with the revealed preimage.
	swap := xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashH))
	leg, _, err := swap.LockBTCLeg(makerClaimPub, refundPub, atomsToCoinsCli(*btcAmount), uint32(*btcLocktime))
	if err != nil {
		fatal("fund BTC HTLC from the user's wallet: %v", err)
	}
	// The funded on-chain facts + the EXACT redeem script are always returned so the
	// caller can persist them and later locate/spend its own HTLC by script alone
	// (xsubas-refund-btc verify-not-trust) without re-deriving it.
	out := map[string]interface{}{
		"btc_htlc_txid":   leg.Funded.TxID,
		"btc_htlc_vout":   leg.Funded.Vout,
		"btc_htlc_amount": leg.Funded.Amount,
		"btc_htlc_script": hex.EncodeToString(leg.Script),
		"btc_locktime":    leg.Locktime,
		"btc_refund_pub":  hex.EncodeToString(refundPub),
		"hash_h":          hex.EncodeToString(hashH),
	}
	// Return the refund PRIVATE key ONLY in self-custody mode (b), where this CLI minted
	// it and the device must keep it. In the LSP payer-bridge mode (a) the caller already
	// persisted its own refund key before broadcast, so we never echo a secret it never
	// asked us to hold.
	if refundKey != nil {
		out["btc_refund_priv"] = hex.EncodeToString(refundKey.Bytes()) // DEVICE-HELD; never send to the LSP
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// atomsToCoinsCli renders sats as an 8-decimal coin string for the wallet-funding RPC.
func atomsToCoinsCli(a uint64) string { return fmt.Sprintf("%d.%08d", a/100_000_000, a%100_000_000) }
