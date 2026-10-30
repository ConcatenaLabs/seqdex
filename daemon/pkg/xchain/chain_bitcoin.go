package xchain

import (
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// BitcoinChain is the maker's view of the REAL Bitcoin parent (bitcoind regtest
// or testnet4). It is the Bitcoin-format analog of Chain (chain.go, which is
// Elements-format): it talks to a genuine bitcoind over the same JSON-RPC
// plumbing (RPC) but parses Bitcoin transactions — 8-byte satoshi values, no
// asset commitments — and derives the HTLC P2SH address itself via btcd rather
// than via Elements decodescript.
//
// Only the calls the maker's BTC leg needs are implemented: read the raw
// funding tx + its confirmations (to VERIFY the taker's HTLC), broadcast the
// claim, learn the destination scriptPubKey, the height, and detect the
// taker's SEQ-side claim is NOT here — that stays on the SEQ (Elements) Chain.
type BitcoinChain struct {
	rpc    *RPC
	params *chaincfg.Params
	// feeRateSatVb, when > 0, is the least sat/vB any of its payments pays: an
	// HTLC funding, claim or refund pays the going rate (GoingRate), never below
	// it. 0 sets no minimum beyond the fee policy's floor.
	feeRateSatVb float64
	// fees is what the going rate is read from (DefaultFeePolicy for the chain).
	fees FeePolicy
}

// SetFeeRate sets the least sat/vB an HTLC funding, claim or refund pays.
func (c *BitcoinChain) SetFeeRate(satVb float64) { c.feeRateSatVb = satVb }

// FeeRate returns the configured minimum fee rate (sat/vB), 0 if unset.
func (c *BitcoinChain) FeeRate() float64 { return c.feeRateSatVb }

// NewBitcoinChain binds a BitcoinChain to a bitcoind RPC + wallet and chain
// params (regtest/testnet4). The wallet is used for getnewaddress
// (claim destination) and, on regtest, generatetoaddress/sendtoaddress in the
// verification harness; HTLC verification/claim themselves are wallet-agnostic.
func NewBitcoinChain(rpc *RPC, wallet string, params *chaincfg.Params) *BitcoinChain {
	return &BitcoinChain{rpc: rpc.WithWallet(wallet), params: params, fees: DefaultFeePolicy(params)}
}

// RPC exposes the underlying wallet-scoped client (for the harness / ad-hoc).
func (c *BitcoinChain) RPC() *RPC { return c.rpc }

// Params returns the chain params this BTC leg uses.
func (c *BitcoinChain) Params() *chaincfg.Params { return c.params }

// BlockCount returns the current Bitcoin chain height.
func (c *BitcoinChain) BlockCount() (int64, error) {
	var n int64
	return n, c.rpc.Call(&n, "getblockcount")
}

// RawTx fetches a transaction's raw (Bitcoin-serialized) hex via
// getrawtransaction <txid> false. Works for any tx the node has indexed
// (txindex) or that is in a wallet/mempool — including the taker-funded HTLC the
// maker did not create.
func (c *BitcoinChain) RawTx(txid string) (string, error) {
	var raw string
	if err := c.rpc.Call(&raw, "getrawtransaction", txid, false); err != nil {
		return "", err
	}
	return raw, nil
}

// TxConfirmations returns a tx's confirmation count via the verbose
// getrawtransaction (Bitcoin returns "confirmations" only when the tx is in a
// block or watched; 0/absent => unconfirmed).
func (c *BitcoinChain) TxConfirmations(txid string) (int, error) {
	var raw struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&raw, "getrawtransaction", txid, true); err != nil {
		return 0, err
	}
	return raw.Confirmations, nil
}

// RawTxAndConfirmations returns both the raw hex and the confirmation count in
// one verbose getrawtransaction (the maker needs both to VerifyFundedHTLC).
func (c *BitcoinChain) RawTxAndConfirmations(txid string) (string, int, error) {
	var raw struct {
		Hex           string `json:"hex"`
		Confirmations int    `json:"confirmations"`
	}
	if err := c.rpc.Call(&raw, "getrawtransaction", txid, true); err != nil {
		return "", 0, err
	}
	return raw.Hex, raw.Confirmations, nil
}

// Broadcast submits a raw Bitcoin tx hex and returns its txid.
func (c *BitcoinChain) Broadcast(rawHex string) (string, error) {
	var txid string
	return txid, c.rpc.Call(&txid, "sendrawtransaction", rawHex)
}

// NewDestScript returns a fresh wallet address' scriptPubKey for the maker to
// receive its claimed BTC, derived locally from the address (no Elements
// validateaddress dependency). It accepts any standard Bitcoin address type the
// wallet hands out (P2PKH, P2SH, bech32 v0/v1).
func (c *BitcoinChain) NewDestScript() ([]byte, error) {
	var addr string
	if err := c.rpc.Call(&addr, "getnewaddress"); err != nil {
		return nil, err
	}
	return c.AddressScript(addr)
}

// AddressScript decodes a Bitcoin address (under this chain's params) and
// returns its scriptPubKey.
func (c *BitcoinChain) AddressScript(addr string) ([]byte, error) {
	a, err := btcutil.DecodeAddress(addr, c.params)
	if err != nil {
		return nil, fmt.Errorf("decode address %q: %w", addr, err)
	}
	if !a.IsForNet(c.params) {
		return nil, fmt.Errorf("address %q not valid for %s", addr, c.params.Name)
	}
	return txscript.PayToAddrScript(a)
}

// Confirmations returns the number of confirmations of a Bitcoin tx via the
// wallet's gettransaction (works for wallet txs only); used by the harness.
func (c *BitcoinChain) Confirmations(txid string) (int, error) {
	var tx struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&tx, "gettransaction", txid); err != nil {
		return 0, err
	}
	return tx.Confirmations, nil
}

// LocatedHTLC is a funded HTLC output found on-chain (or in the wallet's
// mempool) by its EXACT redeemScript, WITHOUT broadcasting anything.
type LocatedHTLC struct {
	TxID   string
	Vout   uint32
	Amount uint64 // satoshis
}

// LocateFundedHTLC searches for an output already paying the P2SH of
// redeemScript — the dry-locate (broadcast-nothing) primitive that makes funding
// the HTLC crash-IDEMPOTENT. The caller (e.g. the LSP payer leg-bridge) computes
// the deterministic redeemScript, persists its refund key, and then — BEFORE
// broadcasting — asks this whether the HTLC is ALREADY funded. If a crash/reorg
// struck between a prior broadcast and the caller persisting the funding facts,
// the funded output is found here and ADOPTED, so the caller never double-funds
// (a second sendtoaddress to the SAME deterministic P2SH would strand the first
// output).
//
// # Return contract — (loc, definitive, err)
//
// The dry-locate feeds an IRREVERSIBLE decision (broadcast the funding), so it
// must never let "I couldn't tell" masquerade as "not funded". The result carries
// a `definitive` flag the caller keys its fail-closed rule on:
//
//   - (loc, true, nil)  — a funded output was DEFINITIVELY found + script-bound.
//     Adopt it; do not broadcast.
//   - (nil, true, nil)  — DEFINITIVELY not funded: EVERY lookup that could reveal
//     an existing funding ran cleanly and none did. Safe to broadcast.
//   - (nil, false, err) — UNCERTAIN: at least one lookup errored (or a candidate
//     that a lookup tied to the P2SH could not be fetched/bound), so a prior
//     self-broadcast funding cannot be ruled out. The caller MUST fail closed —
//     never broadcast; re-drive later. err names the failed lookup(s).
//
// # Wallet-aware lookups
//
// It consults three complementary sources, each reusing LocateHTLCOutputByScript
// for the final script-exact vout/value binding, so the LSP's OWN funding is
// found even on a pruned / non-txindex node or across a mempool eviction:
//
//  1. listunspent minconf=0 filtered to the HTLC P2SH — the wallet's own unspent
//     outputs at that address, INCLUDING a still-unconfirmed (0-conf) one. When
//     the P2SH is watched this surfaces the funded output directly.
//  2. listtransactions — per-output "send" rows carrying the destination address;
//     the LSP's funding send targets exactly the HTLC P2SH. This is where a
//     just-broadcast, still-UNCONFIRMED self-funding shows up because the LSP's
//     own wallet originated it, closing the tight crash window right after the
//     broadcast, before any confirmation.
//  3. scantxoutset over the confirmed UTXO set — a belt-and-suspenders path that
//     finds a CONFIRMED HTLC output even if the wallet's tx history was pruned.
//
// A candidate txid from (1)/(2) is bound to its raw tx wallet-aware: plain
// getrawtransaction first, then gettransaction (the wallet's own tx hex) — so a
// self-broadcast that getrawtransaction misses on a non-txindex node, or after
// the mempool evicted/restart-dropped it, is STILL found and bound.
//
// A located output is verified to pay EXACTLY P2SH(redeemScript) and to be
// unspent; only its own refund/claim key can move it, so adopting it is safe.
func (c *BitcoinChain) LocateFundedHTLC(redeemScript []byte) (*LocatedHTLC, bool, error) {
	addr, err := btcutil.NewAddressScriptHash(redeemScript, c.params)
	if err != nil {
		// Cannot even derive the target address -> cannot certify anything.
		return nil, false, fmt.Errorf("derive HTLC P2SH: %w", err)
	}
	p2sh := addr.EncodeAddress()

	// definitive stays true only while every lookup that could surface an existing
	// funding runs cleanly. The first failed lookup (RPC error, or a P2SH-tied
	// candidate whose tx cannot be fetched/bound) flips it false: a hidden prior
	// funding can no longer be ruled out, so the caller must fail closed.
	definitive := true
	var lookupErrs []string
	note := func(what string, e error) {
		definitive = false
		lookupErrs = append(lookupErrs, fmt.Sprintf("%s: %v", what, e))
	}

	// bindCandidate resolves a txid a lookup already tied to the HTLC P2SH into a
	// script-exact LocatedHTLC. A fetch/bind failure here is NOT benign: the lookup
	// asserted this tx pays the P2SH, so failing to confirm it means a funding may
	// exist that we cannot see -> uncertain (fail closed), never "not funded".
	bindCandidate := func(source, txid string) *LocatedHTLC {
		loc, err := c.locateInTx(txid, redeemScript)
		if err != nil {
			note(source+" candidate "+txid, err)
			return nil
		}
		return loc // may be nil if the tx genuinely does not fund this P2SH (benign)
	}

	seen := map[string]struct{}{}
	once := func(txid string) bool {
		if txid == "" {
			return false
		}
		if _, dup := seen[txid]; dup {
			return false
		}
		seen[txid] = struct{}{}
		return true
	}

	// (1) Wallet-aware: the wallet's OWN unspent outputs at the HTLC P2SH, minconf 0
	// so a still-unconfirmed (mempool) self-funding is included. Surfaces the funded
	// output without needing txindex or the mempool. Yields rows only when the P2SH
	// is watched/owned; otherwise empty (fall through to (2)/(3)).
	var unspents []struct {
		TxID    string `json:"txid"`
		Address string `json:"address"`
	}
	if err := c.rpc.Call(&unspents, "listunspent", 0, 9999999, []interface{}{p2sh}); err != nil {
		note("listunspent", err)
	} else {
		for _, u := range unspents {
			if u.Address != "" && u.Address != p2sh {
				continue
			}
			if !once(u.TxID) {
				continue
			}
			if loc := bindCandidate("listunspent", u.TxID); loc != nil {
				return loc, true, nil
			}
		}
	}

	// (2) Wallet-originated funding (mempool + confirmed). listtransactions "*" count
	// skip include_watchonly — a large count so an older pending funding is not paged
	// out.
	var txns []struct {
		TxID     string `json:"txid"`
		Address  string `json:"address"`
		Category string `json:"category"`
	}
	var lterr error
	for skip := 0; skip < 20000; skip += 1000 {
		var page []struct {
			TxID     string `json:"txid"`
			Address  string `json:"address"`
			Category string `json:"category"`
		}
		if lterr = c.rpc.Call(&page, "listtransactions", "*", 1000, skip, true); lterr != nil {
			break
		}
		txns = append(txns, page...)
		if len(page) < 1000 {
			break
		}
	}
	if lterr != nil {
		note("listtransactions", lterr)
	} else {
		for _, tx := range txns {
			if tx.Address != p2sh {
				continue
			}
			if tx.Category != "send" && tx.Category != "" {
				continue
			}
			if !once(tx.TxID) {
				continue
			}
			if loc := bindCandidate("listtransactions", tx.TxID); loc != nil {
				return loc, true, nil
			}
		}
	}

	// (3) Confirmed UTXO set by address descriptor (no wallet ownership needed).
	var scan struct {
		Success  bool `json:"success"`
		Unspents []struct {
			TxID string `json:"txid"`
		} `json:"unspents"`
	}
	if err := c.rpc.Call(&scan, "scantxoutset", "start", []interface{}{"addr(" + p2sh + ")"}); err != nil {
		note("scantxoutset", err)
	} else if !scan.Success {
		note("scantxoutset", fmt.Errorf("scan did not complete (success=false)"))
	} else {
		for _, u := range scan.Unspents {
			if !once(u.TxID) {
				continue
			}
			if loc := bindCandidate("scantxoutset", u.TxID); loc != nil {
				return loc, true, nil
			}
		}
	}

	if !definitive {
		// UNCERTAIN: nothing bound, but a lookup failed — do NOT certify not-funded.
		return nil, false, fmt.Errorf("locate is UNCERTAIN, a lookup failed (fail closed, do not broadcast): %s", strings.Join(lookupErrs, "; "))
	}
	return nil, true, nil
}

// ClassifyBTCHTLCSpend learns the AUTHORITATIVE on-chain fate of a funded Design-A
// BTC HTLC output — the structural replacement for the LSP payer bridge's persisted
// recoup-vs-refund intent flag. Given the funded outpoint (fundingTxid, vout) + the
// EXACT redeemScript, it reports whether that output is UNSPENT, SPENT_VIA_CLAIM
// (returning the revealed preimage P, sha256(P)==H), or SPENT_VIA_REFUND — by
// reading the chain, not a flag that can go stale across a persist/broadcast crash.
//
// # Return contract — (status, preimage, spenderConfs, definitive, err)
//
// The classification feeds an IRREVERSIBLE decision (settle the taker's hold vs
// release it), so — exactly like LocateFundedHTLC — it must never let "I couldn't
// tell" masquerade as a fate. It fails CLOSED via a `definitive` flag. `spenderConfs`
// is the SECONDARY burial-depth datum (0 when unspent/uncertain, or the spender's
// confirmation count when spent) — best-effort + fail-safe, it NEVER downgrades the
// definitive branch verdict (see spenderConfirmations):
//
//   - (BTCHTLCSpendUnspent, nil, 0, true, nil)   — DEFINITIVELY unspent: gettxout (which
//     accounts for a mempool spend) still shows the output in the UTXO set.
//   - (BTCHTLCSpendClaim, P, confs, true, nil)   — DEFINITIVELY spent via the claim/IF
//     branch; P is the revealed preimage. The maker was paid => the LSP must recoup
//     (at ANY depth — a claim reveals P immutably, so confs are immaterial here).
//   - (BTCHTLCSpendRefund, nil, confs, true, nil)— DEFINITIVELY spent via the refund/ELSE
//     branch; no P. The LSP reclaimed its BTC => release the hold to the taker ONLY once
//     `confs` buries the refund beyond a reorg margin (a 0-conf refund can be replaced
//     by a conflicting maker claim — the LSP keeps watching until it buries).
//   - (BTCHTLCSpendUnknown, nil, 0, false, err)  — UNCERTAIN: a lookup errored, or a
//     spend was seen but the spender/branch could not be resolved. NEVER report
//     UNSPENT here (a stale "unspent" would let the LSP release a hold the maker
//     already claimed against). The caller fails closed and re-drives; err says why.
//
// Wallet-aware + fail-closed: gettxout tests unspent; the spender is found CONFIRMED-first
// via a funding-height-anchored block scan, then the mempool, then the LSP wallet's own
// transactions; the branch is read off the spender's scriptSig by ClassifyBTCHTLCSpendScriptSig.
// The pruned / non-txindex tolerance holds ONLY for the LSP's OWN txs (its refund/funding are
// wallet txs, readable via gettransaction on any node). Reading a NON-wallet maker CLAIM
// confirmed in an older block — the sole on-chain source of P when the maker wins the T_btc
// race — REQUIRES txindex + an unpruned node (assert it once at startup with
// CheckClassifierNodeCapabilities); lacking them, that read fails CLOSED (UNCERTAIN), never a
// wrong verdict, but the front stays stranded until the node is fixed.
func (c *BitcoinChain) ClassifyBTCHTLCSpend(fundingTxid string, vout uint32, redeemScript []byte) (BTCHTLCSpendStatus, []byte, int, bool, error) {
	// Reject a malformed redeemScript up front — claim-detection keys on its H, and a
	// script we cannot read H from can never yield a DEFINITIVE claim/refund verdict.
	if _, err := HashFromHTLCRedeemScript(redeemScript); err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, fmt.Errorf("classify: %w", err)
	}

	// 1) Still unspent? gettxout include_mempool=true accounts for a mempool spend, so
	//    a non-null result is a DEFINITIVE unspent. An RPC error is UNCERTAIN (fail
	//    closed): never report unspent when the lookup itself failed.
	var utxo *struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&utxo, "gettxout", fundingTxid, vout, true); err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, fmt.Errorf("classify UNCERTAIN: gettxout %s:%d failed (fail closed, do not act): %w", fundingTxid, vout, err)
	}
	if utxo != nil {
		return BTCHTLCSpendUnspent, nil, 0, true, nil
	}

	// 2) Spent (or the funding vanished in a reorg). Find the spender wallet-aware. A
	//    miss returns "" and an UNCERTAIN error — inherently fail-closed (we never
	//    guess a branch we did not observe).
	spenderTxid, err := c.findSpender(fundingTxid, vout)
	if err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, err
	}

	// 3) Read the branch off the authoritative on-chain witness of the spender's input
	//    that spends (fundingTxid, vout).
	raw, err := c.rawTxWalletAware(spenderTxid)
	if err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, fmt.Errorf("classify UNCERTAIN: fetch spender %s (fail closed): %w", spenderTxid, err)
	}
	sigScript, err := htlcSpendSigScript(raw, fundingTxid, vout)
	if err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, fmt.Errorf("classify UNCERTAIN: %w", err)
	}
	status, preimage, err := ClassifyBTCHTLCSpendScriptSig(sigScript, redeemScript)
	if err != nil {
		return BTCHTLCSpendUnknown, nil, 0, false, fmt.Errorf("classify UNCERTAIN: %w", err)
	}
	// 4) BURIAL DEPTH of the spender (secondary datum; NEVER downgrades the DEFINITIVE
	//    branch verdict above). The LSP payer bridge treats a REFUND as a TERMINAL fate
	//    only once its own refund is BURIED beyond a reorg margin: an UNCONFIRMED (0-conf)
	//    refund can be replaced by a conflicting maker CLAIM (RBF/reorg), so the LSP must
	//    know the depth before releasing the taker's hold. Best-effort + fail-SAFE — a
	//    mempool / unreadable / reorg-conflicted spender reports 0 (SHALLOW), the safe
	//    direction (keep watching, never prematurely release). A CLAIM is acted on at any
	//    depth, so its depth is immaterial (a 0 here on a claim is harmless).
	spenderConfs := c.spenderConfirmations(spenderTxid)
	return status, preimage, spenderConfs, true, nil
}

// spenderConfirmations returns the confirmation DEPTH of an HTLC-spend tx, wallet-aware
// and BEST-EFFORT. It underpins the payer bridge's "a refund is terminal only once BURIED"
// rule: a maker CLAIM can replace an UNCONFIRMED (0-conf) LSP refund via RBF/reorg, so the
// LSP must know how deep its own refund is before releasing the taker's hold.
//
// Sources, in order: verbose getrawtransaction (a confirmed spender on a txindex node, or
// mempool => 0) then the wallet's gettransaction (the LSP's OWN refund on a pruned /
// non-txindex node — the case that MATTERS, since the refund is always a wallet tx). A
// mempool/unconfirmed spender, a conflicted/reorged-out spender (Bitcoin reports a NEGATIVE
// count), or an unreadable count ALL collapse to 0 — the SAFE direction for the burial test
// (read as SHALLOW => keep watching, never prematurely treat a refund as terminal).
func (c *BitcoinChain) spenderConfirmations(txid string) int {
	var grt struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&grt, "getrawtransaction", txid, true); err == nil && grt.Confirmations > 0 {
		return grt.Confirmations
	}
	var gt struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&gt, "gettransaction", txid, true); err == nil && gt.Confirmations > 0 {
		return gt.Confirmations
	}
	return 0
}

// ClassifierNodeCapabilities is the result of CheckClassifierNodeCapabilities — whether the
// bitcoind backing the LSP payer leg-bridge's HTLC-spend classifier can read a NON-wallet
// maker CLAIM confirmed in an older block (the sole on-chain source of P when the maker wins
// the T_btc race).
type ClassifierNodeCapabilities struct {
	Pruned        bool // getblockchaininfo.pruned — an old block body is unreadable when true
	TxindexKnown  bool // getindexinfo reported a "txindex" entry at all
	TxindexSynced bool // ...and that entry is fully synced
}

// OK reports whether the node has BOTH required capabilities: an unpruned chain and a synced
// txindex. Only then can ClassifyBTCHTLCSpend locate + read a non-wallet maker claim.
func (n ClassifierNodeCapabilities) OK() bool {
	return !n.Pruned && n.TxindexKnown && n.TxindexSynced
}

// CheckClassifierNodeCapabilities verifies, ONCE at startup, that the bitcoind backing the LSP
// payer leg-bridge's HTLC-spend classifier can locate + read a NON-wallet maker CLAIM that
// confirmed in an older block. That claim is the ONLY on-chain source of the preimage P when
// the maker wins the T_btc race, so a node that cannot read it can never recoup the front.
//
// Two node properties are REQUIRED — and ONLY for the non-wallet-claim case; the LSP's OWN
// refund/funding are wallet txs, readable via gettransaction on ANY node:
//
//   - txindex present + synced: getrawtransaction on the maker's claim (never a wallet tx, so
//     no gettransaction fallback exists) needs the transaction index. Without it the classifier
//     fetches the claim's raw bytes, fails, and fails CLOSED (UNCERTAIN) forever — P is never
//     extracted, the front is stranded.
//   - unpruned: the funding-height-anchored block scan reads OLD block bodies (getblock) to find
//     the claim. A pruned node cannot serve an old block body, so the scan cannot find it.
//
// Fail-closed classification already prevents a WRONG verdict on such a node (UNCERTAIN, never a
// fabricated fate), so funds are never lost to a mis-settle. This check makes the operational
// requirement EXPLICIT + loud at startup instead of silently stranding a front at classify time.
// It returns caps plus a non-nil error naming what is missing (nil error when both hold); the
// caller logs it, and the payer bridge should refuse to originate a front on a node that lacks
// the capability. RPC errors on the two info calls also fail closed (returned as the error).
func (c *BitcoinChain) CheckClassifierNodeCapabilities() (ClassifierNodeCapabilities, error) {
	var caps ClassifierNodeCapabilities

	var bci struct {
		Pruned bool `json:"pruned"`
	}
	if err := c.rpc.Call(&bci, "getblockchaininfo"); err != nil {
		return caps, fmt.Errorf("classifier node check: getblockchaininfo failed (cannot confirm the node is unpruned): %w", err)
	}
	caps.Pruned = bci.Pruned

	// getindexinfo returns a map keyed by index name; "txindex" present => enabled, with a
	// "synced" flag. Absence of the key means txindex is disabled.
	var idx map[string]struct {
		Synced bool `json:"synced"`
	}
	if err := c.rpc.Call(&idx, "getindexinfo"); err != nil {
		return caps, fmt.Errorf("classifier node check: getindexinfo failed (cannot confirm txindex is present + synced): %w", err)
	}
	ti, ok := idx["txindex"]
	caps.TxindexKnown = ok
	caps.TxindexSynced = ok && ti.Synced

	var missing []string
	if caps.Pruned {
		missing = append(missing, "node is PRUNED (cannot read an old block body to locate a non-wallet maker claim)")
	}
	if !caps.TxindexKnown {
		missing = append(missing, "txindex is DISABLED (cannot getrawtransaction a non-wallet maker claim)")
	} else if !caps.TxindexSynced {
		missing = append(missing, "txindex is present but NOT yet synced")
	}
	if len(missing) > 0 {
		return caps, fmt.Errorf("BTC node backing the payer leg-bridge classifier lacks a required capability: %s; a confirmed NON-wallet maker CLAIM would be unreadable, so P could not be extracted to recoup the front (classification fails closed => the front is stranded, never mis-settled)", strings.Join(missing, "; "))
	}
	return caps, nil
}

// Spender-scan bounds. NOT magic numbers: they are SIZED FROM the LSP payer bridge's
// coupled locktime deltas so the block scan for a spent output's spender covers the
// full realistic offline horizon, never a fixed recent window a longer outage silently
// outruns.
const (
	// bridgeBtcLocktimeDelta mirrors the LSP payer bridge's BTC-HTLC locktime delta
	// (seqdex client xdriver.go BtcLocktimeDelta default: T_btc = btcTip + 100 blocks,
	// ~16h). A spend of the funded output — the maker CLAIM (reveals P, at latest ~T_btc)
	// or the LSP REFUND (at/after T_btc) — therefore lands within roughly this many blocks
	// of the funding.
	bridgeBtcLocktimeDelta = 100
	// spenderScanBlocks bounds the FALLBACK recent-block spender scan, used ONLY when the
	// funding height cannot be read (see findSpender step 3). It is several times the BTC
	// locktime delta so it also absorbs a deep reorg AND an LSP that was offline across the
	// whole HTLC lifetime — a maker CLAIM that confirmed while the LSP slept must stay
	// inside the window, or the LSP could never extract P to recoup (fund-loss). The
	// PRIMARY scan is funding-height-anchored (window-INDEPENDENT) and never relies on
	// this bound.
	spenderScanBlocks = 6 * bridgeBtcLocktimeDelta // 600 blocks (~4 days on BTC)
)

// findSpender locates the tx that spent (fundingTxid, vout), wallet-aware and
// fail-closed. On success it returns the spender txid (nil error). On failure it
// returns "" with an error describing why the fate is UNCERTAIN — either a top-level
// lookup errored (so a source that could hold the spender was skipped) or the spend
// was not found in the scanned range. "" ALWAYS maps to UNCERTAIN in the caller, so a
// miss can only ever fail closed, never produce a wrong definitive verdict.
//
// # Ordering — a CONFIRMED spend WINS over a 0-conf / conflicted one
//
// The lookups run CONFIRMED-FIRST, and that order is LOAD-BEARING (fund-safety), not just
// a perf choice. During the T_btc race the funded output can have TWO would-be spenders in
// flight: the maker's CLAIM (reveals P) and the LSP's own REFUND. If the maker's claim
// CONFIRMED, it is the authoritative fate (the maker was paid) even while the LSP's evicted
// 0-conf refund still lingers in this wallet's history. So:
//
//  1. The funding-height-anchored BLOCK scan runs FIRST. Any CONFIRMED spender — the maker
//     CLAIM that won the race, or a buried refund — is found here and returned, taking
//     precedence over a 0-conf mempool/wallet spend. It also finds a confirmed NON-wallet
//     claim CHEAPLY (near the funding/T_btc height) instead of after the O(mempool)+O(1000
//     wallet-tx) lookups. NB reading such a non-wallet claim's block + branch REQUIRES
//     txindex + an unpruned node (see CheckClassifierNodeCapabilities); the wallet-aware
//     fallbacks below cover only the LSP's OWN txs, never the maker's claim.
//  2. The MEMPOOL — an unconfirmed maker claim or LSP refund, reached only when NO confirmed
//     spender exists yet.
//  3. The LSP wallet's OWN transactions — its 0-conf refund, bound wallet-aware even on a
//     pruned / non-txindex node (it is a wallet tx). A candidate the wallet reports as
//     CONFLICTED (gettransaction confirmations < 0: replaced/evicted, e.g. by a maker claim
//     that beat it) is REJECTED, never returned — returning a dead refund would misclassify
//     a CLAIMED HTLC as refunded and release the taker's hold while the maker already took
//     the BTC (fund-loss). Rejecting it yields "" => UNCERTAIN => the LSP keeps watching
//     until the winning spender confirms (then found by the block scan).
//
// The block scan is ANCHORED at the FUNDING height whenever it can be read: the spender can
// only exist at a height >= the funding height, so walking forward from there is
// window-INDEPENDENT — an old maker CLAIM confirmed while the LSP was offline for the WHOLE
// HTLC lifetime is still found (closing the round-7 hole where a fixed 50-block window
// silently missed such a claim, so the LSP could never extract P and lost its front). When
// the funding height is unreadable the scan falls back to the last spenderScanBlocks blocks
// (a bound sized from the bridge locktime horizon). Each block is matched via getblock
// verbosity 2 — the outpoint is found in the block's input list directly, so a whole block
// costs ONE RPC (no per-tx fetch), keeping a hundreds-of-blocks scan affordable; a hit is
// bound structurally (an input referencing fundingTxid:vout), so it is never ambiguous.
func (c *BitcoinChain) findSpender(fundingTxid string, vout uint32) (string, error) {
	var lookupErrs []string
	note := func(what string, e error) {
		lookupErrs = append(lookupErrs, fmt.Sprintf("%s: %v", what, e))
	}
	seen := map[string]struct{}{}
	// spends fetches a candidate wallet-aware and reports whether it spends the
	// outpoint. A fetch/parse failure returns false (skip); a genuine miss only ever
	// yields UNCERTAIN, so skipping is safe.
	spends := func(candTxid string) bool {
		if candTxid == "" {
			return false
		}
		if _, dup := seen[candTxid]; dup {
			return false
		}
		seen[candTxid] = struct{}{}
		raw, err := c.rawTxWalletAware(candTxid)
		if err != nil {
			return false
		}
		return spenderSpendsOutpoint(raw, fundingTxid, vout)
	}

	// (1) CONFIRMED spend — block scan FIRST, so a confirmed spender (a maker CLAIM that
	// won the T_btc race, or a buried refund) BEATS a 0-conf mempool/wallet spend and is
	// found cheaply near the funding/T_btc height. Anchor at the funding height when
	// readable (window-INDEPENDENT: never misses an old claim regardless of outage length),
	// else fall back to the last spenderScanBlocks blocks from the tip. Walk FORWARD and
	// stop at the first block spending the outpoint (that spend is unique).
	if tip, err := c.BlockCount(); err != nil {
		// Without the tip the block scan can neither run nor be bounded; note it (it makes
		// the final result UNCERTAIN if the 0-conf sources below also miss) and fall through
		// rather than returning early — a 0-conf mempool/wallet spender may still resolve.
		note("getblockcount", err)
	} else {
		from := tip - spenderScanBlocks
		if fh, ok := c.txBlockHeight(fundingTxid); ok {
			// The spender is at height >= the funding height; start exactly there so no
			// in-range block is skipped no matter how long the LSP was offline.
			from = fh
		}
		if from < 0 {
			from = 0
		}
		for h := from; h <= tip; h++ {
			var bh string
			if err := c.rpc.Call(&bh, "getblockhash", h); err != nil {
				// A missing hash for a height <= tip is a real RPC fault, not a benign gap;
				// stop and note it (the caller re-drives).
				note("getblockhash", err)
				break
			}
			spenderTxid, found, err := c.spenderInBlock(bh, fundingTxid, vout)
			if err != nil {
				// This block could hold the spender but we could not read it (a PRUNED node
				// cannot serve an old block body): note it (=> UNCERTAIN at the end) and keep
				// scanning — a later block may still resolve it.
				note("getblock "+bh, err)
				continue
			}
			if found {
				return spenderTxid, nil
			}
		}
	}

	// (2) Mempool — an unconfirmed maker claim or LSP refund (reached only when NO confirmed
	// spender exists yet; a confirmed spend would have been returned by the block scan).
	var mempool []string
	if err := c.rpc.Call(&mempool, "getrawmempool"); err != nil {
		note("getrawmempool", err)
	} else {
		for _, mt := range mempool {
			if spends(mt) {
				return mt, nil
			}
		}
	}

	// (3) The LSP wallet's OWN transactions — its 0-conf refund spend shows here (as a
	// receive back to itself) and binds wallet-aware even on a pruned / non-txindex node (it
	// is a wallet tx). A CONFLICTED candidate is REJECTED, never returned as the spender:
	// gettransaction confirmations < 0 means a conflicting tx confirmed (the LSP's own refund
	// evicted by a maker claim that won the race), so the dead refund is NOT the authoritative
	// spend — returning it would misclassify a claimed HTLC as refunded and release the taker's
	// hold after the maker took the BTC (fund-loss). Rejecting it leaves the fate UNCERTAIN so
	// the LSP keeps watching until the winning (confirmed) spender is found by the block scan.
	var txns []struct {
		TxID string `json:"txid"`
	}
	if err := c.rpc.Call(&txns, "listtransactions", "*", 1000, 0, true); err != nil {
		note("listtransactions", err)
	} else {
		for _, tx := range txns {
			if !spends(tx.TxID) {
				continue
			}
			if c.walletTxConflicted(tx.TxID) {
				note("wallet spender "+tx.TxID, fmt.Errorf("CONFLICTED (gettransaction confirmations < 0): replaced/evicted, likely by a maker claim that won the race — not the authoritative spend, refusing to classify a refund off it"))
				continue
			}
			return tx.TxID, nil
		}
	}

	return "", spenderMissErr(fundingTxid, vout, lookupErrs)
}

// walletTxConflicted reports whether the wallet considers txid CONFLICTED — gettransaction
// confirmations < 0, which Bitcoin sets when a CONFLICTING tx confirmed (e.g. the LSP's own
// 0-conf refund evicted by a maker claim that won the T_btc race). A conflicted tx is DEAD:
// findSpender must never return it as the authoritative spender, or the classifier would read
// a REFUND off a tx the chain already replaced with a CLAIM and release the taker's hold after
// the maker took the BTC (fund-loss). Best-effort + fail-SAFE: an unreadable count returns
// false (never INVENT a conflict) — the block-scan precedence already routes a confirmed claim
// ahead of any 0-conf wallet tx, and a LIVE 0-conf refund must still be classifiable.
func (c *BitcoinChain) walletTxConflicted(txid string) bool {
	var gt struct {
		Confirmations int `json:"confirmations"`
	}
	if err := c.rpc.Call(&gt, "gettransaction", txid, true); err != nil {
		return false
	}
	return gt.Confirmations < 0
}

// spenderMissErr builds the UNCERTAIN error findSpender returns when no spender was
// located. A noted lookup failure means a source that could have held the spender was
// skipped (so a hidden spender cannot be ruled out); with no failures the output is
// spent yet its spender is absent from every scanned source — either way the caller
// fails closed and re-drives, never guessing a fate.
func spenderMissErr(fundingTxid string, vout uint32, lookupErrs []string) error {
	if len(lookupErrs) > 0 {
		return fmt.Errorf("classify UNCERTAIN: spender of %s:%d not found and a lookup failed (fail closed, re-drive): %s", fundingTxid, vout, strings.Join(lookupErrs, "; "))
	}
	return fmt.Errorf("classify UNCERTAIN: output %s:%d is spent but its spender was not found in the mempool, this wallet, or the scanned blocks (funding height..tip, else the last %d blocks) (fail closed, re-drive)", fundingTxid, vout, spenderScanBlocks)
}

// spenderInBlock reads block <hash> at verbosity 2 (each tx WITH its inputs' prevouts)
// and returns the txid of the first tx that spends (fundingTxid, vout). Verbosity 2
// lets a whole block be matched from the block JSON alone — no per-tx getrawtransaction
// — so a wide (funding-height-anchored) scan costs one RPC per block. Returns
// ("", false, nil) when no tx in the block spends the outpoint, and ("", false, err)
// only when the block itself could not be read (the caller treats that as UNCERTAIN).
func (c *BitcoinChain) spenderInBlock(blockHash, fundingTxid string, vout uint32) (string, bool, error) {
	var blk struct {
		Tx []struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
	}
	if err := c.rpc.Call(&blk, "getblock", blockHash, 2); err != nil {
		return "", false, err
	}
	for _, tx := range blk.Tx {
		for _, vin := range tx.Vin {
			// A coinbase input carries no prevout txid (""), so it never matches a real
			// outpoint; only a genuine spend of (fundingTxid, vout) hits here.
			if vin.TxID == fundingTxid && vin.Vout == vout {
				return tx.TxID, true, nil
			}
		}
	}
	return "", false, nil
}

// txBlockHeight returns the height of the block that confirms txid and whether it could
// be established — the ANCHOR for a window-independent spender scan. Wallet-aware:
// gettransaction carries "blockheight" for a confirmed wallet tx even on a pruned /
// non-txindex node, and the LSP OWNS the funding tx it passes here, so this is the
// reliable path; getrawtransaction verbose (txindex) is the fallback via the confirming
// block header. An unconfirmed / unknown / unreadable tx returns ok=false, and the
// caller widens to the tip-anchored fallback window rather than trusting a bogus height.
func (c *BitcoinChain) txBlockHeight(txid string) (int64, bool) {
	var gt struct {
		BlockHeight *int64 `json:"blockheight"`
		BlockHash   string `json:"blockhash"`
	}
	if err := c.rpc.Call(&gt, "gettransaction", txid, true); err == nil {
		if gt.BlockHeight != nil && *gt.BlockHeight >= 0 {
			return *gt.BlockHeight, true
		}
		if gt.BlockHash != "" {
			if h, ok := c.blockHeaderHeight(gt.BlockHash); ok {
				return h, true
			}
		}
	}
	var rt struct {
		BlockHash string `json:"blockhash"`
	}
	if err := c.rpc.Call(&rt, "getrawtransaction", txid, true); err == nil && rt.BlockHash != "" {
		if h, ok := c.blockHeaderHeight(rt.BlockHash); ok {
			return h, true
		}
	}
	return 0, false
}

// blockHeaderHeight resolves a block hash to its height via getblockheader; ok=false on
// any read failure so the caller falls back rather than anchoring on a bogus height.
func (c *BitcoinChain) blockHeaderHeight(blockHash string) (int64, bool) {
	var hdr struct {
		Height int64 `json:"height"`
	}
	if err := c.rpc.Call(&hdr, "getblockheader", blockHash); err != nil {
		return 0, false
	}
	return hdr.Height, true
}

// locateInTx resolves txid to the HTLC output paying P2SH(redeemScript):
//
//   - (loc, nil) — txid funds this exact P2SH; loc is the bound outpoint/value.
//   - (nil, nil) — txid fetched + parsed cleanly but does NOT fund this P2SH
//     (a false candidate; a certain, benign negative).
//   - (nil, err) — txid could NOT be fetched (unavailable via getrawtransaction
//     AND gettransaction); the caller must treat this as UNCERTAIN.
//
// The fetch is wallet-aware: getrawtransaction (mempool / txindex) first, then
// gettransaction (the wallet's own tx hex) so a self-broadcast that
// getrawtransaction misses — pruned / non-txindex node, or evicted/restart-dropped
// mempool — is still bound.
func (c *BitcoinChain) locateInTx(txid string, redeemScript []byte) (*LocatedHTLC, error) {
	rawHex, err := c.rawTxWalletAware(txid)
	if err != nil {
		return nil, err
	}
	vout, value, err := LocateHTLCOutputByScript(c.params, rawHex, redeemScript)
	if err != nil {
		// Fetched + parsed, but this tx does not pay our P2SH: a certain negative.
		return nil, nil
	}
	return &LocatedHTLC{TxID: txid, Vout: vout, Amount: value}, nil
}

// rawTxWalletAware returns txid's raw (Bitcoin-serialized) hex, consulting the
// WALLET when the mempool/txindex view misses it. getrawtransaction covers a
// mempool tx (always) and any confirmed tx on a txindex node; gettransaction is
// the wallet-aware fallback that returns the LSP's OWN funding hex even on a
// non-txindex/pruned node or after the mempool dropped the still-unconfirmed
// self-broadcast (a node restart or eviction) — because the wallet persists its
// own transactions. Returns an error only when BOTH paths fail to produce hex.
//
// The wallet fallback ONLY covers the LSP's own txs. A NON-wallet tx — the maker's
// CLAIM, the sole on-chain source of P — has NO gettransaction hex, so it is readable
// here solely via getrawtransaction: i.e. it REQUIRES txindex (or the claim still in the
// mempool). On a non-txindex node a confirmed non-wallet claim returns an error here and
// the caller fails CLOSED (UNCERTAIN); CheckClassifierNodeCapabilities asserts that
// requirement up front so the front is not silently stranded at classify time.
func (c *BitcoinChain) rawTxWalletAware(txid string) (string, error) {
	// Primary: getrawtransaction (non-verbose). Returns "" with nil err when the
	// node has no such tx in mempool/index, so treat empty as a miss and fall back.
	if raw, err := c.RawTx(txid); err == nil && raw != "" {
		return raw, nil
	}
	// Wallet-aware fallback: gettransaction include_watchonly verbose. Its "hex" is
	// the raw tx (present whenever the wallet knows the tx); "decoded"/walletconflicts
	// are surfaced by verbose but we bind off hex.
	var gt struct {
		Hex string `json:"hex"`
	}
	if err := c.rpc.Call(&gt, "gettransaction", txid, true, true); err != nil {
		return "", fmt.Errorf("gettransaction %s: %w", txid, err)
	}
	if gt.Hex == "" {
		return "", fmt.Errorf("tx %s not available via getrawtransaction or gettransaction", txid)
	}
	return gt.Hex, nil
}
