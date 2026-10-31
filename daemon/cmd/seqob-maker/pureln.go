package main

// pureln.go adds the PURE-LN (both legs Lightning) maker mode to seqob-maker. It
// is the "assets at the edges" sibling of the submarine mode: instead of an
// on-chain asset HTLC gated by Bitcoin-anchor depth, BOTH legs are off-chain
// Lightning — a Sequentia issued-asset leg and a "BTC" leg — stitched by one
// shared secret at the maker's translating node. There is no on-chain leg and no
// anchor gate, so the happy path settles in seconds.
//
// The maker runs TWO Lightning nodes: a SeqLN-on-Sequentia node with asset
// channels (-asset-ln-socket) and a SeqLN-on-Bitcoin node with BTC channels
// (-ln-socket). It holds its INCOMING leg and pays its OUTGOING leg (learning P),
// selected by -side (maker perspective):
//   - -side sell (maker GIVES the asset): holds BTC, pays the asset
//     (ln_direction LnBTCLNForAssetLN; the taker buys the asset).
//   - -side buy  (maker ACQUIRES the asset): holds the asset, pays BTC
//     (ln_direction LnAssetLNForBTCLN; the taker sells the asset).
// It needs no Sequentia node RPC and no bitcoind — only the two lightning-rpc
// sockets.

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

type pureLNMakerConfig struct {
	relay       string
	makerKey    *btcec.PrivateKey
	makerPubHex string
	makerPubKey []byte // 33-byte compressed (advisory LightningTerms keys)
	asset       string // the SEQ asset id (hex); base of the pair, and the asset LN leg's id
	assetAmt    uint64 // asset atoms exchanged
	btcSats     uint64 // "BTC" sats exchanged
	feeAsset    string
	expiry      time.Duration
	minAnchor   uint32 // offer.min_anchor_depth (display only; pure-LN has no anchor gate)
	offerID     string
	assetLnSock string // the maker's SeqLN-on-Sequentia lightning-rpc (asset leg)
	btcLnSock   string // the maker's SeqLN-on-Bitcoin lightning-rpc (BTC leg)
	btcAsset    string // counter-leg SETTLEMENT asset id (hex); empty = the policy asset / a real BTC-LN node. Set to route the counter leg over a 2nd issued asset (asset<->asset pure-LN).
	quoteAsset  string // pure-LN QUOTE asset id (hex) the offer ADVERTISES; empty = the BTC sentinel (asset<->BTC). Set to a real asset id for a truthful asset<->asset market. Keep the NUMERAIRE as the quote so the market key matches the wallet's canonicalPair (e.g. base=OILX quote=EURX, i.e. OILX priced in EURX) — a reversed pair rests in a market the wallet never queries.
	holdTimeout time.Duration
	reverse     bool // true = SELL the asset (maker gives asset; holds BTC); false = BUY (maker acquires asset; holds asset)
	onchainCltv uint32
	requote     bool // true = re-post a fresh offer after each settled fill instead of exiting
}

// plnDirection maps the maker config to the driver's direction and the offer's
// ln_direction. The driver's PlnDirection is stated from the maker's held leg:
//   - maker SELLS (gives the asset): holds BTC, pays the asset -> PlnBuy.
//   - maker BUYS  (acquires the asset): holds the asset, pays BTC -> PlnSell.
func (cfg pureLNMakerConfig) plnDirection() (client.PlnDirection, uint32) {
	if cfg.reverse {
		return client.PlnBuy, offer.LnBTCLNForAssetLN // maker gives the asset
	}
	return client.PlnSell, offer.LnAssetLNForBTCLN // maker acquires the asset
}

// buildPureLNOffer builds a Lightning offer (base=asset, quote=the BTC sentinel)
// with a pure-LN ln_direction. maker_ln_node_pubkey advertises the maker's HELD
// leg (the taker pays its hold there); the load-bearing value is re-sent per-lift
// in the E2E Terms message.
// plnMsatToBase converts an asset-side msat amount (how the pure-LN driver measures
// the asset leg) back into the offer's base atoms. The offer's own asset-msat total is
// the conversion basis, so this stays exact for a whole fill and rounds DOWN for a
// slice — never claiming more was taken than actually was.
func plnMsatToBase(o *seqobv1.Offer, seqMsat uint64) uint64 {
	if seqMsat == 0 {
		return 0
	}
	wholeMsat := o.GetBaseAmount() * 1000
	if wholeMsat == 0 || seqMsat >= wholeMsat {
		return o.GetBaseAmount()
	}
	// floor(baseAmount * seqMsat / wholeMsat) in 128 bits — a bare uint64 multiply
	// overflows at these magnitudes. ProportionalBtcFloor is that exact operation.
	return client.ProportionalBtcFloor(o.GetBaseAmount(), seqMsat, wholeMsat)
}

// plnWholeMsat is the offer's signed whole in msat, its counter (BTC or quote asset) leg and its
// asset leg: what a taker prices a slice from (the crosser's plnWhole).
func plnWholeMsat(o *seqobv1.Offer) (btcMsat, seqMsat uint64) {
	if o.GetLightning().GetLnDirection() == offer.LnBTCLNForAssetLN { // the maker gives the asset
		return o.GetWantAmount() * 1000, o.GetOfferAmount() * 1000
	}
	return o.GetOfferAmount() * 1000, o.GetWantAmount() * 1000
}

// plnRestRemainder re-rests what is left of o after `taken` base atoms filled, at the offer's own
// rate: the counter leg the maker receives rounded up, the one it gives rounded down. -> the
// remainder, 0 when nothing is left to rest.
func plnRestRemainder(o *seqobv1.Offer, taken uint64) uint64 {
	if taken == 0 || taken >= o.GetBaseAmount() {
		return 0
	}
	remain := o.GetBaseAmount() - taken
	if o.GetLightning().GetLnDirection() == offer.LnBTCLNForAssetLN {
		o.WantAmount = client.ProportionalBtc(o.GetWantAmount(), remain, o.GetBaseAmount())
		o.OfferAmount = remain
	} else {
		o.OfferAmount = client.ProportionalBtcFloor(o.GetOfferAmount(), remain, o.GetBaseAmount())
		o.WantAmount = remain
	}
	o.BaseAmount = remain
	return remain
}

func buildPureLNOffer(cfg pureLNMakerConfig, holdLnNodeID string) *seqobv1.Offer {
	_, lnDir := cfg.plnDirection()
	// The QUOTE (counter) leg: a real Sequentia asset id for an asset<->asset pure-LN market
	// (e.g. base=OILX quote=EURX — numeraire as quote, matching the wallet's canonicalPair), or the
	// BTC sentinel for the classic asset<->BTC pure-LN. The SETTLEMENT
	// leg is routed over cfg.btcAsset (which main.go defaults to this same id for asset<->asset), so
	// the offer now advertises its true pair instead of masquerading every counter-leg as "BTC".
	quoteAsset := orDefault(cfg.quoteAsset, offer.BTCSentinel)
	o := &seqobv1.Offer{
		OfferId:           orDefault(cfg.offerID, randstr.Hex(16)),
		SchemaVersion:     1,
		Pair:              &seqobv1.AssetPair{BaseAsset: cfg.asset, QuoteAsset: quoteAsset},
		BaseAmount:        cfg.assetAmt,
		AllowPartial:      true, // a taker may take a slice; the remainder is re-rested after the fill
		CreatedAtUnix:     uint64(time.Now().Unix()),
		ExpiresAtUnix:     uint64(time.Now().Add(cfg.expiry).Unix()),
		FeeAssetHint:      cfg.feeAsset,
		MinAnchorDepth:    cfg.minAnchor,
		MakerLnNodePubkey: holdLnNodeID,
		Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
			MakerClaimPub:          cfg.makerPubKey,
			MakerRefundPub:         cfg.makerPubKey,
			OnchainCltv:            cfg.onchainCltv,
			MakerIssuesHoldInvoice: false, // the taker pays a bare hash; no maker-issued invoice
			LnDirection:            lnDir,
		}},
	}
	if cfg.reverse {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL // the maker gives up the asset
		o.OfferAsset, o.OfferAmount = cfg.asset, cfg.assetAmt
		o.WantAsset, o.WantAmount = quoteAsset, cfg.btcSats
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY // the maker acquires the asset
		o.OfferAsset, o.OfferAmount = quoteAsset, cfg.btcSats
		o.WantAsset, o.WantAmount = cfg.asset, cfg.assetAmt
	}
	return o
}

// reconnectPlnPeers reconnects any dropped channel peers on BOTH of the maker's
// Lightning nodes (the SeqLN-on-Sequentia asset node and the SeqLN-on-Bitcoin BTC
// node) before a re-quote. A pure-LN swap holds one leg and pays the other, so a
// silently dropped peer on either node would leave the next swap unroutable. It is
// best-effort: failures are logged, not fatal (a peer with no known address stays
// down until it dials in, and the swap-time hold/pay would fail cleanly anyway).
func reconnectPlnPeers(cfg pureLNMakerConfig) {
	assetLeg := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset)
	if n, err := assetLeg.ReconnectPeers(); err != nil {
		fmt.Printf("requote: asset-LN peer reconnect: reconnected %d, err: %v\n", n, err)
	} else if n > 0 {
		fmt.Printf("requote: reconnected %d asset-LN peer(s)\n", n)
	}
	btcLeg := xchain.NewCLNLNLeg(cfg.btcLnSock)
	if n, err := btcLeg.ReconnectPeers(); err != nil {
		fmt.Printf("requote: BTC-LN peer reconnect: reconnected %d, err: %v\n", n, err)
	} else if n > 0 {
		fmt.Printf("requote: reconnected %d BTC-LN peer(s)\n", n)
	}
}

// runPureLNMaker posts a pure-LN offer and serves swaps. No Ocean wallet, no
// bitcoind, no Sequentia node RPC: both legs are the maker's two LN nodes.
func runPureLNMaker(cfg pureLNMakerConfig) {
	if cfg.assetLnSock == "" || cfg.btcLnSock == "" {
		fatal("-mode pureln requires -asset-ln-socket (SeqLN-on-Sequentia) and -ln-socket (SeqLN-on-Bitcoin)")
	}
	// Validate both LN nodes up front and learn their ids.
	assetID, err := xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset).NodeID()
	if err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", cfg.assetLnSock, err)
	}
	btcID, err := xchain.NewCLNLNLeg(cfg.btcLnSock).NodeID()
	if err != nil {
		fatal("btc lightning-rpc %s unreachable: %v", cfg.btcLnSock, err)
	}
	// Advertise the HELD leg's node id (the taker pays the hold there).
	holdID := btcID
	if !cfg.reverse {
		holdID = assetID // maker BUYS -> holds the asset leg
	}

	o := buildPureLNOffer(cfg, holdID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	if cfg.reverse {
		fmt.Printf("seqob-maker up (PURE-LN): posted SELL offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker sells %d %s for %d BTC sats, both over Lightning (taker buys the asset)  hold-leg(BTC)=%s asset-node=%s\n",
			cfg.assetAmt, cfg.asset, cfg.btcSats, btcID, assetID)
		fmt.Printf("  taker swaps with: seqob-cli xpln -side buy -asset %s -offer-id %s -maker-pubkey %s\n", cfg.asset, o.GetOfferId(), cfg.makerPubHex)
	} else {
		fmt.Printf("seqob-maker up (PURE-LN): posted BUY offer %s by maker %s\n", o.GetOfferId(), cfg.makerPubHex)
		fmt.Printf("  maker buys %d %s for %d BTC sats, both over Lightning (taker sells the asset)  hold-leg(asset)=%s btc-node=%s\n",
			cfg.assetAmt, cfg.asset, cfg.btcSats, assetID, btcID)
		fmt.Printf("  taker swaps with: seqob-cli xpln -side sell -asset %s -offer-id %s -maker-pubkey %s\n", cfg.asset, o.GetOfferId(), cfg.makerPubHex)
	}

	servePureLN(ws, wsURL, o, cfg)
}

// servePureLN is the pure-LN event loop: each swap gets its own goroutine running
// RunMakerPureLN; the loop routes sealed courier frames to the session inbox. Same
// whole-swap discipline as serveSubmarine — ONE swap in flight, and the offer is
// cancelled after its first settlement (restart to re-quote).
func servePureLN(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg pureLNMakerConfig) {
	var mu sync.Mutex
	inboxes := make(map[string]chan []byte)
	inFlight := 0
	filled := false
	idle := func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 }
	armRequoteExit(o, idle) // retire for a fresh re-quote before the offer expires
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })

	dir, _ := cfg.plnDirection()
	newOps := func() client.PlnMakerOps {
		var btcLeg xchain.LNLeg = xchain.NewCLNLNLeg(cfg.btcLnSock)
		if cfg.btcAsset != "" {
			btcLeg = xchain.NewCLNAssetLNLeg(cfg.btcLnSock, cfg.btcAsset)
		}
		swap := xchain.NewPureLNSwap(
			xchain.NewCLNAssetLNLeg(cfg.assetLnSock, cfg.asset),
			btcLeg,
		)
		if dir == client.PlnSell {
			return &client.LivePlnMakerSellOps{Swap: swap} // maker holds the asset, pays BTC
		}
		return &client.LivePlnMakerBuyOps{Swap: swap} // maker holds BTC, pays the asset
	}

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
				fmt.Printf("swap %s: crypter error: %v\n", sid, err)
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
			fmt.Printf("pure-LN swap requested: session %s offer %s\n", sid, lr.GetOfferId())

			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }

			go func(sid string, in chan []byte) {
				settled := false
				// A lift may take a SLICE. Both legs are Lightning, so the driver works in
				// msat; filledSeqMsat is what this lift settled and partial says the offer
				// still has size left to re-rest.
				var filledSeqMsat uint64
				partial := false
				defer func() {
					// Record the executed fill FIRST, before requote/cancel touch the still-resting
					// offer: the relay records the trade (so the asset/BTC pure-LN pair gets a
					// last_price + chart) and decrements/finalizes the order. Both legs are Lightning,
					// so there is no settling txid; min_anchor_depth is 0, so the fill finalizes at once.
					if settled {
						fill := plnMsatToBase(o, filledSeqMsat)
						if fill == 0 {
							fill = o.GetBaseAmount()
						}
						reportSettledTrade(ws, o, cfg.makerKey, fill, "")
					}
					// -requote: re-post a fresh quote while still holding the in-flight slot
					// (racing lifts are refused as "busy" until it is live -> no double-post).
					// Both legs are Lightning, so reconnect BOTH LN nodes' dropped channel
					// peers before re-quoting or the next swap's hold/pay cannot route.
					// PARTIAL FILL: re-rest the REMAINDER at the offer's own rate, rather than
					// re-posting the full size the maker no longer has.
					if settled && partial {
						mu.Lock()
						taken := plnMsatToBase(o, filledSeqMsat)
						if remain := plnRestRemainder(o, taken); remain > 0 {
							fmt.Printf("session %s: PARTIAL fill (%d of %d atoms); re-resting the remainder %d\n",
								sid, taken, taken+remain, remain)
						}
						mu.Unlock()
					}
					if settled && (cfg.requote || partial) {
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, func() {
							reconnectPlnPeers(cfg)
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
					}
					requoteExitIfPending(idle)
				}()
				// A slice is priced off the offer as the relay holds it signed, the same whole the
				// taker reads: after a partial fill that is the re-rested remainder, whose amounts
				// were rounded, not the size the maker was started with.
				mu.Lock()
				btcMsat, seqMsat := plnWholeMsat(o)
				mu.Unlock()
				p := client.MakerPlnParams{
					Direction:   dir,
					NewMakerOps: newOps,
					Crypter:     cr,
					BtcAmtMsat:  btcMsat,
					SeqAmtMsat:  seqMsat,
					HoldTimeout: cfg.holdTimeout,
					Log:         logf,
				}
				res, err := client.RunMakerPureLN(p, in, send)
				if err != nil {
					fmt.Printf("session %s: pure-LN swap ended: %v\n", sid, err)
					return
				}
				settled = res.Settled
				filledSeqMsat = res.FilledSeqMsat
				partial = res.RemainderSeqMsat > 0
				fmt.Printf("session %s: PURE-LN SWAP SETTLED: %d of %d msat asset-side, both legs off-chain, no anchor wait\n",
					sid, res.FilledSeqMsat, res.FilledSeqMsat+res.RemainderSeqMsat)
			}(sid, in)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				fmt.Printf("session %s: swap_msg without a live pure-LN session; ignoring\n", sm.GetSessionId())
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
