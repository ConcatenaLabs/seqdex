package main

// xfund-seq: the TAKER side of a rail-crossing bridged SELL — fund the Sequentia-asset
// on-chain HTLC self-custody, so the LSP bridge never holds the taker's asset or its keys.
// It mirrors xsubas-fund-btc (which funds the BTC leg on-chain from the user's own wallet),
// but for the SEQ/Elements leg via the audited xchain.LockSEQLeg: it builds the Design-A
// HTLC (claim = the MAKER's asset claim pubkey with P; refund = the taker's OWN key after
// T_seq) on hash H, funds it from -seq-wallet, waits for it to confirm, and prints the funded
// outpoint + redeem script the LSP relays to the maker (XcSeqLegFunded) so the maker claims
// it (revealing P). The taker keeps -refund-priv (only it can reclaim the asset at T_seq if the
// maker never claims) — it is the SAME key whose pubkey the taker gave the LSP as
// taker_seq_refund_pub, so the maker's re-derived redeem script matches byte-for-byte.
//
//   seqob-cli xfund-seq -asset <hex> -maker-claim-pub <hex> -hash <H hex> -seq-amount <atoms>
//     -seq-locktime <T_seq> -seq-rpc <url> -seq-wallet <w> -refund-priv <hex>

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXFundSeq(args []string) {
	fs := newFlagSet("xfund-seq")
	asset := fs.String("asset", "", "Sequentia asset id (hex) being sold (required)")
	makerClaimPubHex := fs.String("maker-claim-pub", "", "the MAKER's asset claim pubkey (hex) from the bridge handshake (maker_seq_claim_pub) (required)")
	hashHex := fs.String("hash", "", "the swap hash H (hex) = the maker's H from the handshake; the HTLC is locked on it (required)")
	seqAmount := fs.Uint64("seq-amount", 0, "asset atoms to lock in the HTLC (the offer's base/want amount) (required)")
	seqLocktime := fs.Uint("seq-locktime", 0, "T_seq: the CLTV height for the taker refund branch (from the handshake) (required)")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet holding the asset that funds the HTLC (required)")
	refundPrivHex := fs.String("refund-priv", "", "the taker's OWN asset refund privkey (32-byte hex); its pubkey MUST equal the taker_seq_refund_pub given to the LSP (required)")
	noWait := fs.Bool("no-wait", false, "return as soon as the HTLC is broadcast (0-conf, empty block_hash) so the caller relays the leg to the maker immediately, before the LSP's courier session idles through a SEQ block; the maker polls VerifySEQLeg until it confirms")
	_ = fs.Parse(args)

	if *asset == "" || *makerClaimPubHex == "" || *hashHex == "" || *seqAmount == 0 || *seqLocktime == 0 || *seqRPCURL == "" || *seqWallet == "" || *refundPrivHex == "" {
		fatal("xfund-seq requires -asset, -maker-claim-pub, -hash, -seq-amount, -seq-locktime, -seq-rpc, -seq-wallet, -refund-priv")
	}
	makerClaimPub, err := hex.DecodeString(*makerClaimPubHex)
	if err != nil || len(makerClaimPub) == 0 {
		fatal("bad -maker-claim-pub")
	}
	hashH, err := hex.DecodeString(*hashHex)
	if err != nil || len(hashH) != 32 {
		fatal("-hash must be 32-byte hex H")
	}
	kb, err := hex.DecodeString(*refundPrivHex)
	if err != nil || len(kb) != 32 {
		fatal("-refund-priv must be 32-byte hex")
	}
	refundKey := xchain.KeyFromBytes(kb)

	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, *seqWallet)
	// NewSwapBitcoin builds a Bitcoin backend eagerly (it needs chain params), but LockSEQLeg only ever
	// touches the SEQ side — so a params-only dummy BTC chain (never dialed) lets us fund the SEQ leg
	// without a bitcoind. testnet4 params match the live parent chain.
	params, err := xchain.BitcoinChainParams("testnet4")
	if err != nil {
		fatal("chain params: %v", err)
	}
	dummyBtc := xchain.NewBitcoinChain(xchain.NewRPC("127.0.0.1", 1, "x", "x"), "", params)

	// Build the HTLC on H (claim=maker with P, refund=taker after T_seq) and fund it from the taker's own
	// asset wallet. LockSEQLeg re-derives the redeem script exactly as the maker will, waits for the funding
	// tx to confirm in a Sequentia block, and returns the funded leg. A slow-confirm returns the leg WITH an
	// error so we can still print the refundable outpoint.
	swap := xchain.NewSwapBitcoin(dummyBtc, seqChain, xchain.NewHashLockFromHash(hashH))
	var leg *xchain.LegLock
	var blockHash string
	if *noWait {
		// 0-conf: broadcast + return immediately so the caller relays the funded leg to the maker BEFORE the
		// LSP's courier session idles out through a SEQ block (the maker polls VerifySEQLeg until it confirms).
		leg, err = swap.LockSEQLegNoWait(makerClaimPub, refundKey.PubKey(), atomsToCoinsCli(*seqAmount), *asset, uint32(*seqLocktime))
	} else {
		leg, blockHash, err = swap.LockSEQLeg(makerClaimPub, refundKey.PubKey(), atomsToCoinsCli(*seqAmount), *asset, uint32(*seqLocktime))
	}
	if leg == nil {
		fatal("fund SEQ HTLC from the taker's wallet: %v", err)
	}
	out := map[string]interface{}{
		"seq_htlc_txid": leg.Funded.TxID,
		"seq_htlc_vout": leg.Funded.Vout,
		"amount":        leg.Funded.Amount,
		"redeem_script": hex.EncodeToString(leg.Script),
		"seq_locktime":  leg.Locktime,
		"asset":         *asset,
		"refund_pub":    hex.EncodeToString(refundKey.PubKey()),
		"block_hash":    blockHash,
		"hash_h":        hex.EncodeToString(hashH),
	}
	if err != nil {
		out["confirm_warning"] = err.Error() // funded but slow to confirm; still refundable after T_seq
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
