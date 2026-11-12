package main

// submarine.go adds the SUBMARINE-SWAP (Sequentia asset on-chain <-> BTC over
// Lightning) maker mode to seqob-maker. It is the Lightning sibling of the CROSS
// mode (runCrossMaker): it posts a signed LightningTerms offer and serves each
// lift over the same relay courier. Both plugin-free directions are wired,
// selected by -side:
//   - -side buy  (NORMAL): the maker BUYS the asset for BTC-LN — it pays the
//     taker's BOLT11 and claims the asset (serve -> RunMakerSubmarineNormal).
//   - -side sell (REVERSE, maker-secret): the maker SELLS the asset — it locks the
//     asset HTLC (claim=taker) and issues a plain invoice; the taker pays it and
//     claims the asset (serve -> RunMakerReverseSubmarine).
// It needs no hold-invoice plugin and no BTC chain backend — only the Sequentia
// node (asset leg) and the maker's SeqLN-on-Bitcoin lightning-rpc (BTC-LN leg).

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type submarineMakerConfig struct {
	relay       string
	makerKey    *btcec.PrivateKey
	makerPubHex string
	makerPubKey []byte // 33-byte compressed (advisory LightningTerms keys)
	asset       string // the SEQ asset the maker buys (base)
	assetAmt    uint64 // asset atoms the maker acquires
	btcSats     uint64 // BTC-LN sats the maker pays (converted to msat per-lift)
	feeAsset    string
	expiry      time.Duration
	minAnchor   uint32 // offer.min_anchor_depth (finality DISPLAY only)
	offerID     string
	seqRPCURL   string
	seqWallet   string
	lnSocket    string // the maker's SeqLN-on-Bitcoin lightning-rpc
	seqDelta    uint32 // T_seq = SEQ tip + this
	subAnchor   int64  // the submarine cross-leg anchor-depth gate (>=2)
	onchainCltv uint32 // advisory CLTV in the resting LightningTerms
	spendFee    uint64 // maker asset-claim fee target (native sats; sized per-asset)
	max0conf    uint64 // 0-conf LP-fronting cap (asset atoms): trades <= it settle instantly
	reverse     bool   // true = SELL the asset for BTC-LN (maker-secret REVERSE); false = BUY (NORMAL)
	requote     bool   // true = re-post a fresh offer after each settled fill instead of exiting
	stateDir    string // per-lift session persistence (keys/legs; the recovery material)
}

// buildSubmarineOffer builds a Lightning offer (base=asset, quote=the BTC
// sentinel). The resting LightningTerms keys are ADVISORY (the load-bearing
// SEQ-claim key is minted per-lift over the E2E courier); maker_ln_node_pubkey
// advertises the maker's LN node. Two directions:
//   - NORMAL (cfg.reverse=false): the maker PAYS the taker's BOLT11 and CLAIMS the
//     asset, so the maker BUYS the asset (trade_dir=BUY, ln_direction=LnAssetForBTC).
//   - REVERSE (cfg.reverse=true, maker-secret): the maker LOCKS the asset (claim=
//     taker) and issues a plain invoice, so the maker SELLS the asset (trade_dir=
//     SELL, ln_direction=LnBTCForAsset).
func buildSubmarineOffer(cfg submarineMakerConfig, makerLnNodeID string) *seqobv1.Offer {
	o := &seqobv1.Offer{
		OfferId:           orDefault(cfg.offerID, randstr.Hex(16)),
		SchemaVersion:     1,
		Pair:              &seqobv1.AssetPair{BaseAsset: cfg.asset, QuoteAsset: offer.BTCSentinel},
		BaseAmount:        cfg.assetAmt,
		AllowPartial:      true, // a taker may take a slice; the remainder is re-rested after the fill
		CreatedAtUnix:     uint64(time.Now().Unix()),
		ExpiresAtUnix:     uint64(time.Now().Add(cfg.expiry).Unix()),
		FeeAssetHint:      cfg.feeAsset,
		MinAnchorDepth:    cfg.minAnchor,
		MakerLnNodePubkey: makerLnNodeID,
		Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
			MakerClaimPub:          cfg.makerPubKey,
			MakerRefundPub:         cfg.makerPubKey,
			OnchainCltv:            cfg.onchainCltv,
			MakerIssuesHoldInvoice: false,        // both v1 modes are plugin-free
			Max_0ConfAmount:        cfg.max0conf, // advertise the 0-conf cap so takers can front small amounts
		}},
	}
	if cfg.reverse {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL // the maker gives up the asset
		o.OfferAsset, o.OfferAmount = cfg.asset, cfg.assetAmt
		o.WantAsset, o.WantAmount = offer.BTCSentinel, cfg.btcSats
		o.GetLightning().LnDirection = offer.LnBTCForAsset
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY // the maker acquires the asset
		o.OfferAsset, o.OfferAmount = offer.BTCSentinel, cfg.btcSats
		o.WantAsset, o.WantAmount = cfg.asset, cfg.assetAmt
		o.GetLightning().LnDirection = offer.LnAssetForBTC
	}
	return o
}

// runSubmarineMaker posts a Lightning offer and serves NORMAL submarine lifts. It
// needs no Ocean wallet and no bitcoind: the SEQ asset leg is funded from the
// Sequentia NODE wallet, and the BTC-LN it receives lands in the maker's SeqLN
// channel balance.
func runSubmarineMaker(cfg submarineMakerConfig) {
	if cfg.seqRPCURL == "" {
		fatal("-mode lightning requires -xseq-rpc (the Sequentia node RPC)")
	}
	if cfg.lnSocket == "" {
		fatal("-mode lightning requires -ln-socket (the maker's SeqLN lightning-rpc)")
	}
	seqRPC, err := rpcFromURL(cfg.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, cfg.seqWallet)
	if _, err := seqChain.BlockCount(); err != nil {
		fatal("sequentia node unreachable: %v", err)
	}
	// Validate the LN node up front and advertise its id in the offer.
	lnID, err := xchain.NewCLNLNLeg(cfg.lnSocket).NodeID()
	if err != nil {
		fatal("lightning-rpc %s unreachable: %v", cfg.lnSocket, err)
	}

	o := buildSubmarineOffer(cfg, lnID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	if cfg.reverse {
		// The reverse-submarine T_seq is COUPLED to the invoice min_final_cltv (not cfg.seqDelta,
		// which sizes the cross W1/W2 leg); RunMakerReverseSubmarine raises a short delta to the
		// coupled minimum. Print the effective coupled values so the operator sees what takers gate on.
		effDelta := cfg.seqDelta
		if effDelta < client.SubReverseSeqLocktimeDelta {
			effDelta = client.SubReverseSeqLocktimeDelta
		}
		fmt.Printf("seqob-maker up (LIGHTNING/submarine): posted SELL offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker sells %d %s for up to %d BTC sats over Lightning (REVERSE maker-secret: taker buys the asset)  T_seq=+%d min-final-cltv=%d min-anchor-depth=%d max-0conf=%d  ln-node=%s\n",
			cfg.assetAmt, cfg.asset, cfg.btcSats, effDelta, client.SubReverseInvoiceCLTV, cfg.subAnchor, cfg.max0conf, lnID)
		fmt.Printf("  taker lifts with: seqob-cli xsubbuy -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), cfg.makerPubHex)
	} else {
		fmt.Printf("seqob-maker up (LIGHTNING/submarine): posted BUY offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker pays up to %d BTC sats over Lightning for %d %s (NORMAL: taker sells the asset)  T_seq=+%d min-anchor-depth=%d max-0conf=%d  ln-node=%s\n",
			cfg.btcSats, cfg.assetAmt, cfg.asset, cfg.seqDelta, cfg.subAnchor, cfg.max0conf, lnID)
		fmt.Printf("  taker lifts with: seqob-cli xsublift -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), cfg.makerPubHex)
	}

	serveSubmarine(ws, wsURL, o, cfg, seqChain)
}

// serveSubmarine is the Lightning-mode event loop: each lift gets its own
// goroutine running RunMakerSubmarineNormal; the loop routes sealed courier
// frames to the session's inbox. Same whole-HTLC discipline as serveCross: ONE
// lift in flight, and the offer is cancelled after its first settlement.
func serveSubmarine(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg submarineMakerConfig, seqChain *xchain.Chain) {
	var mu sync.Mutex
	inboxes := make(map[string]chan []byte)
	inFlight := 0
	filled := false
	idle := func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 }
	armRequoteExit(o, idle) // retire for a fresh re-quote before the offer expires
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })

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
				resubmit = nil
			}
			mu.Unlock()
			if done {
				fmt.Println("offer filled and no lift in flight; exiting (restart to re-quote)")
				return
			}
			fmt.Printf("ws read error: %v; reconnecting (in-flight settlements continue)\n", err)
			ws.redialLoop(wsURL, resubmit)
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
			// A lift NAMES the offer it takes, but the relay routes it to this maker by PUBKEY —
			// and a stable identity key can be shared by siblings (a requote overlap, a stray
			// process). Negotiating someone else's offer with OUR terms rejects the taker only
			// AFTER it may have committed funds ("btc leg X != required Y", seen live). Refuse
			// up front instead: an honest, instant "wrong maker process" costs the taker nothing.
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
			if refuseIfDraining(sid, cr, refuse) {
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				mu.Unlock()
				continue
			}
			fmt.Printf("submarine lift requested: session %s offer %s\n", sid, lr.GetOfferId())

			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }

			go func(sid string, in chan []byte) {
				settled := false
				var settleTxid string
				// A lift may take a SLICE. filledSeq is what this one settled; partial says
				// the offer still has size left, which we re-rest rather than retiring.
				var filledSeq uint64
				partial := false
				defer func() {
					// Record the executed fill FIRST, before requote/cancel touch the still-resting
					// offer: the relay records the trade (last_price + chart for the asset/BTC pair)
					// and decrements/finalizes the order. Maker-signed + offer-keyed, so it lands even
					// though the courier session may already be gone.
					if settled {
						fill := filledSeq
						if fill == 0 {
							fill = o.GetBaseAmount()
						}
						reportSettledTrade(ws, o, cfg.makerKey, fill, settleTxid)
					}
					// -requote: re-post a fresh quote while still holding the in-flight slot
					// (racing lifts are refused as "busy" until it is live -> no double-post).
					// The asset leg is on-chain; only the BTC-LN leg has channel peers to
					// reconnect before the next lift's pay.
					// PARTIAL FILL: re-rest the REMAINDER, priced at the offer's own rate.
					// Without this the maker would re-post the FULL size after selling a slice,
					// advertising asset it no longer has.
					if settled && partial {
						mu.Lock()
						remain := o.GetBaseAmount() - filledSeq
						if o.GetLightning().GetLnDirection() == offer.LnBTCForAsset {
							// The maker SELLS the asset: OfferAmount=asset given, WantAmount=BTC wanted.
							o.WantAmount = client.ProportionalBtc(o.GetWantAmount(), remain, o.GetBaseAmount())
							o.OfferAmount = remain
						} else {
							// The maker BUYS the asset: OfferAmount=BTC given, WantAmount=asset wanted.
							o.OfferAmount = client.ProportionalBtcFloor(o.GetOfferAmount(), remain, o.GetBaseAmount())
							o.WantAmount = remain
						}
						o.BaseAmount = remain
						mu.Unlock()
						fmt.Printf("session %s: PARTIAL fill (%d of %d atoms); re-resting the remainder %d\n",
							sid, filledSeq, filledSeq+remain, remain)
					}
					if settled && (cfg.requote || partial) {
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, func() {
							if n, err := xchain.NewCLNLNLeg(cfg.lnSocket).ReconnectPeers(); err != nil {
								fmt.Printf("requote: BTC-LN peer reconnect: reconnected %d, err: %v\n", n, err)
							} else if n > 0 {
								fmt.Printf("requote: reconnected %d BTC-LN peer(s)\n", n)
							}
						})
					}
					mu.Lock()
					inFlight--
					delete(inboxes, sid)
					if settled && !cfg.requote && !partial {
						filled = true
					}
					mu.Unlock()
					if settled && !cfg.requote && !partial {
						cancelOffer(cfg.relay, o, cfg.makerKey)
						exitAfterFill(idle)
					}
					requoteExitIfPending(idle)
				}()
				if o.GetLightning().GetLnDirection() == offer.LnBTCForAsset {
					// REVERSE (maker-secret): the maker locks the asset + issues a
					// plain invoice; the taker pays it and claims the asset.
					p := client.MakerReverseSubmarineParams{
						NewMakerOps: func(secret []byte) client.SubReverseMakerOps {
							sub := xchain.NewSubmarineSwap(seqChain, xchain.NewCLNLNLeg(cfg.lnSocket), xchain.NewHashLock(secret))
							return &client.LiveSubReverseMakerOps{Sub: sub}
						},
						Crypter:     cr,
						SeqTip:      seqChain.BlockCount,
						AssetHex:    o.GetPair().GetBaseAsset(),
						SeqAmount:   o.GetOfferAmount(),       // the asset the maker sells
						InvoiceMsat: o.GetWantAmount() * 1000, // BTC sats wanted -> msat
						// T_seq and the invoice min_final_cltv are COUPLED from ONE invariant so an honest
						// offer clears the taker's hold-CLTV masquerade gate. This is the single-chain
						// submarine gate, NOT the cross W1/W2 delta (cfg.seqDelta) — coupleSubReverse inside
						// RunMakerReverseSubmarine raises T_seq to the coupled minimum if cfg.seqDelta is short.
						SeqLocktimeDelta: cfg.seqDelta,
						InvoiceCLTV:      client.SubReverseInvoiceCLTV,
						Log:              logf,
						// The refund key is derived from the identity key and the leg is persisted
						// the moment it is locked: a per-swap random key that lived only in this
						// process took the locked asset with it whenever the supervisor re-quoted
						// or killed the maker before the taker paid.
						SeqRefundKey: derivedRefundKey(cfg.makerKey, "submarine-reverse"),
						OnUpdate: func(r *client.MakerReverseSubmarineResult) {
							persistXSessionSubmarineReverse(cfg.stateDir, sid, o.GetOfferId(), r)
						},
					}
					res, err := client.RunMakerReverseSubmarine(p, in, send)
					if err != nil {
						fmt.Printf("session %s: reverse submarine lift ended: %v\n", sid, err)
						if res != nil && res.SeqLeg != nil {
							fmt.Printf("session %s: asset %s:%d refundable after T_seq=%d if unpaid (refund key derived from -maker-priv; leg persisted under %s)\n",
								sid, res.SeqLeg.Funded.TxID, res.SeqLeg.Funded.Vout, res.SeqLocktime, cfg.stateDir)
						}
						return
					}
					settled = true
					filledSeq = res.FilledSeq
					partial = res.RemainderSeq > 0
					fmt.Printf("session %s: REVERSE SUBMARINE SWAP SETTLED: %d of %d atoms for %d msat over BTC-LN; the taker claimed the asset\n",
						sid, res.FilledSeq, o.GetBaseAmount(), res.PaidMsat)
					return
				}
				// NORMAL: the taker funds the asset HTLC + hands us a BOLT11; we
				// pay it (learning P) and claim the asset.
				p := client.MakerSubmarineParams{
					NewMakerOps: func(hashH []byte) client.SubMakerOps {
						sub := xchain.NewSubmarineSwap(seqChain, xchain.NewCLNLNLeg(cfg.lnSocket), xchain.NewHashLockFromHash(hashH))
						return &client.LiveSubMakerOps{Sub: sub}
					},
					Crypter:          cr,
					SeqTip:           seqChain.BlockCount,
					AssetHex:         o.GetPair().GetBaseAsset(),
					SeqAmount:        o.GetWantAmount(),         // the asset the maker acquires
					InvoiceMsat:      o.GetOfferAmount() * 1000, // BTC sats the maker pays -> msat
					SeqLocktimeDelta: cfg.seqDelta,
					MinAnchorDepth:   cfg.subAnchor,
					Max0ConfAmount:   cfg.max0conf,
					SpendFeeAtoms:    cfg.spendFee,
					Log:              logf,
					// Persist at every value transition. Without this a requote-killed
					// submarine maker took its per-lift claim key to the grave and a
					// funded taker leg became permanently unspendable (2026-08-11).
					OnUpdate: func(r *client.MakerSubmarineResult) {
						persistXSessionSubmarine(cfg.stateDir, sid, o.GetOfferId(), r)
					},
				}
				res, err := client.RunMakerSubmarineNormal(p, in, send)
				if err != nil {
					fmt.Printf("session %s: submarine lift ended: %v\n", sid, err)
					if res != nil && res.SeqClaimTxid != "" {
						fmt.Printf("session %s: (asset claimed in %s despite error)\n", sid, res.SeqClaimTxid)
					}
					return
				}
				settled = true
				settleTxid = res.SeqClaimTxid
				filledSeq = res.FilledSeq
				partial = res.RemainderSeq > 0
				fmt.Printf("session %s: SUBMARINE SWAP SETTLED: paid the taker's BTC-LN for %d of %d atoms, claimed the asset in %s\n",
					sid, res.FilledSeq, o.GetBaseAmount(), res.SeqClaimTxid)
			}(sid, in)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				fmt.Printf("session %s: swap_msg without a live submarine session; ignoring\n", sm.GetSessionId())
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
			releaseDetachedSession(e, &mu, inboxes)
			if offerRejected(e.GetCode(), e.GetMessage()) {
				requoteExitIfIdle("relay rejected our offer ("+e.GetMessage()+")", idle)
			}
		}
	}
}
