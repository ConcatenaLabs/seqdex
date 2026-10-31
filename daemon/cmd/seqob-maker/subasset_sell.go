package main

// subasset_sell.go adds the SUB-ASSET SELL maker mode to seqob-maker: the maker BUYS
// the asset. It LOCKS BTC in an on-chain HTLC (claim=taker with P, refund=maker) and
// holds an asset invoice on H; the taker pays the asset over Lightning and claims the
// BTC. It is the MIRROR of subasset.go (the sub-asset BUY maker). It needs a bitcoind
// (-btc-rpc / -btc-wallet, to LOCK + refund the BTC HTLC) and an asset LN node with the
// holdinvoice plugin (-asset-ln-socket, to hold + settle the asset invoice).

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

type subAssetSellMakerConfig struct {
	relay        string
	makerKey     *btcec.PrivateKey
	makerPubHex  string
	makerPubKey  []byte
	asset        string // the SEQ asset the maker BUYS (base), and the asset LN leg's id
	assetAmt     uint64 // asset atoms the maker receives over LN
	btcSats      uint64 // BTC sats the maker locks on-chain (the taker claims)
	feeAsset     string
	expiry       time.Duration
	offerID      string
	btcRPCURL    string
	btcWallet    string // bitcoind wallet that FUNDS the locked BTC HTLC
	btcChainName string
	// Mixed same-chain (rails 7/8): when quoteAsset is a real Sequentia asset id,
	// the maker LOCKS that asset in an on-chain HTLC on the Sequentia chain
	// (funded/refunded via seqRPCURL/seqWallet, Elements format; heights and
	// btcDelta are SEQUENTIA heights) and bitcoind is not used at all.
	quoteAsset  string
	seqRPCURL   string
	seqWallet   string
	assetLnSock string // the maker's asset LN node (holdinvoice plugin; receives the asset)
	btcDelta    uint32 // T_btc = parent tip + this (the maker's on-chain refund CLTV)
	minBTCConf  int    // confs the maker waits on its own BTC funding before advertising
	spendFee    uint64
	holdTimeout time.Duration
	requote     bool
	stateDir    string // per-lift session persistence (the funded on-chain leg + refund material)
}

// buildSubAssetSellOffer builds a Lightning offer (base=asset, quote=BTC sentinel) with
// ln_direction=LnBTCForAssetLN, TradeDir=BUY: the maker BUYS the asset (locks BTC
// on-chain, receives the asset over LN).
func buildSubAssetSellOffer(cfg subAssetSellMakerConfig, assetLnNodeID string) *seqobv1.Offer {
	// Mixed same-chain (rails 7/8): advertise the TRUE quote asset; ln_direction
	// stays LnBTCForAssetLN — the quote asset stands in BTC's structural place
	// (maker locks the quote on-chain, receives the base over Lightning).
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
		MakerLnNodePubkey: assetLnNodeID,
		TradeDir:          seqobv1.TradeDir_TRADE_DIR_BUY, // the maker acquires the asset
		Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
			MakerClaimPub:          cfg.makerPubKey,
			MakerRefundPub:         cfg.makerPubKey,
			OnchainCltv:            cfg.btcDelta,
			MakerIssuesHoldInvoice: true, // the MAKER issues the asset hold invoice
			LnDirection:            offer.LnBTCForAssetLN,
		}},
	}
	o.OfferAsset, o.OfferAmount = quote, cfg.btcSats    // maker gives the quote (BTC or the mixed quote asset)
	o.WantAsset, o.WantAmount = cfg.asset, cfg.assetAmt // maker wants the base asset
	return o
}

func runSubAssetSellMaker(cfg subAssetSellMakerConfig) {
	if cfg.assetLnSock == "" {
		fatal("-mode subasset-sell requires -asset-ln-socket (the maker's asset LN node with the holdinvoice plugin)")
	}
	// Chain seams: legacy = bitcoind (real BTC); mixed same-chain (-quote-asset)
	// = the Sequentia chain, HTLC on the quote asset, Elements format.
	var (
		tip    func() (int64, error)
		newOps func(preimage []byte) client.SubAssetSellMakerOps
	)
	if cfg.quoteAsset != "" {
		if cfg.seqRPCURL == "" {
			fatal("-mode subasset-sell with -quote-asset requires -xseq-rpc (the Sequentia node that LOCKS + refunds the on-chain quote-asset HTLC)")
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
		newOps = func(preimage []byte) client.SubAssetSellMakerOps {
			return client.NewLiveSubAssetSellMakerOpsSeq(seqChain, cfg.quoteAsset,
				xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset), preimage)
		}
	} else {
		if cfg.btcRPCURL == "" {
			fatal("-mode subasset-sell requires -btc-rpc (the bitcoind that LOCKS + refunds the BTC HTLC)")
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
		newOps = func(preimage []byte) client.SubAssetSellMakerOps {
			return &client.LiveSubAssetSellMakerOps{
				Swap:    xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLock(preimage)),
				AssetLN: xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset),
				BTC:     btcChain,
			}
		}
	}
	assetID, err := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset).NodeID()
	if err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", cfg.assetLnSock, err)
	}

	o := buildSubAssetSellOffer(cfg, assetID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}
	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	fmt.Printf("seqob-maker up (SUB-ASSET SELL): posted BUY offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
	fmt.Printf("  maker BUYS %d %s over Lightning for %d BTC sats on-chain (taker pays the asset over LN, receives BTC on-chain)  T_btc=+%d  asset-node=%s btc-chain=%s\n",
		cfg.assetAmt, cfg.asset, cfg.btcSats, cfg.btcDelta, assetID, cfg.btcChainName)
	fmt.Printf("  taker lifts with: seqob-cli xsubas-sell -asset %s -offer-id %s -maker-pubkey %s\n", cfg.asset, o.GetOfferId(), cfg.makerPubHex)

	serveSubAssetSell(ws, wsURL, o, cfg, tip, newOps, assetID)
}

// rerestSubAssetSellRemainder prices the unfilled remainder of a partially-filled
// sub-asset SELL offer for its T8 re-rest. The asset side (base = want) is the
// exact remainder; the BTC side (the maker's OFFER — it GIVES the BTC) is re-priced
// at the offer's OWN rate with ProportionalBtc (ceil) rather than by subtracting
// the rounded filled sats — that subtraction drifts, and ceil keeps the BTC side
// >= 1 sat for any remainder > 0, so a live remainder never re-rests with a zero
// (unpriceable) BTC side. The <= 1-sat ceil surplus is the same direction both
// sides already agreed on for the slice itself (the taker-receives-BTC ceil).
func rerestSubAssetSellRemainder(offerBtc, baseAsset, filledAsset uint64) (remainAsset, remainBtc uint64) {
	remainAsset = baseAsset - filledAsset
	remainBtc = client.ProportionalBtc(offerBtc, remainAsset, baseAsset)
	return remainAsset, remainBtc
}

func serveSubAssetSell(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg subAssetSellMakerConfig,
	tip func() (int64, error), newOps func(preimage []byte) client.SubAssetSellMakerOps, assetLNID string) {
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
			fmt.Printf("ws read error: %v; reconnecting\n", err)
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
				refuse(sid, cr, "offer_filled", "offer already filled")
				continue
			}
			if busy {
				refuse(sid, cr, "busy", "another swap is in flight")
				continue
			}
			if refuseIfDraining(sid, cr, refuse) {
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				mu.Unlock()
				continue
			}
			fmt.Printf("sub-asset SELL swap requested: session %s offer %s\n", sid, lr.GetOfferId())
			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }

			go func(sid string, in chan []byte) {
				settled := false
				var filledAsset, filledBtc uint64
				defer func() {
					// Record the executed fill FIRST, before the partial-remainder re-rest or the
					// requote/cancel mutate/remove the still-resting offer: the relay records the
					// trade (last_price + chart for the asset/BTC pair) at the original price and
					// decrements by exactly filledAsset (the base atoms actually taken; a lift may
					// be partial). The maker received the asset over LN and the taker claims the
					// BTC on-chain, so there is no maker-held settling txid.
					if settled {
						reportSettledTrade(ws, o, cfg.makerKey, filledAsset, "")
					}
					reconnect := func() {
						if n, err := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset).ReconnectPeers(); err != nil {
							fmt.Printf("requote: asset-LN peer reconnect err: %v\n", err)
						} else if n > 0 {
							fmt.Printf("requote: reconnected %d asset-LN peer(s)\n", n)
						}
					}
					// T8 partial fill: the taker took only part of the offer, so reduce it to the
					// remainder and re-quote (there is more to buy, regardless of -requote);
					// requoteAfterFill re-signs + re-posts the shrunk offer. The BTC side is
					// re-priced at the offer's own rate (ceil, never zero) — see
					// rerestSubAssetSellRemainder. A whole fill keeps the old behavior.
					partial := settled && filledAsset > 0 && filledAsset < o.GetBaseAmount()
					if partial {
						remainAsset, remainBtc := rerestSubAssetSellRemainder(o.GetOfferAmount(), o.GetBaseAmount(), filledAsset)
						mu.Lock()
						o.OfferAmount = remainBtc // the BTC the maker still gives (offer side)
						o.BaseAmount = remainAsset
						o.WantAmount = remainAsset // the asset the maker still wants (want side)
						mu.Unlock()
						fmt.Printf("session %s: PARTIAL fill (%d asset over LN, %d sats on-chain); re-resting the remainder %d %s for %d sats\n",
							sid, filledAsset, filledBtc, remainAsset, o.GetWantAsset(), remainBtc)
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

				h, err := tip()
				if err != nil {
					fmt.Printf("session %s: on-chain-leg tip: %v\n", sid, err)
					return
				}
				tBtc := uint32(h) + cfg.btcDelta
				// The on-chain leg this maker funds must stay reclaimable after T_btc even if
				// this process is gone by then: the refund key is derived from the identity
				// key rather than minted per swap, and the funded leg is written down.
				refundKey := derivedRefundKey(cfg.makerKey, "subasset-sell")
				p := client.MakerSubAssetSellParams{
					NewMakerOps:    newOps,
					Crypter:        cr,
					BtcAmount:      o.GetOfferAmount(), // BTC sats a WHOLE fill locks (a partial locks the priced slice)
					AssetAmount:    o.GetWantAmount(),  // asset atoms a WHOLE fill receives over LN (the taker may take a slice)
					BtcLocktime:    tBtc,
					MinBTCConf:     cfg.minBTCConf,
					SpendFeeSats:   cfg.spendFee,
					HoldTimeout:    cfg.holdTimeout,
					MakerRefundKey: refundKey,
					Log:            logf,
				}
				res, err := client.RunMakerSubAssetSell(p, in, send)
				if res != nil && res.BtcLeg != nil {
					persistXSessionSubAssetSell(cfg.stateDir, sid, o.GetOfferId(), res, refundKey, cfg.quoteAsset)
				}
				if err != nil {
					fmt.Printf("session %s: sub-asset SELL swap ended: %v\n", sid, err)
					if res != nil && res.BtcLeg != nil && !res.Settled {
						fmt.Printf("session %s: (maker on-chain leg %s reclaimable at T_btc=%d if the taker never paid; refund key derived from -maker-priv, leg persisted under %s)\n", sid, res.BtcLeg.Funded.TxID, res.BtcLocktime, cfg.stateDir)
					}
					return
				}
				settled = res.Settled
				filledAsset, filledBtc = res.FilledAsset, res.FilledBtc
				fmt.Printf("session %s: SUB-ASSET SELL SWAP SETTLED: took %d asset over LN; the taker claims %d sats BTC on-chain\n",
					sid, res.FilledAsset, res.FilledBtc)
			}(sid, in)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				continue
			}
			select {
			case in <- sm.GetCiphertext():
			default:
			}

		case from.GetOrderStatus() != nil:
			st := from.GetOrderStatus()
			fmt.Printf("order %s status=%s\n", st.GetOfferId(), st.GetStatus())
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
