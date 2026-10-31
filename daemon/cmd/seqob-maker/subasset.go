package main

// subasset.go adds the SUB-ASSET maker mode to seqob-maker: the MIRROR of the
// submarine mode. Here the asset leg is over LIGHTNING and the BTC leg is an
// ON-CHAIN HTLC, so a taker pays BTC on-chain and receives the asset over
// Lightning. It glues two proven primitives:
//   - the real-bitcoind on-chain BTC HTLC from the CROSS mode (xchain.NewSwapBitcoin:
//     VerifyBTCLeg / ClaimBTCLeg), and
//   - the asset Lightning hold invoice from the PURELN mode (an asset LNLeg:
//     the maker PAYS the taker's hold invoice, learning P).
//
// Only the maker-SELL flow is defined (the directive's shape): the maker gives the
// asset over LN and receives BTC in the taker's on-chain HTLC. It needs a bitcoind
// (-btc-rpc / -btc-chain, to verify + claim the BTC HTLC) and the maker's
// SeqLN-on-Sequentia asset lightning-rpc (-asset-ln-socket, to pay the asset). It
// needs NO Sequentia node RPC and NO BTC-LN socket.

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

type subAssetMakerConfig struct {
	relay        string
	makerKey     *btcec.PrivateKey
	makerPubHex  string
	makerPubKey  []byte // 33-byte compressed (advisory LightningTerms keys)
	asset        string // the SEQ asset the maker sells (base), and the asset LN leg's id
	assetAmt     uint64 // asset atoms the maker pays over Lightning
	btcSats      uint64 // BTC sats the taker locks on-chain (the maker receives)
	feeAsset     string
	expiry       time.Duration
	minAnchor    uint32 // offer.min_anchor_depth (display only; the BTC leg is anchor-final by construction)
	offerID      string
	btcRPCURL    string // bitcoind RPC URL (verify + claim the on-chain BTC HTLC)
	btcWallet    string // bitcoind wallet that receives the claimed BTC
	btcChainName string
	// Mixed same-chain (rails 7/8): when quoteAsset is a real Sequentia asset id,
	// the "BTC leg" is an on-chain HTLC on THAT asset on the Sequentia chain
	// (verified/claimed via seqRPCURL/seqWallet, Elements format; heights and
	// btcDelta are SEQUENTIA heights) and bitcoind is not used at all. Empty =
	// the legacy BTC-sentinel sub-asset shape.
	quoteAsset  string
	seqRPCURL   string
	seqWallet   string
	assetLnSock string        // the maker's SeqLN-on-Sequentia lightning-rpc (asset leg)
	btcDelta    uint32        // T_btc = parent tip + this (the taker's on-chain refund CLTV)
	minBTCConf  int           // confirmations required on the taker's BTC leg before paying the asset
	spendFee    uint64        // BTC HTLC claim fee target (native sats)
	holdTimeout time.Duration // how long the maker waits for the taker to settle after it pays
	requote     bool          // true = re-post a fresh offer after each settled fill instead of exiting
	stateDir    string        // per-lift session persistence (P + the taker's on-chain leg once paid)
}

// buildSubAssetOffer builds a Lightning offer (base=asset, quote=the BTC sentinel)
// with ln_direction=LnAssetLNForBTC. The maker SELLS the asset (trade_dir=SELL):
// it gives the asset over LN and receives BTC in the taker's on-chain HTLC.
// maker_ln_node_pubkey advertises the maker's asset LN node (informational; the
// maker PAYS the taker's invoice, so the taker never dials it).
func buildSubAssetOffer(cfg subAssetMakerConfig, assetLnNodeID string) *seqobv1.Offer {
	// Mixed same-chain (rails 7/8): advertise the TRUE quote asset; ln_direction
	// stays LnAssetLNForBTC — the quote asset stands in BTC's structural place
	// (base over Lightning, quote in an on-chain HTLC on the Sequentia chain).
	quote := offer.BTCSentinel
	if cfg.quoteAsset != "" {
		quote = cfg.quoteAsset
	}
	o := &seqobv1.Offer{
		OfferId:           orDefault(cfg.offerID, randstr.Hex(16)),
		SchemaVersion:     1,
		Pair:              &seqobv1.AssetPair{BaseAsset: cfg.asset, QuoteAsset: quote},
		BaseAmount:        cfg.assetAmt,
		AllowPartial:      true, // T8: a taker may take a slice; the serve loop re-rests the remainder
		CreatedAtUnix:     uint64(time.Now().Unix()),
		ExpiresAtUnix:     uint64(time.Now().Add(cfg.expiry).Unix()),
		FeeAssetHint:      cfg.feeAsset,
		MinAnchorDepth:    cfg.minAnchor,
		MakerLnNodePubkey: assetLnNodeID,
		TradeDir:          seqobv1.TradeDir_TRADE_DIR_SELL, // the maker gives up the asset
		Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
			MakerClaimPub:          cfg.makerPubKey,
			MakerRefundPub:         cfg.makerPubKey,
			OnchainCltv:            cfg.btcDelta,
			MakerIssuesHoldInvoice: false, // the TAKER issues the asset hold invoice
			LnDirection:            offer.LnAssetLNForBTC,
		}},
	}
	o.OfferAsset, o.OfferAmount = cfg.asset, cfg.assetAmt
	o.WantAsset, o.WantAmount = quote, cfg.btcSats
	return o
}

// runSubAssetMaker posts a sub-asset offer and serves swaps. It needs no Ocean
// wallet and no Sequentia node: the asset leg is the maker's SeqLN-on-Sequentia
// node, and the BTC it receives lands on-chain in the maker's bitcoind wallet.
func runSubAssetMaker(cfg subAssetMakerConfig) {
	if cfg.assetLnSock == "" {
		fatal("-mode subasset requires -asset-ln-socket (the maker's SeqLN-on-Sequentia lightning-rpc)")
	}
	// Chain seams: where the on-chain leg lives. Legacy = bitcoind (real BTC);
	// mixed same-chain (-quote-asset) = the Sequentia chain, HTLC on the quote
	// asset, Elements format. The serve loop only ever sees these two closures.
	var (
		tip    func() (int64, error)
		newOps func(hashH []byte) client.SubAssetMakerOps
	)
	if cfg.quoteAsset != "" {
		if cfg.seqRPCURL == "" {
			fatal("-mode subasset with -quote-asset requires -xseq-rpc (the Sequentia node that verifies + claims the on-chain quote-asset HTLC)")
		}
		seqRPC, err := rpcFromURL(cfg.seqRPCURL)
		if err != nil {
			fatal("-xseq-rpc: %v", err)
		}
		seqChain := xchain.NewChain(seqRPC, cfg.seqWallet)
		if _, err := seqChain.BlockCount(); err != nil {
			fatal("sequentia node unreachable: %v", err)
		}
		tip = seqChain.BlockCount
		newOps = func(hashH []byte) client.SubAssetMakerOps {
			return client.NewLiveSubAssetMakerOpsSeq(seqChain, cfg.quoteAsset,
				xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset), hashH)
		}
	} else {
		if cfg.btcRPCURL == "" {
			fatal("-mode subasset requires -btc-rpc (the bitcoind RPC that verifies + claims the on-chain BTC HTLC)")
		}
		btcRPC, err := rpcFromURL(cfg.btcRPCURL)
		if err != nil {
			fatal("-btc-rpc: %v", err)
		}
		params, err := xchain.BitcoinChainParams(cfg.btcChainName)
		if err != nil {
			fatal("-btc-chain: %v", err)
		}
		btcChain := xchain.NewBitcoinChain(btcRPC, cfg.btcWallet, params)
		if _, err := btcChain.BlockCount(); err != nil {
			fatal("bitcoind unreachable: %v", err)
		}
		tip = btcChain.BlockCount
		newOps = func(hashH []byte) client.SubAssetMakerOps {
			return client.NewLiveSubAssetMakerOps(btcChain,
				xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset), hashH)
		}
	}
	// Validate the asset LN node up front and advertise its id in the offer.
	assetID, err := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset).NodeID()
	if err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", cfg.assetLnSock, err)
	}

	o := buildSubAssetOffer(cfg, assetID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	if cfg.quoteAsset != "" {
		fmt.Printf("seqob-maker up (MIXED SAME-CHAIN): posted SELL offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker sells %d %s over Lightning for %d %s atoms in an on-chain Sequentia HTLC (taker pays the quote asset on-chain, receives the base over LN)  T_seq=+%d min-conf=%d  asset-node=%s\n",
			cfg.assetAmt, cfg.asset, cfg.btcSats, cfg.quoteAsset, cfg.btcDelta, cfg.minBTCConf, assetID)
	} else {
		fmt.Printf("seqob-maker up (SUB-ASSET): posted SELL offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker sells %d %s over Lightning for %d BTC sats on-chain (taker pays BTC on-chain, receives the asset over LN)  T_btc=+%d min-btc-conf=%d  asset-node=%s btc-chain=%s\n",
			cfg.assetAmt, cfg.asset, cfg.btcSats, cfg.btcDelta, cfg.minBTCConf, assetID, cfg.btcChainName)
	}
	fmt.Printf("  taker lifts with: seqob-cli xsubas -asset %s -offer-id %s -maker-pubkey %s\n", cfg.asset, o.GetOfferId(), cfg.makerPubHex)

	serveSubAsset(ws, wsURL, o, cfg, tip, newOps, assetID)
}

// serveSubAsset is the sub-asset event loop: each swap gets its own goroutine
// running RunMakerSubAsset; the loop routes sealed courier frames to the session's
// inbox. Same whole-swap discipline as serveSubmarine: ONE swap in flight, and the
// offer is cancelled after its first settlement (unless -requote).
func serveSubAsset(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg subAssetMakerConfig,
	tip func() (int64, error), newOps func(hashH []byte) client.SubAssetMakerOps, assetLNID string) {
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
				fmt.Println("offer filled and no swap in flight; exiting (restart to re-quote)")
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
				refuse(sid, cr, "busy", "another swap is in flight (whole-swap, one at a time)")
				continue
			}
			if refuseIfDraining(sid, cr, refuse) {
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				mu.Unlock()
				continue
			}
			fmt.Printf("sub-asset swap requested: session %s offer %s\n", sid, lr.GetOfferId())

			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }

			go func(sid string, in chan []byte) {
				settled := false
				var filledAsset, filledBtc uint64
				var settleTxid string
				defer func() {
					// Record the executed fill FIRST, before the partial-remainder re-rest or the
					// requote/cancel mutate/remove the still-resting original offer: the relay records
					// the trade (last_price + chart for the asset/BTC pair) at the original price and
					// decrements by exactly filledAsset (the base atoms actually taken; a sub-asset lift
					// may be partial). Maker-signed + offer-keyed, so it lands with the session gone.
					if settled {
						reportSettledTrade(ws, o, cfg.makerKey, filledAsset, settleTxid)
					}
					// Only the asset-LN leg has channel peers to reconnect (the BTC leg is
					// on-chain), so reconnect the asset LN node before re-quoting.
					reconnect := func() {
						if n, err := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset).ReconnectPeers(); err != nil {
							fmt.Printf("requote: asset-LN peer reconnect: reconnected %d, err: %v\n", n, err)
						} else if n > 0 {
							fmt.Printf("requote: reconnected %d asset-LN peer(s)\n", n)
						}
					}
					// T8 partial fill: the taker took only part of the offer, so reduce it to the
					// remainder and re-quote (there is more to sell, regardless of -requote);
					// requoteAfterFill re-signs + re-posts the shrunk offer. A whole fill keeps the
					// old behavior (-requote re-posts the whole; otherwise the offer is retired).
					partial := settled && filledAsset > 0 && filledAsset < o.GetBaseAmount()
					if partial {
						remainAsset := o.GetBaseAmount() - filledAsset
						// Price the remainder at the offer's OWN rate (ceil), not by subtracting the
						// rounded filledBtc — that subtraction drifts and can zero the want on a
						// low-priced offer, re-resting real asset for 0 BTC (free-drainable / unpriceable).
						// ProportionalBtc rounds up, so for any remainAsset>0 the want stays >=1.
						mu.Lock()
						o.WantAmount = client.ProportionalBtc(o.GetWantAmount(), remainAsset, o.GetBaseAmount())
						o.BaseAmount = remainAsset
						o.OfferAmount = remainAsset
						mu.Unlock()
						fmt.Printf("session %s: PARTIAL fill (%d asset, %d sats); re-resting the remainder %d %s for %d sats\n",
							sid, filledAsset, filledBtc, o.GetBaseAmount(), o.GetOfferAsset(), o.GetWantAmount())
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, reconnect)
					} else if settled && cfg.requote {
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

				// The maker builds the on-chain-leg swap with a hash-only lock (it
				// learns P by paying the asset over LN). T_btc is minted per-swap
				// from the current on-chain-leg tip (parent BTC tip for the legacy
				// shape; SEQUENTIA tip for the mixed same-chain shape).
				h, err := tip()
				if err != nil {
					fmt.Printf("session %s: on-chain-leg tip: %v\n", sid, err)
					return
				}
				tBtc := uint32(h) + cfg.btcDelta
				p := client.MakerSubAssetParams{
					// Bind the on-chain-leg swap to the taker's H once it arrives (the
					// hashlock must embed H for VerifyBTCLeg/ClaimBTCLeg).
					NewMakerOps:   newOps,
					AssetLNNodeID: assetLNID,
					Crypter:       cr,
					// Claim the taker's BTC HTLC with the maker's IDENTITY key, so its pubkey
					// equals the offer's LightningTerms.MakerClaimPub (= cfg.makerPubKey) that a
					// device-funded (external-BTC) HTLC is locked to BEFORE the courier handshake.
					// Without this the maker would mint an ephemeral key and VerifyBTCLeg would
					// fail on any device-funded leg (script mismatch).
					MakerBtcClaimKey: xchain.KeyFromBytes(cfg.makerKey.Serialize()),
					BtcAmount:        o.GetWantAmount(),  // BTC sats the taker locks on-chain
					AssetAmount:      o.GetOfferAmount(), // asset atoms the maker pays over LN
					BtcLocktime:      tBtc,
					MinBTCConf:       cfg.minBTCConf,
					SpendFeeSats:     cfg.spendFee,
					HoldTimeout:      cfg.holdTimeout,
					Log:              logf,
					// P is the only key to the taker's on-chain leg; write it down before the
					// claim so a crash in between is a retry, not a loss.
					OnPaid: func(preimage []byte, leg *xchain.LegLock, tBtc uint32) {
						persistXSessionSubAsset(cfg.stateDir, sid, o.GetOfferId(), preimage, leg, tBtc, cfg.quoteAsset, false, "")
					},
				}
				res, err := client.RunMakerSubAsset(p, in, send)
				if res != nil && len(res.Preimage) > 0 && res.BtcLeg != nil {
					persistXSessionSubAsset(cfg.stateDir, sid, o.GetOfferId(), res.Preimage, res.BtcLeg, res.BtcLocktime, cfg.quoteAsset, res.Settled, res.BtcClaimTxid)
				}
				if err != nil {
					fmt.Printf("session %s: sub-asset swap ended: %v\n", sid, err)
					if res != nil && len(res.Preimage) > 0 && res.BtcClaimTxid == "" {
						fmt.Printf("session %s: (maker holds P; retry ClaimBTCLeg before T_btc=%d)\n", sid, tBtc)
					}
					return
				}
				settled = res.Settled
				filledAsset, filledBtc = res.FilledAsset, res.FilledBtc
				settleTxid = res.BtcClaimTxid
				fmt.Printf("session %s: SUB-ASSET SWAP SETTLED: paid %d asset over LN, claimed %d sats BTC on-chain in %s\n",
					sid, res.FilledAsset, res.FilledBtc, res.BtcClaimTxid)
			}(sid, in)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				fmt.Printf("session %s: swap_msg without a live sub-asset session; ignoring\n", sm.GetSessionId())
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
