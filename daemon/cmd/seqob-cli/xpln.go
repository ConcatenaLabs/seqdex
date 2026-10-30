package main

// xpln.go: the PURE-LN swap taker — trade a Sequentia issued asset against "BTC"
// with BOTH legs over Lightning. There is no on-chain leg, so unlike xsublift
// there is no asset HTLC to fund and no refund state file: a stalled swap is
// unwound natively by the LN hold timeout. The taker runs TWO Lightning nodes —
// a SeqLN-on-Sequentia node with asset channels (-asset-ln-socket) and a
// SeqLN-on-Bitcoin node with BTC channels (-ln-socket) — mints an invoice on its
// INCOMING leg (a fresh preimage P), hands the maker H + that invoice, and pays
// the maker's hold on its OUTGOING leg by bare hash. It drives
// internal/seqob/client.RunTakerPureLN over the relay courier.
//
//   - -side buy : BUY the asset with BTC-LN (matches a maker offer that GIVES the
//     asset: ln_direction LnBTCLNForAssetLN). Taker receives the asset, pays BTC.
//   - -side sell: SELL the asset for BTC-LN (matches a maker offer that ACQUIRES
//     the asset: ln_direction LnAssetLNForBTCLN). Taker receives BTC, pays the asset.

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXPln(args []string) {
	fs := newFlagSet("xpln")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	sideFlag := fs.String("side", "", "buy = acquire the asset with BTC-LN; sell = give the asset for BTC-LN (required)")
	asset := fs.String("asset", "", "Sequentia asset id (hex); the pair is <asset>/BTC (required)")
	offerID := fs.String("offer-id", "", "offer id to take (empty: first verified matching pure-LN offer for -asset/-side)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	assetLnSocket := fs.String("asset-ln-socket", "", "the taker's SeqLN-on-Sequentia lightning-rpc unix socket (asset leg) (required)")
	lnSocket := fs.String("ln-socket", "", "the taker's SeqLN-on-Bitcoin lightning-rpc unix socket (BTC leg) (required)")
	btcAsset := fs.String("btc-asset", "", "counter-leg SETTLEMENT asset id (hex); empty = policy asset / real BTC-LN. Set to route the counter leg over a 2nd issued asset (asset<->asset). MUST match the maker")
	quoteAssetFlag := fs.String("quote-asset", "", "QUOTE asset id (hex) of the market; empty = the BTC sentinel (asset<->BTC). Set to a real asset id for an asset<->asset market. Numeraire on the QUOTE side to match the maker + the wallet's canonicalPair (e.g. -asset OILX -quote-asset EURX, i.e. OILX priced in EURX); defaults -btc-asset to it")
	takeSeq := fs.Uint64("take-asset-msat", 0, "PARTIAL FILL: asset-side slice to take, in msat. 0 = the whole offer (the classic lift). The BTC side is derived from the signed offer's ratio and required exactly, so the maker cannot re-price the slice")
	finalCltv := fs.Uint("final-cltv", 18, "final-hop cltv delta when paying the maker's hold")
	termsWait := fs.Duration("terms-wait", 2*time.Minute, "max wait for the maker's terms")
	holdWait := fs.Duration("hold-wait", 2*time.Minute, "max wait for the maker to register its hold after we send the invoice")
	_ = fs.Parse(args)

	side := strings.ToLower(*sideFlag)
	if side != "buy" && side != "sell" {
		fatal("xpln requires -side buy|sell")
	}
	if *asset == "" {
		fatal("xpln requires -asset (the hex asset id; the pair is <asset>/BTC)")
	}
	if *assetLnSocket == "" || *lnSocket == "" {
		fatal("xpln requires -asset-ln-socket (asset leg) and -ln-socket (BTC leg)")
	}

	// asset<->asset pure-LN: the market QUOTE is a real asset id and the settlement counter-leg routes
	// over it (defaulting -btc-asset). Empty -quote-asset keeps the classic asset<->BTC (quote = the BTC
	// sentinel, counter leg = real BTC-LN over -ln-socket).
	quoteAsset := *quoteAssetFlag
	if quoteAsset == "" {
		quoteAsset = offer.BTCSentinel
	}
	effBtcAsset := *btcAsset
	if effBtcAsset == "" && !offer.IsBTCSentinel(quoteAsset) {
		effBtcAsset = quoteAsset
	}

	// The taker's side selects which maker offer it matches: buying the asset
	// needs an offer where the maker GIVES it (LnBTCLNForAssetLN); selling needs
	// an offer where the maker ACQUIRES it (LnAssetLNForBTCLN).
	var wantLnDir uint32
	var dir client.PlnDirection
	if side == "buy" {
		wantLnDir, dir = offer.LnBTCLNForAssetLN, client.PlnBuy
	} else {
		wantLnDir, dir = offer.LnAssetLNForBTCLN, client.PlnSell
	}

	// 1. Find + verify a matching pure-LN offer.
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, quoteAsset), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if *offerID != "" && (o.GetOfferId() != *offerID || o.GetMakerPubkey() != *makerPub) {
			continue
		}
		lt := o.GetLightning()
		if lt == nil || lt.GetLnDirection() != wantLnDir {
			continue
		}
		if err := offer.VerifyOffer(o); err != nil {
			fmt.Printf("skipping unverified offer %s: %v\n", o.GetOfferId(), err)
			continue
		}
		target = o
		break
	}
	if target == nil {
		fatal("no verified pure-LN offer found to %s %s/BTC (use `seqob-cli book -base %s -quote BTC`)", side, *asset, *asset)
	}

	assetAtoms := target.GetBaseAmount()
	btcSats := target.GetWantAmount()
	if target.GetOfferAsset() == quoteAsset {
		btcSats = target.GetOfferAmount()
	}
	seqMsat, btcMsat := assetAtoms*1000, btcSats*1000
	if side == "buy" {
		fmt.Printf("taking pure-LN offer %s by %s: BUY %d %s for %d BTC sats, both over Lightning\n",
			target.GetOfferId(), short(target.GetMakerPubkey()), assetAtoms, *asset, btcSats)
	} else {
		fmt.Printf("taking pure-LN offer %s by %s: SELL %d %s for %d BTC sats, both over Lightning\n",
			target.GetOfferId(), short(target.GetMakerPubkey()), assetAtoms, *asset, btcSats)
	}

	// 2. Validate both LN nodes up front.
	if _, err := xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset).NodeID(); err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", *assetLnSocket, err)
	}
	if _, err := xchain.NewCLNLNLeg(*lnSocket).NodeID(); err != nil {
		fatal("btc lightning-rpc %s unreachable: %v", *lnSocket, err)
	}

	// 3. Open the swap session; E2E key from the SIGNED offer.
	takerKey := loadOrGenKey(*priv)
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()
	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            target.GetOfferId(),
		MakerPubkey:        target.GetMakerPubkey(),
		TakeAmount:         target.GetBaseAmount(), // whole-swap
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}})
	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sid := la.GetLiftAccepted().GetSessionId()
	fmt.Printf("pure-LN swap session %s opened\n", sid)

	makerOfferPub, err := hex.DecodeString(target.GetMakerPubkey())
	if err != nil {
		fatal("decode maker pubkey: %v", err)
	}
	pk, err := btcec.ParsePubKey(makerOfferPub)
	if err != nil {
		fatal("parse maker pubkey: %v", err)
	}
	crypter, err := client.NewCrypter(takerKey, pk)
	if err != nil {
		fatal("crypter: %v", err)
	}

	// 4. Run the taker pure-LN driver over the WS courier.
	send := func(sealed []byte) error {
		b, err := jsonMarshal.Marshal(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	// One background reader keeps gorilla auto-ponging the relay's keepalive through any
	// on-chain confirmation wait, so the session survives to the announce (see startCourierReader).
	recv := startCourierReader(conn)

	var btcLeg xchain.LNLeg = xchain.NewCLNLNLeg(*lnSocket)
	if effBtcAsset != "" {
		// an asset counter leg settles over that asset's channels, on the asset node: the
		// bitcoin node knows no asset (it refuses an asset invoice, and the maker's asset node
		// is not in its graph)
		btcLeg = xchain.NewCLNAssetLNLeg(*assetLnSocket, effBtcAsset)
	}
	swap := xchain.NewPureLNSwap(
		xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset),
		btcLeg,
	)
	var ops client.PlnTakerOps
	if side == "buy" {
		ops = &client.LivePlnTakerBuyOps{Swap: swap} // receive the asset, pay BTC
	} else {
		ops = &client.LivePlnTakerSellOps{Swap: swap} // receive BTC, pay the asset
	}

	res, err := client.RunTakerPureLN(client.TakerPlnParams{
		Direction:   dir,
		Ops:         ops,
		Crypter:     crypter,
		BtcAmtMsat:  btcMsat,
		SeqAmtMsat:  seqMsat,
		TakeSeqMsat: *takeSeq,
		FinalCltv:   uint32(*finalCltv),
		Timing:      client.XcTiming{TermsWait: *termsWait, SeqLockWait: *holdWait},
		Log:         func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	}, send, recv)
	if err != nil {
		fmt.Printf("pure-LN swap ended: %v\n", err)
		return
	}
	// Report what actually SETTLED, not the offer's headline size: with a partial
	// fill those differ, and printing the offer would overstate the trade.
	filledAtoms, filledSats := res.FilledSeqMsat/1000, res.FilledBtcMsat/1000
	verb := "sold"
	if side == "buy" {
		verb = "bought"
	}
	fmt.Printf("PURE-LN SWAP SETTLED: %s %d %s for %d BTC sats over Lightning; preimage %s\n",
		verb, filledAtoms, *asset, filledSats, hex.EncodeToString(res.Preimage))
	if filledAtoms < assetAtoms {
		fmt.Printf("  PARTIAL fill: %d of the offer's %d atoms; %d remain resting\n",
			filledAtoms, assetAtoms, assetAtoms-filledAtoms)
	}
}
