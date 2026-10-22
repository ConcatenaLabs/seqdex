// Command seqob-maker is the long-running MAKER participant for the SeqOB
// order-book DEX. It is just one participant (anyone can run it the same way):
// it posts a signed resting offer to the relay over WebSocket, then settles each
// lift by reusing the PROVEN Ocean settlement (wallet.Service.CompleteSwap, now
// blind-aware) via the shared internal/seqob/client primitives. Confidential is
// opt-in: a confidential offer publishes a blinding pubkey and settles blinded;
// an explicit offer omits it and settles explicit. The relay never decrypts the
// couriered swap messages; it only routes ciphertext.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"github.com/thanhpk/randstr"
	"google.golang.org/protobuf/encoding/protojson"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	oceanwallet "github.com/aejkcs50/seqdex/daemon/internal/infrastructure/ocean-wallet"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/aejkcs50/seqdex/daemon/pkg/swap"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true}
var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// ══ GRACEFUL SHUTDOWN: NEVER DIE MID-SWAP ════════════════════════════════════
//
// The maker fleet is repriced by killing the process and starting a fresh one.
// With no signal handler, SIGTERM killed the maker WHEREVER it was — including
// after it had locked the asset on-chain and issued its invoice, waiting for the
// taker to pay. The relay then saw the maker depart and ended the session, so the
// taker never received the leg it was waiting for: its trade hung until timeout
// while the maker's asset stayed locked until T_seq. Nothing about the parent
// chain was involved; the swap died for a process-lifecycle reason alone.
//
// The supervisor tried to avoid this by deferring the kill until the maker had
// been LOG-QUIET for a while — but a maker mid-swap is quiet BY DEFINITION: it has
// said everything it has to say and is waiting on its counterparty. The heuristic
// therefore killed precisely the swaps it existed to protect. (subasset-requote.sh
// says as much in its own comment, and names this as the proper fix.)
//
// So the maker now owns the decision, because only it knows whether value is in
// flight: on SIGTERM it stops taking NEW lifts and exits once the ones it has are
// done. The cap is a backstop against a wedged session holding a process forever —
// it is deliberately longer than any honest swap needs, and exiting on it is still
// safer than exiting immediately because the in-flight count is logged either way.
var (
	draining int32
	busyFn   atomic.Value // func() int — how many swaps this serve loop still has in flight
)

// setBusyFn is called by whichever serve loop is running.
func setBusyFn(f func() int) { busyFn.Store(f) }

func inFlightSwaps() int {
	if f, ok := busyFn.Load().(func() int); ok && f != nil {
		return f()
	}
	return 0
}

// isDraining reports whether a shutdown has been requested; serve loops refuse NEW
// lifts once it is set, so the drain can actually finish.
func isDraining() bool { return atomic.LoadInt32(&draining) == 1 }

// refuseIfDraining answers a lift with an explicit, retryable refusal while the
// maker is shutting down, so it takes no NEW value during the drain. Every serve
// loop calls it after reserving the in-flight slot; the caller releases the slot.
func refuseIfDraining(sid string, cr *client.Crypter, refuse func(sid string, cr *client.Crypter, code, msg string)) bool {
	if !isDraining() {
		return false
	}
	refuse(sid, cr, "draining", "this maker is shutting down for a re-quote; retry on another offer")
	return true
}

// derivedRefundKey is a per-purpose key derived from the maker's identity key, used
// where a maker locks funds it may later need to RECLAIM by timelock. A fresh random
// key minted for one swap lives only in that process: if the supervisor re-quotes or
// kills it before the refund, the locked asset is lost for good (the 2026-08-11
// class). A key derived from -maker-priv can always be rebuilt from the operator's
// own key file plus the on-chain script, with no per-swap state at all.
func derivedRefundKey(identity *btcec.PrivateKey, purpose string) *xchain.Key {
	h := sha256.Sum256(append(identity.Serialize(), []byte("seqob-maker/refund/"+purpose)...))
	return xchain.KeyFromBytes(h[:])
}

func armGracefulShutdown(cap time.Duration) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-ch
		atomic.StoreInt32(&draining, 1)
		n := inFlightSwaps()
		if n == 0 {
			fmt.Printf("%v: no swap in flight; exiting cleanly\n", sig)
			os.Exit(0)
		}
		fmt.Printf("%v: %d swap(s) IN FLIGHT — refusing new lifts and draining before exit"+
			" (a kill here would strand a locked asset)\n", sig, n)
		deadline := time.Now().Add(cap)
		for time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
			if n = inFlightSwaps(); n == 0 {
				fmt.Printf("drain complete; exiting cleanly\n")
				os.Exit(0)
			}
		}
		fmt.Printf("drain cap %s reached with %d swap(s) still in flight; exiting anyway"+
			" (operator: check for a wedged session)\n", cap, n)
		os.Exit(0)
	}()
}

func main() {
	relay := flag.String("relay", "http://127.0.0.1:9955", "relay base URL")
	ocean := flag.String("ocean", "127.0.0.1:18000", "ocean wallet endpoint")
	nodeRPC := flag.String("node-rpc", "", "Sequentia node RPC URL (for the open fee market)")
	account := flag.String("account", "", "Ocean account that holds the OFFER asset (and funds the fee)")
	makerPriv := flag.String("maker-priv", "", "maker offer/identity + E2E key (32-byte hex); generated if empty")
	base := flag.String("base", "gold", "base asset id")
	quote := flag.String("quote", "usdx", "quote asset id")
	side := flag.String("side", "sell", "maker side: sell|buy (sells/buys the base)")
	baseAmt := flag.Uint64("base-amount", 100, "base size (base atoms)")
	quoteAmt := flag.Uint64("quote-amount", 45, "quote size (quote atoms)")
	feeAsset := flag.String("fee-asset", "", "preferred fee asset hint (any-asset fee market)")
	expiry := flag.Duration("expiry", time.Hour, "time until the offer expires")
	quoteAge := flag.Duration("max-quote-age", 0, "retire for a re-price no later than this after posting, regardless of -expiry (0 = only at expiry); the supervisor re-runs at the current price")
	minAnchor := flag.Uint("min-anchor-depth", 0, "Bitcoin-anchor confs before FILLED (0 = 0-conf tolerant)")
	confidential := flag.Bool("confidential", false, "post a confidential offer (blinded settlement); default explicit — Sequentia is transparent by default, confidentiality is opt-in")
	msats := flag.Uint64("msats-per-byte", 150, "network fee rate (milli-sat/vByte). The default keeps ~50%% headroom over the node's floor: a fee that clears the floor by a hair at build time strands the tx once the fee asset's published rate dips before confirmation")
	offerID := flag.String("offer-id", "", "offer id (random 16-byte hex if empty)")
	levels := flag.Int("levels", 1, "samechain: ladder depth — quote this many price levels on this side from ONE process/account (level 0 = the configured price/size; each deeper level rests further from the touch and larger). Every level is a real co-signable offer of this maker")
	levelBps := flag.Int("level-bps", 40, "samechain ladder: extra distance from the touch per level, in basis points of the quote amount")
	levelGrowth := flag.Float64("level-growth", 1.12, "samechain ladder: size multiplier per level (deeper levels rest larger)")
	mode := flag.String("mode", "samechain", "settlement mode: samechain | cross | lightning | pureln | subasset | subasset-sell (cross = BTC<->asset on-chain HTLC; lightning = asset<->BTC-over-LN submarine swap; pureln = BOTH legs over Lightning; subasset = the submarine's MIRROR: asset over Lightning + BTC on-chain HTLC, taker pays BTC on-chain and receives the asset over LN; subasset-sell = the same rails in the other direction, taker pays the asset over LN and receives BTC on-chain; base is the asset, quote is the BTC sentinel)")
	// Cross-mode settlement wiring (pkg/xchain, no Ocean needed): the SEQ leg is
	// funded from the Sequentia NODE wallet and the BTC leg is claimed into the
	// bitcoind wallet — the same reserves the RFQ maker uses.
	btcRPCURL := flag.String("btc-rpc", "", "cross: bitcoind RPC URL http://user:pass@host:port (required for -mode cross)")
	btcWallet := flag.String("btc-wallet", "", "cross: bitcoind wallet holding/receiving the BTC side")
	btcChainName := flag.String("btc-chain", "testnet4", "cross: parent chain params: testnet4 | regtest")
	xseqRPCURL := flag.String("xseq-rpc", "", "cross: Sequentia node RPC URL http://user:pass@host:port (required for -mode cross)")
	xseqWallet := flag.String("xseq-wallet", "", "cross: Sequentia node wallet funding the asset leg")
	btcDelta := flag.Uint("btc-locktime-delta", 180, "cross: T_btc = parent tip + this (longer leg in time; ~30h). 180 clears the LSP payer-bridge fund gate for T_seq=240 (B1 floor ~150, B3 ceiling ~192); see RunMakerForward's BtcLocktimeDelta comment")
	seqDelta := flag.Uint("seq-locktime-delta", 240, "cross: T_seq = SEQ tip + this (shorter leg in time; ~2h — must cover the taker's real parent confirmation, or takers refuse the terms)")
	minBTCConf := flag.Int("min-btc-conf", 0, "cross: confirmations required on the counterparty's BTC leg. 0 = accept from the mempool, which is what keeps a rail-crossing take INSTANT: a taker paying over Lightning against a best-priced on-chain maker must not wait a Bitcoin block, because the bridge (the LSP) is already holding that taker's Lightning payment when it funds and so gains nothing by double-spending itself. Raise it for a maker facing anonymous takers or real value.")
	spendFee := flag.Uint64("spend-fee", 1000, "cross: HTLC spend fee target in native sats (converted per-asset via the fee market)")
	btcFeeRate := flag.Float64("btc-fee-rate", 2, "cross: sat/vB fee rate for funding the BTC HTLC leg (explicit, so it never depends on the node's estimatesmartfee/settxfee; 0 = node default)")
	xstateDir := flag.String("xstate-dir", "xmaker-sessions", "cross: directory for per-lift session state (keys/legs; the recovery material)")
	resume := flag.Bool("resume", false, "instead of serving, finish every non-terminal session in -xstate-dir (post-restart on-chain claim/refund: cross, submarine, submarine-reverse, subasset, subasset-sell) and exit; pass -maker-priv for sub-asset BUY claims")
	lnSocket := flag.String("ln-socket", "", "lightning/pureln: the maker's SeqLN-on-Bitcoin lightning-rpc unix socket (BTC leg; required for -mode lightning and -mode pureln)")
	subAnchor := flag.Int64("sub-anchor-depth", 3, "lightning: Bitcoin-anchor depth the maker requires on the taker's asset funding before it pays the invoice (>=2; the submarine cross-leg safety gate)")
	max0conf := flag.Uint64("max-0conf", 0, "lightning: 0-conf LP-fronting cap (asset atoms). Trades whose on-chain asset leg is <= this settle INSTANTLY (skip the anchor-bury wait); the maker/taker fronts the Bitcoin-reorg risk. Advertised in the offer's LightningTerms. 0 = disabled (always anchor-gate).")
	onchainCltv := flag.Uint("onchain-cltv", 240, "lightning: advisory CLTV (blocks) in the resting LightningTerms (the load-bearing T_seq is minted per-lift)")
	assetLnSocket := flag.String("asset-ln-socket", "", "pureln: the maker's SeqLN-on-Sequentia lightning-rpc unix socket (asset leg; required for -mode pureln)")
	btcAsset := flag.String("btc-asset", "", "pureln: counter-leg SETTLEMENT asset id (hex); empty = policy asset / real BTC-LN. Set to route the counter leg over a 2nd issued asset (asset<->asset pure-LN)")
	quoteAsset := flag.String("quote-asset", "", "pureln: QUOTE asset id (hex) the offer advertises; empty = the BTC sentinel (asset<->BTC). Set to a real asset id for a truthful asset<->asset market. Put the NUMERAIRE on the QUOTE side so the market key matches the wallet's canonicalPair (e.g. base=OILX -quote-asset=EURX, i.e. OILX priced in EURX) — the wallet queries <base>/<quote> in exactly that orientation, so a reversed pair rests in a market the wallet never reads. Defaults -btc-asset to the quote id so settlement routes over it")
	holdTimeout := flag.Duration("hold-timeout", 2*time.Minute, "pureln: how long the maker waits for the taker to lock its hold and then fulfills before giving up")
	requote := flag.Bool("requote", false, "cross/lightning/pureln: after each settled fill, reconnect dropped channel peers and re-post a FRESH offer (same offer id) instead of cancelling and exiting; keeps a live quote without a manual restart (default off = quote once then exit)")
	flag.Parse()
	// Secrets and RPC URLs may come from the environment instead of the command line,
	// where `ps` shows them to every user on the box: SEQOB_MAKER_PRIV, SEQOB_BTC_RPC,
	// SEQOB_XSEQ_RPC, SEQOB_NODE_RPC. A flag given explicitly wins.
	envDefault(makerPriv, "SEQOB_MAKER_PRIV")
	envDefault(btcRPCURL, "SEQOB_BTC_RPC")
	envDefault(xseqRPCURL, "SEQOB_XSEQ_RPC")
	envDefault(nodeRPC, "SEQOB_NODE_RPC")
	// A reprice must never kill this process mid-swap; drain first (see armGracefulShutdown).
	armGracefulShutdown(15 * time.Minute)

	// Cross resume needs no maker key or offer: it drives on-chain settlement
	// from persisted per-session keys. Handle it before the key/offer setup.
	if *resume {
		// The sub-asset BUY claim key is the identity key; a refund key is persisted
		// per session. Only a key given explicitly is used here, never a generated one.
		if *makerPriv != "" {
			resumeIdentity = loadOrGenKey(*makerPriv)
		}
		resumeCrossSessions(*xstateDir, *btcRPCURL, *btcWallet, *btcChainName, *xseqRPCURL, *xseqWallet, *spendFee, *btcFeeRate, *lnSocket)
		return
	}

	cross := strings.ToLower(*mode) == "cross"
	lightning := strings.ToLower(*mode) == "lightning"
	pureln := strings.ToLower(*mode) == "pureln"
	subasset := strings.ToLower(*mode) == "subasset"
	subassetSell := strings.ToLower(*mode) == "subasset-sell"
	if !cross && !lightning && !pureln && !subasset && !subassetSell && *account == "" {
		fatal("-account is required (the Ocean account holding the offer asset)")
	}

	maxQuoteAge = *quoteAge
	makerKey := loadOrGenKey(*makerPriv)
	makerPubHex := hex.EncodeToString(makerKey.PubKey().SerializeCompressed())
	ctx := context.Background()

	if subassetSell {
		runSubAssetSellMaker(subAssetSellMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			// Mixed same-chain (rails 7/8): -quote-asset makes the on-chain leg an
			// HTLC on that Sequentia asset (via -xseq-rpc/-xseq-wallet), no bitcoind.
			quoteAsset: *quoteAsset, seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet,
			assetLnSock: *assetLnSocket, btcDelta: uint32(*btcDelta), minBTCConf: *minBTCConf,
			spendFee: *spendFee, holdTimeout: *holdTimeout, requote: *requote,
			stateDir: *xstateDir,
		})
		return
	}

	if subasset {
		runSubAssetMaker(subAssetMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			// Mixed same-chain (rails 7/8): -quote-asset makes the on-chain leg an
			// HTLC on that Sequentia asset (via -xseq-rpc/-xseq-wallet), no bitcoind.
			quoteAsset: *quoteAsset, seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet,
			assetLnSock: *assetLnSocket, btcDelta: uint32(*btcDelta), minBTCConf: *minBTCConf,
			spendFee: *spendFee, holdTimeout: *holdTimeout, requote: *requote,
			stateDir: *xstateDir,
		})
		return
	}

	if pureln {
		// asset<->asset: -quote-asset advertises the true quote AND (unless -btc-asset is set
		// explicitly) routes the settlement counter-leg over that same asset, so one flag makes a
		// truthful OILX/EURX market (base=commodity, quote=numeraire) instead of the old BTC-sentinel
		// disguise. Keep the numeraire as the quote so the market key matches the wallet's canonicalPair.
		effBtcAsset := *btcAsset
		if effBtcAsset == "" {
			effBtcAsset = *quoteAsset
		}
		runPureLNMaker(pureLNMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			assetLnSock: *assetLnSocket, btcLnSock: *lnSocket, btcAsset: effBtcAsset, quoteAsset: *quoteAsset,
			holdTimeout: *holdTimeout, onchainCltv: uint32(*onchainCltv),
			reverse: strings.ToLower(*side) == "sell", // sell = maker gives the asset (holds BTC); buy = maker acquires (holds asset)
			requote: *requote,
		})
		return
	}

	if lightning {
		runSubmarineMaker(submarineMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet, lnSocket: *lnSocket,
			seqDelta: uint32(*seqDelta), subAnchor: *subAnchor, onchainCltv: uint32(*onchainCltv),
			spendFee: *spendFee, max0conf: *max0conf,
			reverse:  strings.ToLower(*side) == "sell", // sell = maker-secret REVERSE; buy = NORMAL
			requote:  *requote,
			stateDir: *xstateDir,
		})
		return
	}

	if cross {
		runCrossMaker(crossMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			asset: *base, side: *side, assetAmt: *baseAmt, btcAmt: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet,
			btcDelta: uint32(*btcDelta), seqDelta: uint32(*seqDelta),
			minBTCConf: *minBTCConf, spendFee: *spendFee, stateDir: *xstateDir,
			btcFeeRate: *btcFeeRate,
			requote:    *requote,
		})
		return
	}

	// Reuse the proven Ocean settlement exactly like the daemon.
	w, err := oceanwallet.NewService(*ocean)
	if err != nil {
		fatal("connect ocean wallet %q: %v", *ocean, err)
	}
	svc, err := application.NewWalletService(w, *nodeRPC)
	if err != nil {
		fatal("wallet service: %v", err)
	}
	defer svc.Close()
	net := svc.Network()

	// Derive the maker's receive address; publish its blinding pubkey only for a
	// confidential offer so the taker mirrors the maker's confidentiality posture.
	addrs, err := svc.Account().DeriveAddresses(ctx, *account, 1)
	if err != nil || len(addrs) == 0 {
		fatal("derive recv address for account %q: %v", *account, err)
	}
	recvAddr := addrs[0]
	blindingPub := ""
	if *confidential {
		info, err := seqnet.FromConfidential(recvAddr, &net)
		if err != nil {
			fatal("parse recv address: %v", err)
		}
		blindingPub = hex.EncodeToString(info.BlindingKey)
	}

	// The ladder: level 0 is exactly the classic single offer; each deeper level
	// rests further from the touch (SELL asks more quote, BUY bids less) and
	// larger. Every level is a REAL offer of this maker — the co-sign path serves
	// whichever level a taker lifts, funded from the same account — so ladder
	// depth is never ghost depth (the relay hides interactive offers whose maker
	// cannot co-sign; these all can).
	if *levels < 1 {
		fatal("-levels must be >= 1")
	}
	if *levelBps < 0 || *levels*(*levelBps) >= 10000 {
		fatal("-level-bps must be >= 0 and -levels x -level-bps below 10000 (a negative bps makes deeper SELL rungs cheaper; past 100%% a BUY rung prices below zero)")
	}
	if *levelGrowth < 1 {
		fatal("-level-growth must be >= 1 (a shrinking rung rounds to a size the relay refuses)")
	}
	ladder := make([]*seqobv1.Offer, 0, *levels)
	for i := 0; i < *levels; i++ {
		grow := math.Pow(*levelGrowth, float64(i))
		lb := uint64(math.Round(float64(*baseAmt) * grow))
		lq := float64(*quoteAmt) * grow
		adj := float64(i*(*levelBps)) / 10000.0
		if strings.ToLower(*side) == "sell" {
			lq *= 1 + adj
		} else {
			lq *= 1 - adj
		}
		id := *offerID
		if id != "" && i > 0 {
			id = fmt.Sprintf("%s-l%d", id, i)
		}
		lo := buildOffer(*base, *quote, *side, lb, uint64(math.Round(lq)), *feeAsset,
			*expiry, uint32(*minAnchor), recvAddr, blindingPub, id)
		// Post into the SEPARATE blinded book when confidential: the signed namespace tag
		// segregates this offer so it only crosses another confidential offer (both legs
		// blind on-chain). recvAddr is already the blech32 confidential form here (its
		// blinding pubkey was just parsed), satisfying the relay's confidential-offer rules.
		lo.Confidential = *confidential
		if err := offer.SignOffer(lo, makerKey); err != nil {
			fatal("sign offer: %v", err)
		}
		ladder = append(ladder, lo)
	}
	// A ladder advertises the SUM of its rungs, all funded from one account: ten
	// rungs at 12% growth sum to 17.5x the base size. Depth the account cannot fund is
	// phantom depth — the lift fails in coin selection after the taker built and
	// couriered its PSET, which the relay's ghost filter cannot see. Trim the deepest
	// rungs to what the account holds; refuse outright if even the first does not fit.
	ladder = trimLadderToBalance(ctx, svc, *account, ladder)

	// Maker-only backend: the LiveWallet only calls ResponderComplete, which uses
	// CompleteSwapFn. Wire it to the blind-aware CompleteSwap; the taker-side seams
	// are unused here (dummy key, never exercised).
	rb := client.NewRealBackend(&net, makerKey.Serialize(), makerKey.Serialize())
	// Every co-sign goes through the spent-coin guard: coins our own swaps spend are
	// locked again first, so the wallet never hands one back while it still lists it.
	guard := newSpentGuard()
	coinSpent := nodeCoinSpent(*nodeRPC)
	rb.CompleteSwapFn = func(req *seqdexv1.SwapRequest, blind bool) (string, []swap.UnblindedInput, error) {
		signedPSET, utxos, err := guardedCompleteSwap(guard,
			func(ctx context.Context, us []ports.Utxo) error {
				return svc.Transaction().LockUtxos(ctx, *account, us)
			},
			func() (string, []ports.Utxo, error) {
				signed, us, _, err := svc.CompleteSwap(*account, swapReqAdapter{req}, *msats, true, blind)
				return signed, us, err
			}, coinSpent)
		if err != nil {
			return "", nil, err
		}
		return signedPSET, utxosToSwapUnblinded(utxos), nil
	}
	wallet := &client.LiveWallet{Backend: rb, MakerOutputsConfidential: *confidential, RequireConfidential: *confidential}
	// One Maker binding per ladder level: HandleRequest binds every co-sign to the
	// LIFTED offer (asset legs, price floor, remaining size) so a malicious taker
	// cannot drain the maker at another level's price.
	makers := make(map[string]*client.Maker, len(ladder))
	for _, lo := range ladder {
		makers[lo.GetOfferId()] = &client.Maker{Wallet: wallet, Offer: lo}
	}

	// Connect, submit the offers (this registers the conn for live lifts), then
	// serve lifts until killed.
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	defer conn.Close()

	for i, lo := range ladder {
		if err := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: lo}}); err != nil {
			fatal("post offer: %v", err)
		}
		if i > 0 && i%4 == 0 {
			time.Sleep(500 * time.Millisecond) // pace a deep ladder under the relay's submit throttle
		}
	}
	o := ladder[0]
	fmt.Printf("seqob-maker up: posted %s offer %s (+%d ladder levels) by maker %s\n", *side, o.GetOfferId(), len(ladder)-1, makerPubHex)
	fmt.Printf("  pair %s/%s  give %d %s  want %d %s  confidential=%v  fee-rate=%d msat/vB\n",
		o.GetPair().GetBaseAsset(), o.GetPair().GetQuoteAsset(), o.GetOfferAmount(), o.GetOfferAsset(), o.GetWantAmount(), o.GetWantAsset(), *confidential, *msats)
	fmt.Printf("  taker lifts with: -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), makerPubHex)

	serve(conn, makers, ladder, makerKey, wsURL, *expiry, guard)
}

// serve is the maker's single-goroutine event loop: derive a per-lift E2E key on
// lift_requested, then on the taker's couriered SwapRequest run the responder and
// courier back the SwapAccept. A later swap_msg for the same session is the
// taker's SwapComplete (the swap settled).
func serve(conn *websocket.Conn, makers map[string]*client.Maker, ladder []*seqobv1.Offer, makerKey *btcec.PrivateKey, wsURL string, expiry time.Duration, guard *spentGuard) {
	crypters := make(map[string]*client.Crypter)
	sessionCoins := make(map[string][]ports.Utxo) // session -> the coins its co-sign spends
	accepted := make(map[string]bool)
	// What "busy" means for this loop: a lift we have accepted and not yet settled.
	// The shutdown drain reads this rather than guessing from log silence — a maker
	// waiting on its counterparty is silent precisely when it must NOT be killed.
	var serveMu sync.Mutex
	setBusyFn(func() int { serveMu.Lock(); defer serveMu.Unlock(); return len(accepted) })
	takes := make(map[string]uint64)                    // session -> take_amount, for the settle-ack fill_base (B-1)
	sessionMaker := make(map[string]*client.Maker)      // session -> the ladder level it lifted
	o := ladder[0]                                      // every level shares one expiry window; level 0 represents it
	// Exit-for-requote (see requote_exit.go). A co-sign session quiet for 10
	// minutes is treated as abandoned (taker vanished after SwapAccept) so a
	// stuck session can never pin a stale quote on the book forever.
	var liveSessions, lastAccept atomic.Int64
	idle := func() bool {
		return liveSessions.Load() == 0 || time.Since(time.Unix(lastAccept.Load(), 0)) > 10*time.Minute
	}
	armRequoteExit(o, idle)
	// Reconnect resilience + STAY on the book. Exiting on a WS error lets the supervisor restart us,
	// which mints a NEW offer_id → a taker who lifted the old id races a replaced offer and gets "no
	// maker co-sign". So we reconnect and re-post the SAME offer_id (refreshed window + re-sign). Two
	// hardening rules the naive version missed: (1) re-posts are RATE-LIMITED — the relay validates a
	// submit ASYNC and rejects (rate limit / expired) with a From_Error, keeping the conn open; an
	// unthrottled recovery loop would exhaust the per-maker rate budget and leave us silently off-book
	// (blocked on ReadMessage with no live offer). (2) reconnects use an ESCALATING backoff so a
	// reconnect-succeeds-then-immediately-drops cycle can't spin the CPU / hammer a struggling relay.
	var lastPost time.Time
	const minRepostGap = 20 * time.Second
	repost := func(c *websocket.Conn, reason string) bool {
		if time.Since(lastPost) < minRepostGap {
			return false
		}
		for i, lo := range ladder {
			if e := refreshOfferForRequote(lo, expiry, makerKey); e != nil {
				fmt.Printf("re-post (%s): re-sign failed: %v\n", reason, e)
				return false
			}
			if we := writeWS(c, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: lo}}); we != nil {
				return false
			}
			if i > 0 && i%4 == 0 {
				time.Sleep(500 * time.Millisecond) // pace a deep ladder under the relay's submit throttle
			}
		}
		lastPost = time.Now()
		fmt.Printf("re-posted offer %s (+%d ladder levels) (%s)\n", o.GetOfferId(), len(ladder)-1, reason)
		return true
	}
	backoff := time.Second
	redial := func(old *websocket.Conn) *websocket.Conn {
		old.Close()
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		for {
			nc, _, de := websocket.DefaultDialer.Dial(wsURL, nil)
			if de != nil {
				time.Sleep(backoff)
				continue
			}
			lastPost = time.Time{} // a reconnect genuinely evicted our offer — force the re-post past the rate gate
			if !repost(nc, "reconnect") {
				nc.Close()
				time.Sleep(backoff)
				continue
			}
			// RE-ATTACH EVERY IN-FLIGHT SESSION, exactly as serveCross does.
			//
			// Re-posting the offer restores this connection as the route for NEW lifts and
			// says nothing about lifts already running. On the relay a role's courier frames
			// are delivered by a pump started in attach(), and that pump dies with its
			// connection; the fresh one carries no role binding, so we can neither receive
			// the counterparty's frames nor send our own (wsSwapMsg answers 403 "not a
			// participant"). The session is simply orphaned, in both directions.
			//
			// That is what breaks a submarine take across ordinary maker churn: we lock the
			// asset on-chain, our socket blips, we re-post — and the taker never receives the
			// leg it is waiting for. It waits out its timeout while our asset sits locked
			// until T_seq. Nothing about the parent chain is involved; the trade fails for a
			// courier reason alone, which is exactly the case where the DEX must keep working.
			for sid := range accepted {
				if we := writeWS(nc, &seqobv1.To{Msg: &seqobv1.To_SessionReattach{
					SessionReattach: &seqobv1.SessionReattach{
						SessionId: sid,
						Role:      "maker",
						Sig:       offer.SignReattach(sid, "maker", makerKey),
					}}}); we != nil {
					fmt.Printf("reconnect: re-attach of session %s FAILED: %v"+
						" (its courier frames are stranded; the lift will time out)\n", sid, we)
					continue
				}
				fmt.Printf("reconnect: re-attached session %s; couriered frames resume\n", sid)
			}
			return nc
		}
	}
	for {
		var from seqobv1.From
		_, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("ws read: %v; reconnecting + re-posting\n", err)
			conn = redial(conn)
			continue
		}
		backoff = time.Second // a healthy read: the connection is good, reset the reconnect backoff
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			cr, err := client.NewMakerCrypterFromLift(makerKey, lr.GetTakerSessionPubkey())
			if err != nil {
				fmt.Printf("lift %s: crypter error: %v\n", lr.GetSessionId(), err)
				continue
			}
			// The relay routes lifts to this maker by PUBKEY, and a stable identity key can be
			// shared by sibling processes; a lift for an offer this process never posted must not
			// be negotiated with OUR offer's terms. Same-chain lifts commit nothing before the
			// co-sign, so skipping registration is a safe refusal — the taker's request finds no
			// session and times out on its own short deadline.
			lm := makers[lr.GetOfferId()]
			if lm == nil {
				fmt.Printf("lift %s: for offer %s, but this process serves %s (+%d ladder levels); ignoring (sibling maker owns it)\n",
					lr.GetSessionId(), lr.GetOfferId(), o.GetOfferId(), len(ladder)-1)
				continue
			}
			sessionMaker[lr.GetSessionId()] = lm
			crypters[lr.GetSessionId()] = cr
			takes[lr.GetSessionId()] = lr.GetTakeAmount()
			fmt.Printf("lift requested: session %s offer %s take %d\n",
				lr.GetSessionId(), lr.GetOfferId(), lr.GetTakeAmount())

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			sid := sm.GetSessionId()
			sm2 := sessionMaker[sid]
			if accepted[sid] && sm2 != nil {
				// The taker couriered SwapComplete: the same-chain lift settled on-chain. Ack the relay
				// so it records the trade + decrements this resting order — the relay never parses the
				// opaque courier, so without this ack the order lingers as a ghost and the trades feed
				// stays empty (B-1). Best-effort txid from the completed tx; fill_base is the lift's take.
				txid, _ := sm2.HandleComplete(sm.GetCiphertext(), crypters[sid])
				// anchor_confs = the offer's own min_anchor_depth: the maker declares its finality
				// bar met at settlement, so a min_anchor_depth>0 offer reaches FILLED (default 0 keeps
				// the 0-conf-tolerant behavior). Previously the relay hard-coded 0, so >0 never finalized.
				if we := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
					SessionId: sid, SettleTxid: txid, FillBase: takes[sid], AnchorConfs: sm2.Offer.GetMinAnchorDepth(),
				}}}); we != nil {
					fmt.Printf("session %s: settle-ack write failed: %v; reconnecting\n", sid, we)
					conn = redial(conn)
				}
				fmt.Printf("session %s: SWAP SETTLED (acked relay; txid=%s)\n", sid, txid)
				// Its coins are spent now: held until the wallet has long seen the spend.
				guard.hold(sessionCoins[sid], settledHold)
				delete(sessionCoins, sid)
				delete(accepted, sid) // B3: prune per-session state now the swap is done (serve() runs forever)
				delete(crypters, sid)
				delete(takes, sid)
				delete(sessionMaker, sid)
				liveSessions.Add(-1)
				requoteExitIfPending(idle)
				continue
			}
			cr := crypters[sid]
			if cr == nil || sm2 == nil {
				fmt.Printf("session %s: swap_msg before lift_requested; ignoring\n", sid)
				continue
			}
			sealedAccept, err := sm2.HandleRequest(sm.GetCiphertext(), cr)
			if err != nil {
				fmt.Printf("session %s: complete swap failed: %v\n", sid, err)
				continue
			}
			if we := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealedAccept}}}); we != nil {
				fmt.Printf("session %s: co-sign write failed: %v; reconnecting\n", sid, we)
				conn = redial(conn)
				continue
			}
			accepted[sid] = true
			sessionCoins[sid] = guard.takeLast()
			liveSessions.Add(1)
			lastAccept.Store(time.Now().Unix())
			fmt.Printf("session %s: couriered SwapAccept (%d bytes); awaiting taker broadcast\n", sid, len(sealedAccept))

		case from.GetOrderStatus() != nil:
			st := from.GetOrderStatus()
			fmt.Printf("order %s status=%s active=%d txid=%s\n",
				st.GetOfferId(), st.GetStatus(), st.GetActiveAmount(), st.GetSettleTxid())
			// One of our rungs left the book: filled whole, or dropped because a co-sign
			// failed. Post it again, fresh, so the book keeps this maker's quote instead of
			// waiting for a restart; a lift is co-signed only from coins still spendable.
			if lm := makers[st.GetOfferId()]; lm != nil && offerLeftBook(st.GetStatus()) {
				repostOne(conn, lm.Offer, expiry, makerKey, "after "+st.GetStatus().String())
			}

		case from.GetError() != nil:
			e := from.GetError()
			fmt.Printf("relay error %d: %s\n", e.GetCode(), e.GetMessage())
			// B1: a relay error can mean our offer was rejected/evicted (expired, bad window) and is
			// now off-book — the relay validates a submit ASYNC and reports rejection here, not as
			// a write error. Re-post on THAT (rate-limited), never on every error frame: a re-post
			// re-signs every ladder level and each one is charged against the per-pubkey budget,
			// so re-posting on a benign 409 or on a rate-limit refusal turned one error into a
			// storm and then into a restart loop.
			if offerRejected(e.GetCode(), e.GetMessage()) {
				if !repost(conn, "after relay rejection") {
					requoteExitIfIdle("relay rejected our offer ("+e.GetMessage()+")", idle)
				}
			}
		}
	}
}

// offerLeftBook: statuses after which the relay no longer rests the offer and this
// maker should post it again.
func offerLeftBook(st seqobv1.OfferStatus) bool {
	return st == seqobv1.OfferStatus_OFFER_STATUS_FILLED || st == seqobv1.OfferStatus_OFFER_STATUS_UTXO_INVALIDATED
}

// repostOne posts one offer again with a fresh window and signature.
func repostOne(c *websocket.Conn, lo *seqobv1.Offer, expiry time.Duration, key *btcec.PrivateKey, reason string) {
	if err := refreshOfferForRequote(lo, expiry, key); err != nil {
		fmt.Printf("re-post %s (%s): re-sign failed: %v\n", lo.GetOfferId(), reason, err)
		return
	}
	if err := writeWS(c, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: lo}}); err != nil {
		fmt.Printf("re-post %s (%s): write failed: %v\n", lo.GetOfferId(), reason, err)
		return
	}
	fmt.Printf("re-posted offer %s (%s)\n", lo.GetOfferId(), reason)
}

func buildOffer(base, quote, side string, baseAmt, quoteAmt uint64, feeAsset string,
	expiry time.Duration, minAnchor uint32, recvAddr, blindingPub, id string) *seqobv1.Offer {
	o := &seqobv1.Offer{
		OfferId:        orDefault(id, randstr.Hex(16)),
		SchemaVersion:  1,
		Pair:           &seqobv1.AssetPair{BaseAsset: base, QuoteAsset: quote},
		BaseAmount:     baseAmt,
		AllowPartial:   true,
		CreatedAtUnix:  uint64(time.Now().Unix()),
		ExpiresAtUnix:  uint64(time.Now().Add(expiry).Unix()),
		FeeAssetHint:   feeAsset,
		MinAnchorDepth: minAnchor,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{
			MakerRecvAddress: recvAddr,
			MakerBlindingPub: blindingPub,
		}},
	}
	switch strings.ToLower(side) {
	case "sell":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = base, baseAmt
		o.WantAsset, o.WantAmount = quote, quoteAmt
	case "buy":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = quote, quoteAmt
		o.WantAsset, o.WantAmount = base, baseAmt
	default:
		fatal("side must be sell or buy")
	}
	return o
}

// buildCrossOffer builds a CROSS-CHAIN (BTC<->asset) order-book offer: pair is
// base=asset, quote=the BTC sentinel. The resting CrossChainTerms keys/locktime are
// ADVISORY (display + a stable signed commitment from the maker identity key); the
// load-bearing HTLC keys and CLTVs are minted per-lift over the E2E courier (Phase 2).
// A SELL gives the asset for BTC (taker pays BTC; direction BTC_TO_ASSET); a BUY gives
// BTC for the asset (taker sells the asset; direction ASSET_TO_BTC).
func buildCrossOffer(asset, side string, assetAmt, btcAmt, spendFee uint64, feeAsset string,
	expiry time.Duration, minAnchor uint32, recvAddr, makerPubHex, id string) *seqobv1.Offer {
	isSell := strings.ToLower(side) == "sell"
	direction := offer.DirAssetToBTC
	if isSell {
		direction = offer.DirBTCToAsset
	}
	o := &seqobv1.Offer{
		OfferId:       orDefault(id, randstr.Hex(16)),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: asset, QuoteAsset: offer.BTCSentinel},
		BaseAmount:    assetAmt,
		AllowPartial:  true, // a taker may take a slice; the serve loop re-rests the remainder
		// min_fill = the smallest base slice whose BTC leg still clears the dust+fee
		// floor, so the book never advertises a partial that would strand a sub-dust
		// HTLC leg. Clamped to <= base_amount (validator requires it); when even the
		// whole offer barely clears the floor it equals base_amount (whole take only).
		MinFill:        client.MinFillBase(btcAmt, assetAmt, spendFee),
		CreatedAtUnix:  uint64(time.Now().Unix()),
		ExpiresAtUnix:  uint64(time.Now().Add(expiry).Unix()),
		FeeAssetHint:   feeAsset,
		MinAnchorDepth: minAnchor,
		Settlement: &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{
			BtcSentinel:      offer.BTCSentinel,
			MakerRecvAddress: recvAddr,
			MakerClaimPub:    makerPubHex,
			MakerRefundPub:   makerPubHex,
			MakerLegLocktime: 144,
			Direction:        direction,
		}},
	}
	if isSell {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = asset, assetAmt
		o.WantAsset, o.WantAmount = offer.BTCSentinel, btcAmt
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = offer.BTCSentinel, btcAmt
		o.WantAsset, o.WantAmount = asset, assetAmt
	}
	return o
}

// crossReRestRemainder decides how a settled cross fill re-rests, enforcing the
// minimum-slice floor on the REMAINDER (the covenant CLOB's 'remainder == 0 OR
// remainder >= min_fill'). Given the offer's base_amount, the base atoms actually
// filled, and the offer's min_fill, it returns the remainder to re-rest and
// whether to re-rest at all. reRest is false for a whole fill (nothing left) OR a
// fill that would leave a sub-min_fill dust remainder — in both cases the caller
// drops to a full fill (retire / -requote) rather than re-resting an order whose
// only fills would strand sub-dust HTLC legs. A min_fill of 0 (unset) imposes no
// remainder floor: any non-empty remainder re-rests.
func crossReRestRemainder(base, filledSeq, minFill uint64) (remainder uint64, reRest bool) {
	if filledSeq == 0 || filledSeq >= base {
		return 0, false // whole fill (or nonsensical over-fill): nothing to re-rest
	}
	remainder = base - filledSeq
	if minFill > 0 && remainder < minFill {
		return remainder, false // sub-min_fill dust remainder: drop to a full fill
	}
	return remainder, true
}

// resumeCrossSessions finishes every non-terminal cross session persisted in
// dir after a restart: it reconstructs the legs/keys from each <sid>.json and
// re-enters the on-chain settle loop (claim on the taker's reveal, or refund
// after the CLTV). This is the 2f recovery path — a mid-swap crash or courier
// timeout no longer strands the maker's asset leg. FORWARD sessions only for
// now (the direction served today); reverse resume lands with reverse serving.
func resumeCrossSessions(dir, btcRPCURL, btcWallet, btcChainName, seqRPCURL, seqWallet string, spendFee uint64, btcFeeRate float64, lnSocket string) {
	if btcRPCURL == "" && seqRPCURL == "" {
		fatal("-resume requires -btc-rpc and/or -xseq-rpc (the chains the persisted legs live on)")
	}
	var btcChain *xchain.BitcoinChain
	var seqChain *xchain.Chain
	if btcRPCURL != "" {
		btcRPC, err := rpcFromURL(btcRPCURL)
		if err != nil {
			fatal("-btc-rpc: %v", err)
		}
		params, err := xchain.BitcoinChainParams(btcChainName)
		if err != nil {
			fatal("-btc-chain: %v", err)
		}
		btcChain = xchain.NewBitcoinChain(btcRPC, btcWallet, params)
		btcChain.SetFeeRate(btcFeeRate)
	}
	if seqRPCURL != "" {
		seqRPC, err := rpcFromURL(seqRPCURL)
		if err != nil {
			fatal("-xseq-rpc: %v", err)
		}
		seqChain = xchain.NewChain(seqRPC, seqWallet)
	}
	drivePendingCrossSessions(dir, btcChain, seqChain, spendFee, lnSocket)
}

// resumeIdentity is the -maker-priv key handed to -resume for directions whose
// claim key is the identity key (sub-asset BUY). nil when not given.
var resumeIdentity *btcec.PrivateKey

func resumeIdentityKey() *xchain.Key {
	if resumeIdentity == nil {
		return nil
	}
	return xchain.KeyFromBytes(resumeIdentity.Serialize())
}

// drivePendingCrossSessions scans dir for non-terminal cross sessions and re-drives each to settlement
// (claim on the counterparty's reveal, or refund after CLTV), each in its own goroutine, then waits. Shared
// core of BOTH the explicit -resume one-shot AND the automatic resume pass every serve startup runs — so a
// supervised restart no longer strands a pending session (whose asset HTLC otherwise never gets claimed and
// the counterparty never learns P). Non-fatal: an unreadable dir/record is logged and skipped, never killing
// the caller — the live serving maker depends on that.
func drivePendingCrossSessions(dir string, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain, spendFee uint64, lnSocket string) {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		fmt.Printf("resume: read -xstate-dir %s: %v (nothing to resume)\n", dir, err)
		return
	}
	var pending []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			pending = append(pending, e.Name())
		}
	}
	if len(pending) == 0 {
		fmt.Printf("no cross sessions to resume in %s\n", dir)
		return
	}
	fmt.Printf("resuming %d cross session(s) from %s\n", len(pending), dir)

	var wg sync.WaitGroup
	for _, name := range pending {
		path := filepath.Join(dir, name)
		raw, rerr := ioutil.ReadFile(path)
		if rerr != nil {
			fmt.Printf("%s: read: %v\n", name, rerr)
			continue
		}
		var st xmakerSessionState
		if jerr := json.Unmarshal(raw, &st); jerr != nil {
			fmt.Printf("%s: parse: %v\n", name, jerr)
			continue
		}
		if st.Settled || st.SeqRefundTx != "" || st.BtcRefundTx != "" {
			fmt.Printf("%s: already terminal (settled=%v seqrefund=%s btcrefund=%s); skipping\n", name, st.Settled, st.SeqRefundTx, st.BtcRefundTx)
			continue
		}
		if st.Direction == "submarine" {
			// A submarine maker's lift, persisted at terms/leg/pay time. Everything here is
			// recovery: claim with the persisted P if the pay landed but the claim did not;
			// re-run the full verify->anchor-gate->pay->claim when the leg is known, the
			// invoice is persisted and an LN socket was provided (CLN pay is idempotent by
			// payment_hash, so a re-pay of an already-paid invoice just returns P); and
			// DISCOVER an announced leg the maker process never recorded, script-first,
			// exactly like the reverse discovery above it in this file.
			wg.Add(1)
			go func(name string, st xmakerSessionState) {
				defer wg.Done()
				resumeSubmarineSession(&st, seqChain, lnSocket, spendFee, dir, name)
			}(name, st)
			continue
		}
		if st.Direction == "submarine-reverse" || st.Direction == "subasset-sell" || st.Direction == "subasset" {
			wg.Add(1)
			go func(name string, st xmakerSessionState) {
				defer wg.Done()
				resumeTimelockSession(&st, btcChain, seqChain, spendFee, dir, name)
			}(name, st)
			continue
		}
		if btcChain == nil || seqChain == nil {
			fmt.Printf("%s: a %q session needs both -btc-rpc and -xseq-rpc to resume; skipping\n", name, st.Direction)
			continue
		}
		if st.Direction == "reverse" {
			if st.BtcLegTxid == "" || st.BtcLegScriptHex == "" || st.BtcRefundPrivHex == "" || st.SecretHex == "" {
				fmt.Printf("%s: reverse session with no funded BTC leg / secret to recover; skipping\n", name)
				continue
			}
			rp, rerr := resumeParamsFromStateReverse(&st, btcChain, seqChain, spendFee, dir)
			if rerr != nil {
				fmt.Printf("%s: reverse reconstruct: %v\n", name, rerr)
				continue
			}
			wg.Add(1)
			go func(name string, rp client.MakerReverseResumeParams) {
				defer wg.Done()
				fmt.Printf("%s: resuming reverse session (claim asset if funded, else refund BTC after T_btc)\n", name)
				res, rerr := client.ResumeMakerReverse(rp)
				if rerr != nil {
					fmt.Printf("%s: reverse resume ended: %v\n", name, rerr)
					if res != nil && res.BtcRefundTx != "" {
						fmt.Printf("%s: BTC leg refunded in %s\n", name, res.BtcRefundTx)
					}
					return
				}
				fmt.Printf("%s: REVERSE RESUMED + SETTLED: claimed the asset in %s\n", name, res.SeqClaimTxid)
			}(name, rp)
			continue
		}
		if st.SeqLegTxid == "" || st.SeqLegScriptHex == "" || st.BtcClaimPrivHex == "" || st.SeqRefundPrivHex == "" {
			fmt.Printf("%s: no locked SEQ leg / keys to resume (session died before lock); nothing on-chain to settle\n", name)
			continue
		}
		p, perr := resumeParamsFromState(&st, btcChain, seqChain, spendFee, dir)
		if perr != nil {
			fmt.Printf("%s: reconstruct: %v\n", name, perr)
			continue
		}
		wg.Add(1)
		go func(name string, p client.MakerForwardResumeParams) {
			defer wg.Done()
			fmt.Printf("%s: resuming on-chain settle loop\n", name)
			res, rerr := client.ResumeMakerForward(p)
			if rerr != nil {
				fmt.Printf("%s: resume ended: %v\n", name, rerr)
				if res != nil && res.SeqRefundTx != "" {
					fmt.Printf("%s: SEQ leg refunded in %s\n", name, res.SeqRefundTx)
				}
				return
			}
			fmt.Printf("%s: RESUMED + SETTLED: BTC claimed in %s\n", name, res.BtcClaimTxid)
		}(name, p)
	}
	wg.Wait()
	fmt.Println("resume pass complete")
}

// resumeParamsFromState rebuilds the resume params (legs, keys, swap) from a
// persisted session record.
func resumeParamsFromState(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain,
	spendFee uint64, dir string) (client.MakerForwardResumeParams, error) {
	var zero client.MakerForwardResumeParams
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil || len(hashH) != 32 {
		return zero, fmt.Errorf("bad hash_hex")
	}
	btcClaimBytes, err := hex.DecodeString(st.BtcClaimPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_claim_priv_hex")
	}
	seqRefundBytes, err := hex.DecodeString(st.SeqRefundPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_refund_priv_hex")
	}
	btcScript, err := hex.DecodeString(st.BtcLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_leg_script_hex")
	}
	seqScript, err := hex.DecodeString(st.SeqLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_leg_script_hex")
	}
	sid := st.SessionID
	return client.MakerForwardResumeParams{
		Ops: &client.LiveXcOps{
			Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(hashH)),
			BTC:  btcChain, SEQ: seqChain,
		},
		BtcLeg: &xchain.LegLock{
			Script:   btcScript,
			Funded:   &xchain.FundedHTLC{TxID: st.BtcLegTxid, Vout: st.BtcLegVout, Amount: st.BtcLegAmount},
			Locktime: st.BtcLocktime,
		},
		SeqLeg: &xchain.LegLock{
			Script:   seqScript,
			Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
			Locktime: st.SeqLocktime,
		},
		BtcClaimKey:  xchain.KeyFromBytes(btcClaimBytes),
		SeqRefundKey: xchain.KeyFromBytes(seqRefundBytes),
		HashH:        hashH,
		BtcLocktime:  st.BtcLocktime,
		SeqLocktime:  st.SeqLocktime,
		AssetHex:     st.SeqLegAsset,
		BtcAmount:    st.BtcLegAmount,
		SeqAmount:    st.SeqLegAmount,
		SpendFeeSats: spendFee,
		OnUpdate: func(r *client.MakerForwardResult) {
			persistXSession(dir, sid, st.OfferID, r)
		},
		Log: func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) },
	}, nil
}

// resumeParamsFromStateReverse rebuilds a reverse resume from a persisted record.
// The taker's asset leg may be absent (taker never funded) — resume then just
// refunds our BTC leg after T_btc.
func resumeParamsFromStateReverse(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain,
	spendFee uint64, dir string) (client.MakerReverseResumeParams, error) {
	var zero client.MakerReverseResumeParams
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil || len(hashH) != 32 {
		return zero, fmt.Errorf("bad hash_hex")
	}
	secret, err := hex.DecodeString(st.SecretHex)
	if err != nil || len(secret) != 32 {
		return zero, fmt.Errorf("bad secret_hex")
	}
	seqClaimBytes, err := hex.DecodeString(st.SeqClaimPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_claim_priv_hex")
	}
	btcRefundBytes, err := hex.DecodeString(st.BtcRefundPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_refund_priv_hex")
	}
	btcScript, err := hex.DecodeString(st.BtcLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_leg_script_hex")
	}
	sid := st.SessionID
	rp := client.MakerReverseResumeParams{
		// The maker holds the secret, so the ops swap is built from it.
		Ops: &client.LiveXcOps{
			Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLock(secret)),
			BTC:  btcChain, SEQ: seqChain,
		},
		BtcLeg: &xchain.LegLock{
			Script:   btcScript,
			Funded:   &xchain.FundedHTLC{TxID: st.BtcLegTxid, Vout: st.BtcLegVout, Amount: st.BtcLegAmount},
			Locktime: st.BtcLocktime,
		},
		SeqBlockHash: st.SeqBlockHash,
		Secret:       secret,
		HashH:        hashH,
		SeqClaimKey:  xchain.KeyFromBytes(seqClaimBytes),
		BtcRefundKey: xchain.KeyFromBytes(btcRefundBytes),
		BtcLocktime:  st.BtcLocktime,
		SeqLocktime:  st.SeqLocktime,
		AssetHex:     st.SeqLegAsset,
		BtcAmount:    st.BtcLegAmount,
		SeqAmount:    st.SeqLegAmount,
		SpendFeeSats: spendFee,
		OnUpdate: func(r *client.MakerReverseResult) {
			persistXSessionReverse(dir, sid, st.OfferID, r)
		},
		Log: func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) },
	}
	// The taker's asset leg is present only if it was funded + verified.
	if st.SeqLegTxid != "" && st.SeqLegScriptHex != "" {
		seqScript, serr := hex.DecodeString(st.SeqLegScriptHex)
		if serr != nil {
			return zero, fmt.Errorf("bad seq_leg_script_hex")
		}
		rp.SeqLeg = &xchain.LegLock{
			Script:   seqScript,
			Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
			Locktime: st.SeqLocktime,
		}
	}
	// ON-CHAIN DISCOVERY of an announced-never leg. The taker's SeqLegFunded announce
	// rides the courier only, and a maker that gave up on a detached taker ("no seq
	// leg from the taker") used to be the END of the settle path even though the
	// taker HAD funded — its asset sat waiting for a claim that never came while the
	// maker's BTC refunded (both sides watched two live legs die of a lost message;
	// two such sessions were repaired by hand on 2026-08-11). The script is fully
	// determined by material the maker persisted at terms time (H via the secret,
	// its claim key, the taker's refund pub, T_seq), so when no leg was recorded,
	// rebuild the script and scan the UTXO set for it.
	if rp.SeqLeg == nil && st.TakerSeqRefundPub != "" && st.SeqLocktime > 0 && rp.SeqClaimKey != nil {
		if refundPub, rerr := hex.DecodeString(st.TakerSeqRefundPub); rerr == nil && len(refundPub) == 33 {
			if script, utxo := discoverHTLC(seqChain, rp.Ops.(*client.LiveXcOps).Swap.SEQHTLCScriptVariants, rp.SeqClaimKey.PubKey(), refundPub, st.SeqLocktime); utxo != nil {
				{
					bh, _ := seqChain.BlockHashOfTx(utxo.TxID)
					fmt.Printf("session %s: reverse resume: DISCOVERED the taker's asset leg on-chain (%s:%d, %d atoms) — the courier announce never arrived; claiming from the persisted terms\n",
						sid, utxo.TxID, utxo.Vout, utxo.Amount)
					rp.SeqLeg = &xchain.LegLock{Script: script, Funded: utxo, Locktime: st.SeqLocktime}
					rp.SeqBlockHash = bh
					if rp.AssetHex == "" {
						rp.AssetHex = utxo.AssetID
					}
					if rp.SeqAmount == 0 {
						rp.SeqAmount = utxo.Amount
					}
				}
			}
		}
	}
	return rp, nil
}

// swapReqAdapter adapts a seqob *seqdexv1.SwapRequest to ports.SwapRequest. The
// seqob request carries no fee asset/amount (the open fee market is resolved
// inside CompleteSwap), so those return zero values; *seqdexv1.UnblindedInput
// already satisfies ports.UnblindedInput.
type swapReqAdapter struct{ r *seqdexv1.SwapRequest }

func (a swapReqAdapter) GetId() string          { return a.r.GetId() }
func (a swapReqAdapter) GetAssetP() string      { return a.r.GetAssetP() }
func (a swapReqAdapter) GetAmountP() uint64     { return a.r.GetAmountP() }
func (a swapReqAdapter) GetAssetR() string      { return a.r.GetAssetR() }
func (a swapReqAdapter) GetAmountR() uint64     { return a.r.GetAmountR() }
func (a swapReqAdapter) GetTransaction() string { return a.r.GetTransaction() }
func (a swapReqAdapter) GetFeeAsset() string    { return "" }
func (a swapReqAdapter) GetFeeAmount() uint64   { return 0 }
func (a swapReqAdapter) GetUnblindedInputs() []ports.UnblindedInput {
	src := a.r.GetUnblindedInputs()
	out := make([]ports.UnblindedInput, 0, len(src))
	for _, u := range src {
		out = append(out, u)
	}
	return out
}

// utxosToSwapUnblinded converts the maker's CompleteSwap-selected utxos to the
// swap.UnblindedInput list for the SwapAccept, using the same index convention as
// the proven trade path (trading.go).
func utxosToSwapUnblinded(utxos []ports.Utxo) []swap.UnblindedInput {
	ins := make([]swap.UnblindedInput, 0, len(utxos))
	for i, u := range utxos {
		ins = append(ins, swap.UnblindedInput{
			Index:         uint32(i),
			Asset:         u.GetAsset(),
			Amount:        u.GetValue(),
			AssetBlinder:  u.GetAssetBlinder(),
			AmountBlinder: u.GetValueBlinder(),
		})
	}
	return ins
}

// trimLadderToBalance drops the deepest rungs until the ladder's total give-side
// amount fits the account's confirmed balance of the give asset.
func trimLadderToBalance(ctx context.Context, svc application.WalletService, account string, ladder []*seqobv1.Offer) []*seqobv1.Offer {
	if len(ladder) == 0 {
		return ladder
	}
	bals, err := svc.Account().GetBalance(ctx, account)
	if err != nil {
		fmt.Printf("ladder: cannot read account %q balance (%v); posting unchecked\n", account, err)
		return ladder
	}
	giveAsset := ladder[0].GetOfferAsset()
	var have uint64
	if b, ok := bals[giveAsset]; ok {
		have = b.GetConfirmedBalance()
	}
	var total uint64
	for i, lo := range ladder {
		if total+lo.GetOfferAmount() > have {
			if i == 0 {
				fatal("account %q holds %d of %s but even the first rung gives %d; fund it or lower -base-amount/-quote-amount", account, have, giveAsset, lo.GetOfferAmount())
			}
			fmt.Printf("ladder: account %q holds %d of %s; keeping %d of %d rungs (the rest would be phantom depth)\n", account, have, giveAsset, i, len(ladder))
			return ladder[:i]
		}
		total += lo.GetOfferAmount()
	}
	return ladder
}

// envDefault fills an empty flag value from the named environment variable.
func envDefault(flagVal *string, env string) {
	if *flagVal == "" {
		*flagVal = os.Getenv(env)
	}
}

func loadOrGenKey(hexKey string) *btcec.PrivateKey {
	if hexKey == "" {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			fatal("gen key: %v", err)
		}
		// Only the public half is printed: under a supervisor stdout is a log file, and
		// the identity key must not end up in it. A generated key lives only in this
		// process; pass -maker-priv to keep a stable identity across restarts.
		fmt.Printf("generated maker key (not persisted; pass -maker-priv for a stable identity): pub=%s\n",
			hex.EncodeToString(k.PubKey().SerializeCompressed()))
		return k
	}
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		fatal("-maker-priv must be 32-byte hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

// wsWriteMu serializes WS writes: cross-mode lift sessions courier from their
// own goroutines and gorilla/websocket allows only one concurrent writer.
var wsWriteMu sync.Mutex

// writeWS returns the write error (rather than exiting) so the same-chain serve loop can reconnect
// and re-post instead of dying — dying would let the supervisor restart us, which churns the offer_id.
func writeWS(c *websocket.Conn, to *seqobv1.To) error {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		return err
	}
	wsWriteMu.Lock()
	defer wsWriteMu.Unlock()
	return c.WriteMessage(websocket.TextMessage, b)
}

// --- Cross-chain (BTC<->asset) maker -----------------------------------------

type crossMakerConfig struct {
	relay        string
	makerKey     *btcec.PrivateKey
	makerPubHex  string
	asset, side  string
	assetAmt     uint64
	btcAmt       uint64
	feeAsset     string
	expiry       time.Duration
	minAnchor    uint32
	offerID      string
	btcRPCURL    string
	btcWallet    string
	btcChainName string
	seqRPCURL    string
	seqWallet    string
	btcDelta     uint32
	seqDelta     uint32
	minBTCConf   int
	spendFee     uint64
	stateDir     string
	btcFeeRate   float64
	requote      bool
}

// runCrossMaker posts a cross-chain offer and serves forward lifts with the
// xdriver over pkg/xchain. It needs no Ocean wallet: the SEQ asset leg is
// funded from the Sequentia NODE wallet and the claimed BTC lands in the
// bitcoind wallet — the same reserves the RFQ maker uses (do not re-fund).
func runCrossMaker(cfg crossMakerConfig) {
	sideL := strings.ToLower(cfg.side)
	if sideL != "sell" && sideL != "buy" {
		fatal("-mode cross -side must be sell (taker pays BTC) or buy (taker sells the asset)")
	}
	if cfg.btcRPCURL == "" || cfg.seqRPCURL == "" {
		fatal("-mode cross requires -btc-rpc and -xseq-rpc")
	}
	btcRPC, err := rpcFromURL(cfg.btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	seqRPC, err := rpcFromURL(cfg.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(cfg.btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, cfg.btcWallet, params)
	btcChain.SetFeeRate(cfg.btcFeeRate)
	seqChain := xchain.NewChain(seqRPC, cfg.seqWallet)

	// Sanity: both nodes reachable before we advertise anything.
	if _, err := btcChain.BlockCount(); err != nil {
		fatal("bitcoind unreachable: %v", err)
	}
	if _, err := seqChain.BlockCount(); err != nil {
		fatal("sequentia node unreachable: %v", err)
	}

	// Advisory receive address for the resting offer, from the node wallet.
	var recvAddr string
	if err := seqChain.RPC().Call(&recvAddr, "getnewaddress"); err != nil {
		fatal("getnewaddress on the Sequentia wallet: %v", err)
	}

	o := buildCrossOffer(cfg.asset, cfg.side, cfg.assetAmt, cfg.btcAmt, cfg.spendFee, cfg.feeAsset,
		cfg.expiry, cfg.minAnchor, recvAddr, cfg.makerPubHex, cfg.offerID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		fatal("create -xstate-dir %s: %v", cfg.stateDir, err)
	}
	// NOTE: pending-session resume is deliberately NOT done per-maker here. The fleet shares ONE
	// -xstate-dir, so having every serving maker scan+resume on startup would make 144 makers herd on the
	// same sessions (redundant claims/refunds, broadcast conflicts). Recovery is driven instead by a SINGLE
	// dedicated `-mode cross -resume` settler loop (drivePendingCrossSessions, via resumeCrossSessions) that
	// re-drives every non-terminal session to claim-or-refund. That is what unstrands a maker cycled by the
	// supervisor between recording the taker's asset leg and claiming it — the stall that blocked the E2E.

	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	fmt.Printf("seqob-maker up (CROSS): posted %s offer %s by maker %s\n", cfg.side, o.GetOfferId(), cfg.makerPubHex)
	fmt.Printf("  %d %s <- %d BTC sats  T_btc=+%d T_seq=+%d min-conf=%d\n",
		cfg.assetAmt, cfg.asset, cfg.btcAmt, cfg.btcDelta, cfg.seqDelta, cfg.minBTCConf)
	fmt.Printf("  taker lifts with: seqob-cli xlift -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), cfg.makerPubHex)

	serveCross(ws, wsURL, o, cfg, btcChain, seqChain)
}

// crossWS holds the (re)dialable relay connection: cross sessions run for many
// minutes in their own goroutines, so writes are serialized here and a WS drop
// reconnects instead of killing in-flight ON-CHAIN settlements.
type crossWS struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *crossWS) current() *websocket.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

func (w *crossWS) write(to *seqobv1.To) error {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws not connected")
	}
	return w.conn.WriteMessage(websocket.TextMessage, b)
}

// redial dials and, when an offer is given, (re)submits it so the relay
// re-registers this connection as the maker's lift route.
func (w *crossWS) redial(wsURL string, o *seqobv1.Offer) error {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
	}
	w.conn = conn
	w.mu.Unlock()
	if o != nil {
		return w.write(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	}
	return nil
}

func (w *crossWS) redialLoop(wsURL string, o *seqobv1.Offer) {
	for {
		if err := w.redial(wsURL, o); err == nil {
			fmt.Println("relay reconnected")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// serveCross is the cross-mode event loop: each lift gets its own goroutine
// running RunMakerForward; the loop routes sealed courier frames to the
// session's inbox. Whole-HTLC discipline: ONE lift in flight at a time, and
// the offer is cancelled after its first settlement (no fill accounting exists
// for cross offers, so serving further lifts would oversell the signed size at
// a stale price; restart the maker to re-quote).
func serveCross(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg crossMakerConfig,
	btcChain *xchain.BitcoinChain, seqChain *xchain.Chain) {
	var mu sync.Mutex
	inboxes := make(map[string]chan []byte)
	inFlight := 0
	filled := false
	idle := func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 }
	// The shutdown drain asks the loop that owns the value, not the log.
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })
	armRequoteExit(o, idle) // retire for a fresh re-quote before the offer expires

	refuse := func(sid string, cr *client.Crypter, code, msg string) {
		m := &client.XcMsg{Type: client.XcFail, Code: code, Message: msg}
		if sealed, err := m.Seal(cr); err == nil {
			_ = ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		}
	}

	for {
		conn := ws.current()
		_, data, err := conn.ReadMessage()
		if err != nil {
			mu.Lock()
			done := filled && inFlight == 0
			resubmit := o
			if filled {
				resubmit = nil // never re-advertise a filled offer
			}
			mu.Unlock()
			if done {
				fmt.Println("offer filled and no lift in flight; exiting (restart to re-quote)")
				return
			}
			// The relay EVICTS a maker's offers when its WS drops (an offline maker can't
			// co-sign — ws.go closeConn -> RemoveByMaker), so on reconnect we must re-post.
			// Refresh the offer's created/expiry window + re-sign FIRST: otherwise the relay
			// rejects the stale copy as "invalid offer: offer already expired" and the offer
			// never comes back — the cross book stays empty until a manual fleet restart (the
			// fragility a relay restart exposes). Refreshing revives an expired offer and is a
			// harmless expiry-extension for a still-valid one.
			if resubmit != nil {
				if e := refreshOfferForRequote(resubmit, cfg.expiry, cfg.makerKey); e != nil {
					fmt.Printf("reconnect: re-sign offer failed: %v (relay may reject re-post)\n", e)
				}
			}
			fmt.Printf("ws read error: %v; reconnecting (in-flight settlements continue on-chain)\n", err)
			ws.redialLoop(wsURL, resubmit)

			// RE-ATTACH EVERY IN-FLIGHT SESSION, or the courier goes silent forever.
			//
			// Re-posting the offer restores this connection as the maker's route for NEW
			// lifts, but it says nothing about lifts already running. On the relay a role's
			// courier frames are delivered by a pump started in attach(), and that pump exits
			// as soon as its connection closes. The new connection carries no role binding and
			// no pump — while Router.Send still SUCCEEDS, because each inbox is a buffered
			// channel. So the counterparty's next frame is queued into a channel nobody will
			// ever drain, and both sides wait for each other until their deadlines elapse.
			//
			// That is exactly how a rail-crossing take died: the LSP funded the BTC HTLC and
			// sent btc_leg_funded, we never received it ("courier recv while waiting for
			// btc_leg_funded: courier timeout"), and the trade stalled with real coin already
			// locked on-chain. The same fault kills the other direction when the TAKER
			// reconnects, which is why no rail settled — one courier-delivery bug, not one
			// per rail.
			//
			// The relay has implemented an authenticated session_reattach all along; nothing
			// had ever sent one. Re-proving the offer key suffices: it is the maker's session
			// key and the relay already holds it for this session, so no new trust appears.
			reattachSessions(ws, cfg.makerKey, &mu, inboxes)
			continue
		}
		var from seqobv1.From
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			sid := lr.GetSessionId()
			cr, err := client.NewMakerCrypterFromLift(cfg.makerKey, lr.GetTakerSessionPubkey())
			if err != nil {
				fmt.Printf("lift %s: crypter error: %v\n", sid, err)
				continue
			}
			// The relay routes by pubkey; a sibling process under the same identity key
			// may own the offer this lift names. The other serve loops already refuse
			// that up front. Here it matters most: the REVERSE cross maker funds its BTC
			// leg before the taker checks anything, so a mis-wired lift strands maker BTC
			// until T_btc.
			if lr.GetOfferId() != o.GetOfferId() {
				refuse(sid, cr, "stale_offer", "this maker process serves offer "+o.GetOfferId()+", not "+lr.GetOfferId())
				continue
			}
			mu.Lock()
			busy, done := inFlight > 0, filled
			var in chan []byte
			if !busy && !done {
				inFlight++
				in = make(chan []byte, 8)
				inboxes[sid] = in
			}
			mu.Unlock()
			if done {
				refuse(sid, cr, "offer_filled", "offer already filled; awaiting re-quote")
				continue
			}
			if busy {
				refuse(sid, cr, "busy", "another lift is in flight (whole-HTLC, one at a time)")
				continue
			}
			if isDraining() {
				// Shutting down: take no NEW value, so the swaps we already hold can drain.
				// An explicit refusal is retryable against another maker; silence is not.
				refuse(sid, cr, "draining", "this maker is shutting down for a re-quote; retry on another offer")
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				mu.Unlock()
				continue
			}
			if lr.GetTakeAmount() > o.GetBaseAmount() {
				fmt.Printf("lift %s: take %d > offer %d; the driver will reject the over-ask\n",
					sid, lr.GetTakeAmount(), o.GetBaseAmount())
			}
			fmt.Printf("cross lift requested: session %s offer %s (take %d of %d)\n",
				sid, lr.GetOfferId(), lr.GetTakeAmount(), o.GetBaseAmount())

			// QUOTE THE SLICE THE TAKER ASKED FOR, not the whole offer.
			//
			// This offer advertises allow_partial with a min_fill, the relay forwards
			// take_amount, and the post-fill path below already re-rests the remainder via
			// ProportionalBtc/MinFillBase — but the HANDSHAKE still quoted the offer's whole
			// OfferAmount/WantAmount. So a taker asking for 8% of a resting offer was told the
			// price of 100% of it, and the bridge's bind check ("maker wants N sats, above the
			// offered M — refuse") correctly killed the trade instantly. Partial support was
			// built everywhere except the one place that names the price.
			//
			// Rounding is the maker's favour on both sides, matching the taker's mirrored
			// arithmetic bit-for-bit (xswap.js proportionalBtcCeil / the Go helpers): CEIL when
			// the TAKER pays the BTC (forward), FLOOR when the MAKER pays it (reverse). Both
			// helpers return the whole amount exactly when take >= whole, so a whole-offer lift
			// is byte-identical to before this change.
			takeBase := lr.GetTakeAmount()
			if takeBase == 0 || takeBase > o.GetBaseAmount() {
				takeBase = o.GetBaseAmount()
			}
			if mf := o.GetMinFill(); mf > 0 && takeBase < mf {
				// Below our own advertised minimum. Say so rather than quoting a slice we do not
				// offer — a sub-min slice prices to a leg that cannot economically settle.
				refuse(sid, cr, "below_min_fill",
					fmt.Sprintf("take %d is below this offer's min_fill %d", takeBase, mf))
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				mu.Unlock()
				continue
			}
			// SELL (forward): OfferAmount=asset given, WantAmount=BTC wanted, Base=asset.
			// BUY (reverse):  OfferAmount=BTC given,   WantAmount=asset wanted, Base=asset.
			// The SAME predicate the handshake below branches on (`reverse`), deliberately —
			// deciding the rounding direction from a different field than the one that picks
			// the handshake is how the two silently drift into disagreeing.
			sliceReverse := o.GetCrossChain().GetDirection() == offer.DirAssetToBTC
			sliceSeq := takeBase
			var sliceBtc uint64
			if sliceReverse {
				sliceBtc = client.ProportionalBtcFloor(o.GetOfferAmount(), takeBase, o.GetBaseAmount())
			} else {
				sliceBtc = client.ProportionalBtc(o.GetWantAmount(), takeBase, o.GetBaseAmount())
			}
			if takeBase < o.GetBaseAmount() {
				fmt.Printf("session %s: PARTIAL quote %d of %d atoms for %d sats (whole offer %d/%d)\n",
					sid, takeBase, o.GetBaseAmount(), sliceBtc, o.GetOfferAmount(), o.GetWantAmount())
			}

			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }
			newOpsFromHash := func(hashH []byte) (client.XcOps, error) {
				swp := xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(hashH))
				return &client.LiveXcOps{Swap: swp, BTC: btcChain, SEQ: seqChain}, nil
			}
			reverse := sliceReverse

			go func(sid string, in chan []byte, reverse bool) {
				settled := false
				var settleTxid string
				var filledSeq, filledBtc uint64
				defer func() {
					// Record the executed fill FIRST, before the partial-remainder re-rest or the
					// requote/cancel mutate/remove the still-resting offer: the relay records the
					// trade (price/size for the BTC pair's last_price + chart) at the ORIGINAL price
					// and decrements by exactly filledSeq (the base atoms actually taken; a cross lift
					// may be partial). Maker-signed + offer-keyed, so it lands with the session gone.
					if settled {
						reportSettledTrade(ws, o, cfg.makerKey, filledSeq, settleTxid)
					}
					// Cross legs are on-chain (no LN channel peers), so the pre-quote "reconnect" is a
					// reachability ping of both backends.
					reconnect := func() {
						if _, err := btcChain.BlockCount(); err != nil {
							fmt.Printf("requote: bitcoind unreachable before re-quote: %v\n", err)
						}
						if _, err := seqChain.BlockCount(); err != nil {
							fmt.Printf("requote: sequentia node unreachable before re-quote: %v\n", err)
						}
					}
					// Partial fill: the taker took only a slice, so reduce the offer to the remainder
					// and re-quote (there is more to trade, regardless of -requote); requoteAfterFill
					// re-signs + re-posts the shrunk offer. A whole fill keeps the old behaviour
					// (-requote re-posts the whole; otherwise the offer is retired). The remainder is
					// priced so the maker NEVER over-commits its offer's capital across a full sweep:
					//   - forward (SELL, maker receives BTC): re-quote WantAmount at the offer's own
					//     rate (ProportionalBtc, ceil) — the maker asks >= its rate on the remainder,
					//     total received >= the offer, capped by the asset it actually gives.
					//   - reverse (BUY, maker gives BTC): SUBTRACT the exact filled sats, so the sum of
					//     partials commits at most the offer's original BTC (a proportional re-quote
					//     would round each partial up and over-commit).
					// Minimum-slice floor on the REMAINDER: mirror the covenant CLOB's
					// 'remainder == 0 OR remainder >= min_fill'. crossReRestRemainder returns
					// reRest=false for a whole fill OR a fill that would leave a sub-min_fill dust
					// remainder — re-resting such a remainder would advertise an order only sub-dust
					// partials could fill, so drop to a full fill (retire / -requote) instead.
					remainAsset, canReRest := crossReRestRemainder(o.GetBaseAmount(), filledSeq, o.GetMinFill())
					partial := settled && canReRest
					if partial && reverse && filledBtc >= o.GetOfferAmount() {
						// Defensive: a partial that somehow consumed the whole BTC budget leaves no
						// capital to re-rest; treat it as a full fill (retire/-requote) rather than
						// re-posting a 0-BTC offer.
						partial = false
					}
					if partial {
						mu.Lock()
						if reverse {
							// BUY offer: OfferAmount=BTC (given), WantAmount=asset (wanted), Base=asset.
							o.OfferAmount = o.GetOfferAmount() - filledBtc
							o.BaseAmount = remainAsset
							o.WantAmount = remainAsset
							// Re-derive min_fill for the shrunk BUY offer (price is ~unchanged, so this
							// stays consistent and never exceeds the new base_amount).
							o.MinFill = client.MinFillBase(o.GetOfferAmount(), remainAsset, cfg.spendFee)
						} else {
							// SELL offer: OfferAmount=asset (given), WantAmount=BTC (wanted), Base=asset.
							// Price the remainder at the offer's OWN rate (ceil), not by subtracting the
							// rounded filledBtc — that subtraction drifts and can zero the want on a
							// low-priced offer. ProportionalBtc rounds up, so remainAsset>0 => want>=1.
							o.WantAmount = client.ProportionalBtc(o.GetWantAmount(), remainAsset, o.GetBaseAmount())
							o.BaseAmount = remainAsset
							o.OfferAmount = remainAsset
							// Re-derive min_fill for the shrunk SELL offer against its new BTC want.
							o.MinFill = client.MinFillBase(o.GetWantAmount(), remainAsset, cfg.spendFee)
						}
						mu.Unlock()
						fmt.Printf("session %s: PARTIAL fill (%d %s, %d sats); re-resting the remainder %d %s (offer %d, want %d)\n",
							sid, filledSeq, o.GetPair().GetBaseAsset(), filledBtc, remainAsset,
							o.GetPair().GetBaseAsset(), o.GetOfferAmount(), o.GetWantAmount())
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, reconnect)
					} else if settled && cfg.requote {
						// -requote: re-post a fresh quote WHILE still holding the in-flight slot, so the
						// serve loop refuses any racing lift as "busy" until the new offer is live.
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, reconnect)
					}
					mu.Lock()
					inFlight--
					delete(inboxes, sid)
					if settled && !partial && !cfg.requote {
						filled = true
					}
					mu.Unlock()
					if settled && !partial && !cfg.requote {
						cancelOffer(cfg.relay, o, cfg.makerKey)
					}
					requoteExitIfPending(idle)
				}()
				if reverse {
					// BUY: the maker holds the secret and funds the BTC leg first.
					p := client.MakerReverseParams{
						NewOps: func(secret []byte) (client.XcOps, error) {
							swp := xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLock(secret))
							return &client.LiveXcOps{Swap: swp, BTC: btcChain, SEQ: seqChain}, nil
						},
						Crypter: cr, BtcTip: btcChain.BlockCount, SeqTip: seqChain.BlockCount,
						AssetHex: o.GetPair().GetBaseAsset(), SeqAmount: sliceSeq, BtcAmount: sliceBtc,
						BtcLocktimeDelta: cfg.btcDelta, SeqLocktimeDelta: cfg.seqDelta,
						MinBTCConf: cfg.minBTCConf, SpendFeeSats: cfg.spendFee, Log: logf,
						OnUpdate: func(r *client.MakerReverseResult) { persistXSessionReverse(cfg.stateDir, sid, o.GetOfferId(), r) },
					}
					res, err := client.RunMakerReverse(p, in, send)
					if err != nil {
						fmt.Printf("session %s: reverse cross lift ended: %v\n", sid, err)
						if res != nil && res.BtcRefundTx != "" {
							fmt.Printf("session %s: BTC leg refunded in %s\n", sid, res.BtcRefundTx)
						}
						return
					}
					settled = true
					settleTxid = res.SeqClaimTxid
					filledSeq, filledBtc = res.FilledSeq, res.FilledBtc
					fmt.Printf("session %s: REVERSE CROSS SWAP SETTLED: bought %d %s for %d sats, claimed the asset in %s\n",
						sid, res.FilledSeq, o.GetPair().GetBaseAsset(), res.FilledBtc, res.SeqClaimTxid)
					return
				}
				// SELL (forward): the taker pays BTC and holds the secret.
				p := client.MakerForwardParams{
					NewOps: newOpsFromHash, Crypter: cr,
					BtcTip: btcChain.BlockCount, SeqTip: seqChain.BlockCount,
					AssetHex: o.GetPair().GetBaseAsset(), SeqAmount: sliceSeq, BtcAmount: sliceBtc,
					BtcLocktimeDelta: cfg.btcDelta, SeqLocktimeDelta: cfg.seqDelta,
					MinBTCConf: cfg.minBTCConf, SpendFeeSats: cfg.spendFee, Log: logf,
					OnUpdate: func(r *client.MakerForwardResult) { persistXSession(cfg.stateDir, sid, o.GetOfferId(), r) },
				}
				res, err := client.RunMakerForward(p, in, send)
				if err != nil {
					fmt.Printf("session %s: cross lift ended: %v\n", sid, err)
					if res != nil && res.SeqRefundTx != "" {
						fmt.Printf("session %s: SEQ leg refunded in %s\n", sid, res.SeqRefundTx)
					}
					return
				}
				settled = true
				settleTxid = res.BtcClaimTxid
				filledSeq, filledBtc = res.FilledSeq, res.FilledBtc
				fmt.Printf("session %s: CROSS SWAP SETTLED: sold %d %s for %d sats, taker claimed the asset, BTC claimed in %s\n",
					sid, res.FilledSeq, o.GetPair().GetBaseAsset(), res.FilledBtc, res.BtcClaimTxid)
			}(sid, in, reverse)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				fmt.Printf("session %s: swap_msg without a live cross session; ignoring\n", sm.GetSessionId())
				continue
			}
			select {
			case in <- sm.GetCiphertext():
			default:
				fmt.Printf("session %s: inbox full; dropping frame\n", sm.GetSessionId())
			}

		case from.GetOrderStatus() != nil:
			st := from.GetOrderStatus()
			fmt.Printf("order %s status=%s active=%d txid=%s\n",
				st.GetOfferId(), st.GetStatus(), st.GetActiveAmount(), st.GetSettleTxid())

		case from.GetError() != nil:
			e := from.GetError()
			fmt.Printf("relay error %d: %s\n", e.GetCode(), e.GetMessage())
			// THE TAKER IS GONE — FREE THE SLOT NOW, DON'T WAIT OUT THE DEADLINE.
			//
			// This maker serves ONE lift at a time and holds that slot for the whole driver run. When a
			// taker abandons a lift (the wallet gives up on terms after 30s; this driver's own
			// TermsRequest deadline is 2 minutes) the relay answers our next frame with 409 "the taker
			// is not attached to session <id>" — and we used to print it and keep waiting, refusing
			// every other taker with "busy" for the remainder. A handful of abandoned takes wedged the
			// whole fleet that way, which is exactly how it looked from the wallet: every offer
			// answering "busy — another lift is in flight".
			//
			// Closing that session's inbox makes the driver's next receive return "session closed", so
			// it unwinds through its normal error path and the deferred accounting frees the slot. No
			// value is at risk: an abandoned lift is pre-lock by construction — the taker never funded.
			releaseDetachedSession(e, &mu, inboxes)
			if offerRejected(e.GetCode(), e.GetMessage()) {
				requoteExitIfIdle("relay rejected our offer ("+e.GetMessage()+")", idle)
			}
		}
	}
}

// reattachSessions re-binds every in-flight courier session to a freshly dialled
// socket. On the relay a role's courier frames are pumped by a goroutine started in
// attach(), which exits when its connection closes; a new connection carries no
// binding, so without this the counterparty's next frame is queued to nobody and
// both sides wait out their deadlines with funds possibly locked. The cross loop
// has done this since the rail-crossing take that died of it; the other loops
// dialled back and went silent.
func reattachSessions(ws *crossWS, makerKey *btcec.PrivateKey, mu *sync.Mutex, inboxes map[string]chan []byte) {
	mu.Lock()
	live := make([]string, 0, len(inboxes))
	for sid := range inboxes {
		live = append(live, sid)
	}
	mu.Unlock()
	for _, sid := range live {
		if e := ws.write(&seqobv1.To{Msg: &seqobv1.To_SessionReattach{
			SessionReattach: &seqobv1.SessionReattach{
				SessionId: sid,
				Role:      "maker",
				Sig:       offer.SignReattach(sid, "maker", makerKey),
			}}}); e != nil {
			fmt.Printf("reconnect: re-attach of session %s FAILED: %v (its courier frames are stranded; the lift will time out)\n", sid, e)
			continue
		}
		fmt.Printf("reconnect: re-attached session %s; couriered frames resume\n", sid)
	}
}

// releaseDetachedSession frees the lift slot of a session whose taker the relay
// reports as gone ("the taker is not attached to session <id>", 409): closing the
// inbox makes the driver's next receive return "session closed", so it unwinds
// through its normal error path and the deferred accounting frees the slot. An
// abandoned lift is pre-lock by construction — the taker never funded — so nothing
// is at risk; what was at risk was every other taker being told "busy" for the
// rest of a multi-hour driver deadline.
func releaseDetachedSession(e *seqobv1.GenericError, mu *sync.Mutex, inboxes map[string]chan []byte) {
	sid := sessionFromNotAttached(e.GetCode(), e.GetMessage())
	if sid == "" {
		return
	}
	mu.Lock()
	if in, ok := inboxes[sid]; ok {
		delete(inboxes, sid)
		close(in)
		fmt.Printf("session %s: taker detached; releasing the lift slot\n", sid)
	}
	mu.Unlock()
}

// sessionFromNotAttached returns the session id out of the relay's "counterparty is not attached to
// session <id>" 409, or "" for anything else. Deliberately narrow: it releases a LIVE lift slot, so it
// must match only that error and never a generic 409.
func sessionFromNotAttached(code uint32, msg string) string {
	const marker = "is not attached to session "
	if code != 409 {
		return ""
	}
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(msg[i+len(marker):])
}

// xmakerSessionState is the on-disk snapshot of one cross lift: with it, an
// operator can refund the SEQ leg after T_seq or claim the BTC leg with a
// learned secret even if the process died mid-swap.
type xmakerSessionState struct {
	SessionID        string `json:"session_id"`
	OfferID          string `json:"offer_id"`
	Direction        string `json:"direction,omitempty"` // "forward" (sell) | "reverse" (buy)
	HashHex          string `json:"hash_hex,omitempty"`
	BtcClaimPrivHex  string `json:"btc_claim_priv_hex,omitempty"`  // forward: claims taker BTC
	SeqRefundPrivHex string `json:"seq_refund_priv_hex,omitempty"` // forward: refunds our SEQ leg
	// reverse: the maker holds the secret, claims the taker's SEQ leg, refunds its own BTC leg
	SecretForRefundHex string `json:"secret_for_refund_hex,omitempty"` // reverse: same as secret_hex, named for clarity
	SeqClaimPrivHex    string `json:"seq_claim_priv_hex,omitempty"`    // reverse: claims taker SEQ
	BtcRefundPrivHex   string `json:"btc_refund_priv_hex,omitempty"`   // reverse: refunds our BTC leg
	BtcRefundTx        string `json:"btc_refund_tx,omitempty"`         // reverse
	BtcLocktime        uint32 `json:"btc_locktime,omitempty"`
	BtcLegBackend      string `json:"btc_leg_backend,omitempty"` // sub-asset rails: "bitcoin" | "seq:<quote asset>" (the chain the on-chain leg lives on)
	SeqLocktime        uint32 `json:"seq_locktime,omitempty"`
	BtcLegTxid         string `json:"btc_leg_txid,omitempty"`
	BtcLegVout         uint32 `json:"btc_leg_vout"`
	BtcLegAmount       uint64 `json:"btc_leg_amount,omitempty"`
	BtcLegScriptHex    string `json:"btc_leg_script_hex,omitempty"`
	SeqLegTxid         string `json:"seq_leg_txid,omitempty"`
	SeqLegVout         uint32 `json:"seq_leg_vout"`
	SeqLegAmount       uint64 `json:"seq_leg_amount,omitempty"`
	SeqLegAsset        string `json:"seq_leg_asset,omitempty"`
	SeqLegScriptHex    string `json:"seq_leg_script_hex,omitempty"`
	SeqBlockHash       string `json:"seq_block_hash,omitempty"`
	TakerSeqRefundPub  string `json:"taker_seq_refund_pub,omitempty"` // reverse: rebuilds the taker's asset-leg script for on-chain discovery
	Bolt11             string `json:"bolt11,omitempty"`               // submarine: the taker's invoice (pay -> learn P) for a full resume
	InvoiceMsat        uint64 `json:"invoice_msat,omitempty"`         // submarine: the agreed invoice amount
	SecretHex          string `json:"secret_hex,omitempty"`
	BtcClaimTxid       string `json:"btc_claim_txid,omitempty"` // forward: maker claimed the taker's BTC
	SeqRefundTx        string `json:"seq_refund_tx,omitempty"`  // forward: maker refunded its SEQ leg
	SeqClaimTxid       string `json:"seq_claim_txid,omitempty"` // reverse: maker claimed the taker's SEQ
	Settled            bool   `json:"settled"`
	UpdatedAt          string `json:"updated_at"`
}

func persistXSession(dir, sid, offerID string, r *client.MakerForwardResult) {
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "forward",
		BtcLocktime: r.BtcLocktime, SeqLocktime: r.SeqLocktime,
		SeqBlockHash: r.SeqBlockHash,
		BtcClaimTxid: r.BtcClaimTxid, SeqRefundTx: r.SeqRefundTx,
		Settled:   r.Settled,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if r.BtcClaimKey != nil {
		st.BtcClaimPrivHex = hex.EncodeToString(r.BtcClaimKey.Bytes())
	}
	if r.SeqRefundKey != nil {
		st.SeqRefundPrivHex = hex.EncodeToString(r.SeqRefundKey.Bytes())
	}
	if r.BtcLeg != nil && r.BtcLeg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = r.BtcLeg.Funded.TxID, r.BtcLeg.Funded.Vout, r.BtcLeg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
	}
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	if len(r.Secret) > 0 {
		st.SecretHex = hex.EncodeToString(r.Secret)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	if err := ioutil.WriteFile(filepath.Join(dir, sid+".json"), b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
	}
}

// persistXSessionReverse snapshots a reverse (buy) cross lift. The maker holds
// the secret and funds the BTC leg, so the recovery material is the secret plus
// the seq-claim / btc-refund keys and both legs.
func persistXSessionReverse(dir, sid, offerID string, r *client.MakerReverseResult) {
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "reverse",
		BtcLocktime: r.BtcLocktime, SeqLocktime: r.SeqLocktime,
		SeqBlockHash: r.SeqBlockHash,
		SeqClaimTxid: r.SeqClaimTxid, BtcRefundTx: r.BtcRefundTx,
		Settled:   r.Settled,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if len(r.Secret) > 0 {
		st.SecretHex = hex.EncodeToString(r.Secret)
		st.SecretForRefundHex = st.SecretHex
	}
	if r.SeqClaimKey != nil {
		st.SeqClaimPrivHex = hex.EncodeToString(r.SeqClaimKey.Bytes())
	}
	if r.BtcRefundKey != nil {
		st.BtcRefundPrivHex = hex.EncodeToString(r.BtcRefundKey.Bytes())
		if len(r.TakerSeqRefundPub) == 33 {
			// Received in the very first TermsRequest. With it persisted, the taker's
			// asset-leg script is reconstructable and the resume can DISCOVER a funded
			// leg whose courier announce never arrived (see resumeParamsFromStateReverse).
			st.TakerSeqRefundPub = hex.EncodeToString(r.TakerSeqRefundPub)
		}
	}
	if r.BtcLeg != nil && r.BtcLeg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = r.BtcLeg.Funded.TxID, r.BtcLeg.Funded.Vout, r.BtcLeg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
	}
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	if err := ioutil.WriteFile(filepath.Join(dir, sid+".json"), b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
	}
}

// persistXSessionSubmarine writes a NORMAL submarine maker lift's recovery material.
// The submarine rail ran with NO session persistence at all: a killed maker lost its
// per-lift claim key, so a taker HTLC funded against it became unspendable by anyone
// (the 2026-08-11 burn). The state mirrors the cross rails' files (same directory,
// direction "submarine"); the resume loop skips unknown directions, so these files
// are recovery material first and an automated-resume input second.
func persistXSessionSubmarine(dir, sid, offerID string, r *client.MakerSubmarineResult) {
	if dir == "" {
		dir = "xmaker-sessions"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Printf("session %s: persist mkdir: %v\n", sid, err)
		return
	}
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "submarine",
		SeqLocktime: r.SeqLocktime,
		Settled:     r.Settled,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if r.SeqClaimKey != nil {
		st.SeqClaimPrivHex = hex.EncodeToString(r.SeqClaimKey.Bytes())
	}
	if len(r.TakerSeqRefundPub) == 33 {
		st.TakerSeqRefundPub = hex.EncodeToString(r.TakerSeqRefundPub)
	}
	if len(r.Preimage) > 0 {
		st.SecretHex = hex.EncodeToString(r.Preimage)
	}
	st.Bolt11 = r.Bolt11
	st.InvoiceMsat = r.InvoiceMsat
	st.SeqClaimTxid = r.SeqClaimTxid
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	if err := ioutil.WriteFile(filepath.Join(dir, sid+".json"), b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
	}
}

// persistXSessionSubmarineReverse records a REVERSE submarine maker lift (the maker
// locked the asset and waits for the taker's Lightning payment). The refund key is
// derived from the identity key, so the file's job is to name the leg: with it, the
// operator can build the CLTV refund after T_seq without scanning the chain.
func persistXSessionSubmarineReverse(dir, sid, offerID string, r *client.MakerReverseSubmarineResult) {
	if dir == "" {
		dir = "xmaker-sessions"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Printf("session %s: persist mkdir: %v\n", sid, err)
		return
	}
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "submarine-reverse",
		SeqLocktime: r.SeqLocktime,
		Bolt11:      r.Bolt11,
		InvoiceMsat: r.FilledMsat,
		Settled:     r.Settled,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if r.SeqRefundKey != nil {
		st.SeqRefundPrivHex = hex.EncodeToString(r.SeqRefundKey.Bytes())
	}
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	writeSessionState(dir, sid, &st)
}

// persistXSessionSubAssetSell records a sub-asset SELL maker lift whose BTC (or
// quote-asset) HTLC is funded and not yet settled, so the leg can be reclaimed at
// T_btc with the identity-derived refund key.
func persistXSessionSubAssetSell(dir, sid, offerID string, r *client.MakerSubAssetSellResult, refund *xchain.Key, quoteAsset string) {
	if dir == "" {
		dir = "xmaker-sessions"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Printf("session %s: persist mkdir: %v\n", sid, err)
		return
	}
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "subasset-sell",
		BtcLocktime:   r.BtcLocktime,
		BtcLegBackend: onchainBackend(quoteAsset),
		Settled:       r.Settled,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if refund != nil {
		st.BtcRefundPrivHex = hex.EncodeToString(refund.Bytes())
	}
	if r.BtcLeg != nil && r.BtcLeg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = r.BtcLeg.Funded.TxID, r.BtcLeg.Funded.Vout, r.BtcLeg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
	}
	writeSessionState(dir, sid, &st)
}

// discoverHTLC scans the UTXO set for a leg funded to any accepted script form for
// these parameters (a counterparty may have built the legacy form), returning the
// script that matched and the funding.
func discoverHTLC(seqChain *xchain.Chain, variants func(claimPub, refundPub []byte, locktime uint32) ([][]byte, error), claimPub, refundPub []byte, locktime uint32) ([]byte, *xchain.FundedHTLC) {
	vs, err := variants(claimPub, refundPub, locktime)
	if err != nil {
		return nil, nil
	}
	for _, script := range vs {
		if utxo, ferr := seqChain.FindHTLCUTXO(script); ferr == nil && utxo != nil {
			return script, utxo
		}
	}
	return nil, nil
}

// onchainBackend names the chain a sub-asset rail's on-chain leg lives on: "bitcoin"
// for the parent chain, or "seq:<asset>" for the mixed same-chain shape where a
// Sequentia quote asset stands in BTC's place.
func onchainBackend(quoteAsset string) string {
	if quoteAsset == "" {
		return "bitcoin"
	}
	return "seq:" + quoteAsset
}

// persistXSessionSubAsset records a sub-asset BUY maker lift from the moment P is
// learned: the taker's on-chain leg is then claimable with P and the identity key
// until T_btc, and only a record on disk makes a crash before the claim a retry.
func persistXSessionSubAsset(dir, sid, offerID string, preimage []byte, leg *xchain.LegLock, tBtc uint32, quoteAsset string, settled bool, claimTxid string) {
	if dir == "" {
		dir = "xmaker-sessions"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Printf("session %s: persist mkdir: %v\n", sid, err)
		return
	}
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "subasset",
		BtcLocktime:   tBtc,
		BtcLegBackend: onchainBackend(quoteAsset),
		SecretHex:     hex.EncodeToString(preimage),
		BtcClaimTxid:  claimTxid,
		Settled:       settled,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	h := sha256.Sum256(preimage)
	st.HashHex = hex.EncodeToString(h[:])
	if leg != nil && leg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = leg.Funded.TxID, leg.Funded.Vout, leg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(leg.Script)
	}
	writeSessionState(dir, sid, &st)
}

// writeSessionState writes a session file atomically (temp + rename), so a crash
// mid-write never leaves a truncated file the resume pass would skip.
func writeSessionState(dir, sid string, st *xmakerSessionState) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	final := filepath.Join(dir, sid+".json")
	tmp := final + ".tmp"
	if err := ioutil.WriteFile(tmp, b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		fmt.Printf("session %s: persist rename: %v\n", sid, err)
	}
}

// resumeSubmarineSession recovers a persisted NORMAL submarine maker lift.
// Tiers, safest first:
//   - P persisted (paid, crashed before the claim): verify the leg, inject P, CLAIM.
//     Chain-only; no Lightning needed.
//   - leg + bolt11 persisted, no P, an -ln-socket given: re-run the full settle
//     (verify -> anchor gate -> pay -> claim). CLN pay is idempotent by payment_hash,
//     so re-paying an already-paid invoice returns the existing P.
//   - leg absent but the taker refund pub persisted: rebuild the script and DISCOVER
//     the funded leg in the UTXO set (the courier announce died before the maker
//     recorded it), then fall into one of the tiers above on the next pass.
//
// A session with none of that is left alone: the taker's asset refunds itself to the
// taker at T_seq and nothing of the maker's is at risk.
func resumeSubmarineSession(st *xmakerSessionState, seqChain *xchain.Chain, lnSocket string, spendFee uint64, dir, name string) {
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil || len(hashH) != 32 {
		fmt.Printf("%s: submarine session with no usable hash; skipping\n", name)
		return
	}
	claimBytes, err := hex.DecodeString(st.SeqClaimPrivHex)
	if err != nil {
		fmt.Printf("%s: submarine session with no claim key; skipping\n", name)
		return
	}
	claimKey := xchain.KeyFromBytes(claimBytes)
	if claimKey == nil {
		fmt.Printf("%s: submarine session claim key invalid; skipping\n", name)
		return
	}
	var ln xchain.LNLeg
	if lnSocket != "" {
		ln = xchain.NewCLNLNLeg(lnSocket)
	}
	sub := xchain.NewSubmarineSwap(seqChain, ln, xchain.NewHashLockFromHash(hashH))

	// Discovery: the leg was funded but never recorded (announce lost). The script is
	// fully determined by persisted material once the taker refund pub is known.
	if st.SeqLegTxid == "" && st.TakerSeqRefundPub != "" && st.SeqLocktime > 0 {
		if refundPub, rerr := hex.DecodeString(st.TakerSeqRefundPub); rerr == nil && len(refundPub) == 33 {
			if script, utxo := discoverHTLC(seqChain, sub.SEQHTLCScriptVariants, claimKey.PubKey(), refundPub, st.SeqLocktime); utxo != nil {
				{
					fmt.Printf("%s: submarine resume: DISCOVERED the taker's asset leg on-chain (%s:%d, %d atoms)\n",
						name, utxo.TxID, utxo.Vout, utxo.Amount)
					st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = utxo.TxID, utxo.Vout, utxo.Amount
					st.SeqLegAsset = utxo.AssetID
					st.SeqLegScriptHex = hex.EncodeToString(script)
				}
			}
		}
	}
	if st.SeqLegTxid == "" || st.SeqLegScriptHex == "" {
		fmt.Printf("%s: submarine resume: no funded taker leg known (pre-announce session); the taker refunds itself at T_seq %d\n", name, st.SeqLocktime)
		return
	}
	script, err := hex.DecodeString(st.SeqLegScriptHex)
	if err != nil {
		fmt.Printf("%s: submarine resume: bad seq_leg_script_hex; skipping\n", name)
		return
	}
	refundPub, err := hex.DecodeString(st.TakerSeqRefundPub)
	if err != nil || len(refundPub) != 33 {
		fmt.Printf("%s: submarine resume: no taker refund pub to verify the script against; skipping\n", name)
		return
	}
	persist := func(preimage []byte, claimTx string, settled bool) {
		r := &client.MakerSubmarineResult{HashH: hashH, SeqClaimKey: claimKey, SeqLocktime: st.SeqLocktime,
			Preimage: preimage, SeqClaimTxid: claimTx, Settled: settled,
			TakerSeqRefundPub: refundPub, Bolt11: st.Bolt11, InvoiceMsat: st.InvoiceMsat, AssetHex: st.SeqLegAsset,
			SeqLeg: &xchain.LegLock{Script: script,
				Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
				Locktime: st.SeqLocktime}}
		persistXSessionSubmarine(dir, st.SessionID, st.OfferID, r)
	}
	// Persist any discovery immediately, even if the claim below fails.
	persist(mustHexOrNil(st.SecretHex), st.SeqClaimTxid, false)

	if st.SecretHex != "" {
		// Tier 1: paid, crashed before the claim - chain-only recovery.
		preimage, perr := hex.DecodeString(st.SecretHex)
		if perr != nil || len(preimage) != 32 {
			fmt.Printf("%s: submarine resume: bad persisted preimage; skipping\n", name)
			return
		}
		vseq, verr := sub.VerifySEQLeg(hashH, claimKey.PubKey(), refundPub, script,
			st.SeqLocktime, st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount, st.SeqLegAsset, 1)
		if verr != nil {
			fmt.Printf("%s: submarine resume: leg no longer verifies (likely already claimed): %v\n", name, verr)
			return
		}
		if ierr := sub.InjectSecret(preimage); ierr != nil {
			fmt.Printf("%s: submarine resume: inject secret: %v\n", name, ierr)
			return
		}
		claimTx, cerr := sub.ClaimSEQLeg(vseq.Leg, claimKey, spendFee)
		if cerr != nil {
			fmt.Printf("%s: submarine resume: claim (RETRYABLE, we hold P): %v\n", name, cerr)
			return
		}
		fmt.Printf("%s: SUBMARINE RESUMED + SETTLED: claimed the asset in %s\n", name, claimTx)
		persist(preimage, claimTx, true)
		return
	}
	if st.Bolt11 != "" && ln != nil {
		// Tier 2: leg funded, invoice persisted, never paid - re-run the full settle.
		fmt.Printf("%s: submarine resume: re-running verify -> anchor gate -> pay -> claim\n", name)
		nr, rerr := sub.RunNormal(xchain.NormalParams{
			HashH: hashH, MakerSeqClaimPub: claimKey.PubKey(), TakerSeqRefundPub: refundPub,
			SeqRedeemScript: script, SeqLocktime: st.SeqLocktime,
			SeqTxID: st.SeqLegTxid, SeqVout: st.SeqLegVout, SeqAmountAtoms: st.SeqLegAmount, SeqAssetID: st.SeqLegAsset,
			SeqMinConf: 1, Bolt11: st.Bolt11, InvoiceMsat: st.InvoiceMsat,
			MinAnchorDepth: 3, AnchorTimeout: 20 * time.Minute,
		}, claimKey, spendFee)
		if nr != nil && len(nr.Preimage) > 0 {
			persist(nr.Preimage, nr.SeqClaimTxID, rerr == nil)
		}
		if rerr != nil {
			fmt.Printf("%s: submarine resume: settle: %v\n", name, rerr)
			return
		}
		fmt.Printf("%s: SUBMARINE RESUMED + SETTLED: paid the invoice, claimed the asset in %s\n", name, nr.SeqClaimTxID)
		return
	}
	fmt.Printf("%s: submarine resume: leg known but no preimage and no -ln-socket to pay with; the taker refunds itself at T_seq %d\n", name, st.SeqLocktime)
}

// mustHexOrNil decodes hex or returns nil - persistence-path convenience only.
func mustHexOrNil(h string) []byte {
	if h == "" {
		return nil
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil
	}
	return b
}

// reportSettledTrade tells the relay a CROSS-CHAIN / LIGHTNING lift SETTLED, so the relay
// records the trade (feeding last_price / candles / the trades feed for BTC/LN pairs, which
// otherwise report last_price 0 with an empty chart) and decrements/finalizes the resting
// order. It is the durable-settlement analogue of the same-chain maker's SettleAck, but it is
// MAKER-SIGNED and keyed to the offer instead of a live session: a cross/LN swap can finish
// after the short co-sign session was swept (its on-chain / Lightning leg outlives the courier
// deadline) or after the maker reconnected (dropping the relay-side session role), so a
// session-scoped ack is unreachable at settlement time.
//
// anchorConfs is the Bitcoin-anchor depth the maker declares final for this fill: reporting the
// offer's own min_anchor_depth marks the maker's chosen finality bar met (the maker only reaches
// this settled state after its swap-level confirmation gates hold), so a min_anchor_depth>0 offer
// reaches FILLED; a later anchor orphan re-opens it (handled separately). Pure-LN offers carry
// min_anchor_depth 0, so their fills finalize immediately. Best-effort: a send failure is logged.
func reportSettledTrade(ws *crossWS, o *seqobv1.Offer, key *btcec.PrivateKey, fillBase uint64, settleTxid string) {
	if fillBase == 0 {
		return
	}
	st := &seqobv1.SettledTrade{
		OfferId:     o.GetOfferId(),
		MakerPubkey: o.GetMakerPubkey(),
		FillBase:    fillBase,
		SettleTxid:  settleTxid,
		AnchorConfs: o.GetMinAnchorDepth(),
		Nonce:       uint64(time.Now().UnixNano()),
	}
	if err := offer.SignSettledTrade(st, key); err != nil {
		fmt.Printf("settled-trade: sign failed: %v (trade NOT recorded)\n", err)
		return
	}
	if err := ws.write(&seqobv1.To{Msg: &seqobv1.To_SettledTrade{SettledTrade: st}}); err != nil {
		fmt.Printf("settled-trade: relay write failed: %v (trade NOT recorded)\n", err)
	}
}

// cancelOffer removes the filled resting offer from the book (signed cancel).
// This is the NON-requote terminal path: the maker quotes once and exits.
func cancelOffer(relay string, o *seqobv1.Offer, key *btcec.PrivateKey) {
	if err := postCancel(relay, o, key); err != nil {
		fmt.Printf("cancel offer: %v\n", err)
		return
	}
	fmt.Printf("offer %s cancelled after fill (restart the maker to re-quote)\n", o.GetOfferId())
}

// postCancel signs and POSTs an offer cancel to the relay, returning only when the
// relay has processed it (HTTP 200 => the offer is removed from the book). The
// nonce is the wall-clock nanosecond, which is strictly increasing across calls,
// so each cancel beats the store's replay guard and a re-posted same offer_id can
// be cancelled again on the next fill.
func postCancel(relay string, o *seqobv1.Offer, key *btcec.PrivateKey) error {
	c := &seqobv1.OfferCancel{OfferId: o.GetOfferId(), Nonce: uint64(time.Now().UnixNano())}
	if err := offer.SignCancel(c, key); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	b, err := jsonMarshal.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	resp, err := http.Post(relay+"/v1/offers/cancel", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// refreshOfferForRequote rolls an offer forward for a fresh quote: it moves the
// created/expires window to [now, now+expiry) and re-signs it (the signature
// covers those fields), leaving the offer_id, pair, amounts and settlement terms
// untouched. Returns the error from re-signing, if any. It is deliberately pure
// (no I/O) so the re-quote refresh is unit-testable on its own.
func refreshOfferForRequote(o *seqobv1.Offer, expiry time.Duration, key *btcec.PrivateKey) error {
	now := time.Now()
	o.CreatedAtUnix = uint64(now.Unix())
	o.ExpiresAtUnix = uint64(now.Add(expiry).Unix())
	return offer.SignOffer(o, key)
}

// requoteAfterFill keeps a maker's quote continuously live after a settled fill,
// replacing the old cancel-and-exit behaviour when -requote is set. It is called
// from a settle goroutine's defer WHILE that goroutine still holds the single
// in-flight slot, so the serve loop refuses any new lift as "busy" until the fresh
// quote is up: the maker can neither double-post (there is never a second offer_id)
// nor oversell (only one offer rests at a time, at full size). Steps:
//
//  1. reconnect any dropped channel peers so the next leg can actually route
//     (the maker's asset/BTC LN nodes silently drop peers between fills);
//  2. refresh the offer's created/expiry window and re-sign it;
//  3. cancel the just-filled resting offer, then re-submit the SAME offer_id.
//
// Cancel-then-resubmit (rather than an in-place edit) is used on purpose: it is
// robust even if the relay's expiry sweeper already reaped the offer between the
// lift and settlement — the cancel then simply no-ops and the submit re-adds it —
// and the submit path re-registers this connection as the maker's lift endpoint.
// The cancel is synchronous (returns only after HTTP 200 => removed), so the
// submit that follows can never collide with the old resting copy.
func requoteAfterFill(ws *crossWS, wsURL string, o *seqobv1.Offer, relay string, key *btcec.PrivateKey, expiry time.Duration, reconnect func()) {
	if reconnect != nil {
		reconnect()
	}
	if err := refreshOfferForRequote(o, expiry, key); err != nil {
		fmt.Printf("requote: re-sign failed: %v (offer NOT re-posted)\n", err)
		return
	}
	// Remove the filled copy first (idempotent if already swept), then re-post.
	if err := postCancel(relay, o, key); err != nil {
		fmt.Printf("requote: cancel of filled offer failed (continuing to re-post): %v\n", err)
	}
	if err := ws.write(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}}); err != nil {
		fmt.Printf("requote: re-submit failed: %v; reconnecting relay + re-posting\n", err)
		ws.redialLoop(wsURL, o)
	}
	fmt.Printf("re-quoted: offer %s live again (%d %s), fresh expiry; awaiting the next lift\n",
		o.GetOfferId(), o.GetBaseAmount(), o.GetOfferAsset())
}

// rpcFromURL parses http://user:pass@host:port into an xchain RPC client.
func rpcFromURL(raw string) (*xchain.RPC, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User == nil {
		return nil, fmt.Errorf("expected http://user:pass@host:port, got %q", raw)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return nil, fmt.Errorf("bad port in %q", raw)
	}
	pass, _ := u.User.Password()
	return xchain.NewRPC(u.Hostname(), port, u.User.Username(), pass), nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
