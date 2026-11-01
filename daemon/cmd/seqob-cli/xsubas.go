package main

// xsubas.go: the SUB-ASSET swap taker — the MIRROR of xsubbuy. The taker pays BTC
// ON-CHAIN and receives a Sequentia asset OVER LIGHTNING. It matches a maker offer
// where the maker SELLS the asset with ln_direction LnAssetLNForBTC: the taker
// funds a BTC HTLC on-chain (claim=maker with P, refund=taker after T_btc), issues
// an asset hold invoice on H on its own SeqLN-on-Sequentia node, waits for the
// maker's held asset payment, and SETTLES it with P (receiving the asset). It
// drives internal/seqob/client.RunTakerSubAsset over the relay courier.
//
// The taker holds the secret P (it is the LN receiver). Refund material (secret,
// refund key, the funded BTC leg, T_btc) is persisted to -state-file BEFORE the
// asset is received, so `seqob-cli xsubas-refund -state-file ...` (or the built-in
// -refund-wait) recovers the BTC leg after T_btc if the maker never pays.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xsubasState is the sub-asset taker's refund/recovery record. It is the
// BTC-on-chain mirror of xliftState: the taker holds the secret, funds a BTC HTLC,
// and needs (secret, refund key, funded leg, T_btc) to reclaim the BTC if the
// maker never pays the asset.
type xsubasState struct {
	CreatedAt        string `json:"created_at"`
	Relay            string `json:"relay"`
	OfferID          string `json:"offer_id"`
	MakerPubkey      string `json:"maker_pubkey"`
	Asset            string `json:"asset"`
	QuoteAsset       string `json:"quote_asset,omitempty"` // mixed same-chain: the on-chain leg's REAL asset (empty = BTC)
	AssetAmount      uint64 `json:"asset_amount"`
	BtcAmount        uint64 `json:"btc_amount"`
	SecretHex        string `json:"secret_hex"`
	HashHex          string `json:"hash_hex"`
	BtcRefundPrivHex string `json:"btc_refund_priv_hex"`
	SessionID        string `json:"session_id,omitempty"`
	BtcLegTxid       string `json:"btc_leg_txid,omitempty"`
	BtcLegVout       uint32 `json:"btc_leg_vout"`
	BtcLegAmount     uint64 `json:"btc_leg_amount,omitempty"`
	BtcLegScriptHex  string `json:"btc_leg_script_hex,omitempty"`
	BtcLocktime      uint32 `json:"btc_locktime,omitempty"`
	SeqPreimageHex   string `json:"seq_preimage_hex,omitempty"`
	Status           string `json:"status"`
}

func (st *xsubasState) save(path string) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fatal("marshal state: %v", err)
	}
	if err := ioutil.WriteFile(path, b, 0o600); err != nil {
		fatal("write state file %s: %v", path, err)
	}
}

func loadXSubAsState(raw []byte) (*xsubasState, error) {
	var st xsubasState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func cmdXSubAs(args []string) {
	fs := newFlagSet("xsubas")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex); the pair is <asset>/BTC (required)")
	offerID := fs.String("offer-id", "", "offer id to take (empty: first verified matching sub-asset offer for -asset)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required; funds + refunds the BTC HTLC)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet funding the BTC HTLC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	assetLnSocket := fs.String("asset-ln-socket", "", "the taker's SeqLN-on-Sequentia lightning-rpc unix socket (asset leg) (required)")
	quoteAsset := fs.String("quote-asset", "", "MIXED SAME-CHAIN (rails 7/8): the QUOTE asset id (hex). The on-chain leg becomes an HTLC on THIS Sequentia asset (funded/refunded via -xseq-rpc) instead of BTC; the pair is <asset>/<quote-asset>.")
	xseqRPCURL := envString(fs, "xseq-rpc", "SEQOB_XSEQ_RPC", "mixed same-chain: Sequentia node RPC URL http://user:pass@host:port (funds + refunds the on-chain quote-asset HTLC)")
	xseqWallet := fs.String("xseq-wallet", "", "mixed same-chain: Sequentia node wallet funding the quote-asset HTLC")
	minBTCConf := fs.Int("min-btc-conf", 1, "confirmations to wait on our own BTC leg before announcing")
	takeAmount := fs.Uint64("amount", 0, "T8 PARTIAL FILL: asset atoms to take (<= the offer). 0 = the whole offer. The BTC you lock is the proportional price, rounded up; the maker re-rests the remainder.")
	invoiceMode := fs.String("asset-invoice", "plain", "asset LN invoice mode: plain (bare BOLT11 whose payment_hash=H; no plugin) | hold (holdinvoice-plugin invoice; explicit settle). Both are equally atomic: the on-chain BTC HTLC is the hold.")
	extBolt11 := fs.String("asset-bolt11", "", "DEVICE-PREIMAGE mode: an asset invoice created OUT-OF-BAND by the device (the wallet) on its own hosted node with the device's OWN preimage. When set (with -payment-hash), this taker NEVER mints/settles the preimage: it funds the BTC HTLC on H, forwards this bolt11 to the maker, and waits for it to be PAID (the device settles). Non-custodial receive.")
	extHashHex := fs.String("payment-hash", "", "DEVICE-PREIMAGE / DEVICE-HODL mode: the payment_hash H (hex). H = SHA256(device preimage).")
	takerLnNodeID := fs.String("taker-ln-node-id", "", "DEVICE-HODL BUY: the taker's HOSTED asset node id. The device registered a HOLD on -payment-hash at that node and holds P; relays H + this node id (no bolt11), the maker pays the bare hash, and the DEVICE settles out-of-band (POST /node/settle). Requires -payment-hash and (normally) the external-BTC flags.")
	// EXTERNAL BTC HTLC: the wallet already funded the HTLC from the USER's own BTC
	// (xsubas-fund-btc / btc.js). This taker relays it; the LSP funds NOTHING. -btc-rpc
	// is still read (for the tip sanity check) but no -btc-wallet is used.
	extBtcTxid := fs.String("btc-htlc-txid", "", "EXTERNAL BTC: the txid of the user-funded HTLC (from xsubas-fund-btc). Enables relay-only mode.")
	extBtcVout := fs.Uint("btc-htlc-vout", 0, "EXTERNAL BTC: the HTLC output vout")
	extBtcAmount := fs.Uint64("btc-htlc-amount", 0, "EXTERNAL BTC: the HTLC output amount (sats)")
	extBtcScript := fs.String("btc-htlc-script", "", "EXTERNAL BTC: the HTLC redeem script (hex)")
	extBtcLocktime := fs.Uint("btc-locktime", 0, "EXTERNAL BTC: T_btc encoded in the HTLC (the user refund CLTV)")
	extBtcRefundPub := fs.String("btc-refund-pub", "", "EXTERNAL BTC: the user's device refund pubkey (hex) embedded in the HTLC")
	spendFee := fs.Uint64("spend-fee", 1000, "BTC HTLC refund fee target (sats)")
	stateFile := fs.String("state-file", "xsubas-session.json", "session persistence (refund needs this)")
	termsWait := fs.Duration("terms-wait", 2*time.Minute, "max wait for the maker's terms")
	settleWait := fs.Duration("settle-wait", 15*time.Minute, "max wait for the maker's held asset payment")
	refundWait := fs.Bool("refund-wait", false, "on abort after funding, poll until T_btc and refund the BTC HTLC automatically")
	_ = fs.Parse(args)

	if *asset == "" {
		fatal("xsubas requires -asset (the hex asset id; the pair is <asset>/BTC)")
	}
	if *btcRPCURL == "" && *quoteAsset == "" {
		fatal("xsubas requires -btc-rpc (the bitcoind that funds + refunds the BTC HTLC), or -quote-asset + -xseq-rpc for the mixed same-chain shape")
	}
	if *assetLnSocket == "" {
		fatal("xsubas requires -asset-ln-socket (the taker's SeqLN-on-Sequentia lightning-rpc, asset leg)")
	}
	// DEVICE-PREIMAGE (external-invoice) mode: the wallet made the asset invoice on
	// its own hosted node with its OWN preimage; we get only H + the bolt11, never P.
	var extHashH []byte
	if *extBolt11 != "" {
		var derr error
		extHashH, derr = hex.DecodeString(*extHashHex)
		if derr != nil || len(extHashH) != 32 {
			fatal("-asset-bolt11 requires -payment-hash <32-byte hex H>")
		}
	}
	// DEVICE-HODL BUY: the device registered a hold on H at its OWN hosted node and holds
	// P; we relay H + the node id (no bolt11) and wait for HELD. Requires -payment-hash.
	if *takerLnNodeID != "" {
		var derr error
		extHashH, derr = hex.DecodeString(*extHashHex)
		if derr != nil || len(extHashH) != 32 {
			fatal("-taker-ln-node-id requires -payment-hash <32-byte hex H>")
		}
	}
	// EXTERNAL BTC HTLC mode: the wallet funded the HTLC from the USER's own BTC; we
	// relay it. Build the leg from the flags + the refund pubkey.
	var extBtcLeg *xchain.LegLock
	var extBtcRefundPubB []byte
	if *extBtcTxid != "" {
		script, serr := hex.DecodeString(*extBtcScript)
		if serr != nil || len(script) == 0 {
			fatal("-btc-htlc-txid requires -btc-htlc-script <hex>")
		}
		if *extBtcAmount == 0 || *extBtcLocktime == 0 {
			fatal("-btc-htlc-txid requires -btc-htlc-amount and -btc-locktime")
		}
		extBtcRefundPubB, serr = hex.DecodeString(*extBtcRefundPub)
		if serr != nil || len(extBtcRefundPubB) == 0 {
			fatal("-btc-htlc-txid requires -btc-refund-pub <hex>")
		}
		extBtcLeg = &xchain.LegLock{
			Script:   script,
			Funded:   &xchain.FundedHTLC{TxID: *extBtcTxid, Vout: uint32(*extBtcVout), Amount: *extBtcAmount},
			Locktime: uint32(*extBtcLocktime),
		}
	}

	// 1. Find + verify a matching sub-asset offer (maker SELLS: LnAssetLNForBTC).
	// Mixed same-chain: the pair's quote is the REAL asset the on-chain leg pays.
	quote := offer.BTCSentinel
	if *quoteAsset != "" {
		quote = *quoteAsset
	}
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		// The maker filter applies WHENEVER a maker is named, not only alongside an offer id.
		//
		// It used to live inside the offer-id branch, so -maker-pubkey ALONE was silently ignored and
		// any maker's offer could be picked. That matters on a RETRY of an already-funded take: the
		// BTC HTLC has the maker's claim key baked into its redeem script, so only THAT maker can ever
		// claim it. Offers expire and re-post under new ids every few minutes, so a retry must be able
		// to say "the same maker, whatever its offer id is now" — and it had no way to.
		//
		// Seen live: a resumed GOLD buy re-matched to a different maker, which correctly refused with
		// 'redeemScript mismatch', the two scripts differing in exactly one field — the maker claim key.
		if *makerPub != "" && o.GetMakerPubkey() != *makerPub {
			continue
		}
		if *offerID != "" && o.GetOfferId() != *offerID {
			continue
		}
		lt := o.GetLightning()
		if lt == nil || lt.GetLnDirection() != offer.LnAssetLNForBTC {
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
		fatal("no verified sub-asset offer found to buy %s with on-chain %s (use `seqob-cli book -base %s -quote %s`)", *asset, quote, *asset, quote)
	}

	wholeAsset := target.GetOfferAmount() // the asset the maker gives over LN (the whole offer)
	assetAtoms := wholeAsset              // default: take the whole offer
	btcSats := target.GetWantAmount()     // the BTC sats the taker locks on-chain
	// T8 partial fill: take a slice and lock the PROPORTIONAL BTC (rounded up so we never
	// underpay; the maker requires the same and re-rests the remainder).
	if *takeAmount > 0 && *takeAmount < wholeAsset {
		assetAtoms = *takeAmount
		btcSats = client.ProportionalBtc(target.GetWantAmount(), assetAtoms, wholeAsset)
	} else if *takeAmount > wholeAsset {
		fatal("-amount %d exceeds the offer's %d %s", *takeAmount, wholeAsset, *asset)
	}
	fmt.Printf("taking sub-asset offer %s by %s: pay %d BTC sats ON-CHAIN, receive %d %s OVER LIGHTNING%s\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), btcSats, assetAtoms, *asset,
		func() string {
			if assetAtoms < wholeAsset {
				return fmt.Sprintf(" (PARTIAL of %d)", wholeAsset)
			}
			return ""
		}())

	// 2. Wire the on-chain leg (bitcoind, or the Sequentia chain for the mixed
	// same-chain shape) + the asset LN node (asset leg). Validate both.
	var btcChain *xchain.BitcoinChain
	var seqChain *xchain.Chain
	if *quoteAsset != "" {
		if *xseqRPCURL == "" {
			fatal("-quote-asset requires -xseq-rpc (the Sequentia node funding the on-chain quote-asset HTLC)")
		}
		seqRPC, err := xliftRPCFromURL(*xseqRPCURL)
		if err != nil {
			fatal("-xseq-rpc: %v", err)
		}
		seqChain = xchain.NewChain(seqRPC, *xseqWallet)
		if _, err := seqChain.BlockCount(); err != nil {
			fatal("sequentia node unreachable: %v", err)
		}
	} else {
		btcRPC, err := xliftRPCFromURL(*btcRPCURL)
		if err != nil {
			fatal("-btc-rpc: %v", err)
		}
		params, err := xchain.BitcoinChainParams(*btcChainName)
		if err != nil {
			fatal("-btc-chain: %v", err)
		}
		btcChain = xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
		if _, err := btcChain.BlockCount(); err != nil {
			fatal("bitcoind unreachable: %v", err)
		}
	}
	if _, err := xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset).NodeID(); err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", *assetLnSocket, err)
	}

	// Never clobber a prior session that may still hold a refundable BTC leg:
	// overwriting its secret/keys/script would strand those coins forever (the guard
	// xlift and xsell already have).
	if raw, err := ioutil.ReadFile(*stateFile); err == nil {
		var old xsubasState
		if json.Unmarshal(raw, &old) == nil && old.Status != "" &&
			old.Status != "settled" && !strings.HasPrefix(old.Status, "refunded") {
			fatal("state file %s holds a session with status %q (possibly a refundable BTC leg); refund it or pass a different -state-file", *stateFile, old.Status)
		}
	}
	// 3. Mint the secret P + the BTC refund key, and persist the session BEFORE
	//    funding so a crash still leaves a recoverable refund record.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		fatal("mint secret: %v", err)
	}
	hashArr := sha256.Sum256(secret)
	btcRefundKey, err := xchain.NewKey()
	if err != nil {
		fatal("mint refund key: %v", err)
	}
	// H = the device invoice's hash in external mode (we hold no P), else SHA256(P).
	hashHex := hex.EncodeToString(hashArr[:])
	secretHex := hex.EncodeToString(secret)
	if extHashH != nil {
		hashHex = hex.EncodeToString(extHashH)
		secretHex = "" // device-held; this taker never learns the preimage
	}
	st := &xsubasState{
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		Relay:            *relay,
		OfferID:          target.GetOfferId(),
		MakerPubkey:      target.GetMakerPubkey(),
		Asset:            *asset,
		QuoteAsset:       *quoteAsset,
		AssetAmount:      assetAtoms,
		BtcAmount:        btcSats,
		SecretHex:        secretHex,
		HashHex:          hashHex,
		BtcRefundPrivHex: hex.EncodeToString(btcRefundKey.Bytes()),
		Status:           "opening",
	}
	st.save(*stateFile)

	// 4. Open the swap session; E2E key from the SIGNED offer.
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
		TakeAmount:         assetAtoms, // T8: the slice we're taking (== base_amount for a whole lift)
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}})
	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sid := la.GetLiftAccepted().GetSessionId()
	st.SessionID = sid
	st.save(*stateFile)
	fmt.Printf("sub-asset swap session %s opened\n", sid)

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

	// 5. Run the taker sub-asset driver over the WS courier.
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

	if *invoiceMode != "plain" && *invoiceMode != "hold" {
		fatal("-asset-invoice must be plain or hold")
	}
	// External mode builds the BTC HTLC on the device-supplied H (not SHA256(secret)).
	htlcLock := xchain.NewHashLock(secret)
	if extHashH != nil {
		htlcLock = xchain.NewHashLockFromHash(extHashH)
	}
	var takerOps client.SubAssetTakerOps
	if *quoteAsset != "" {
		takerOps = client.NewLiveSubAssetTakerOpsSeq(seqChain, *quoteAsset,
			xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset), htlcLock,
			*invoiceMode == "plain" && *takerLnNodeID == "")
	} else {
		takerOps = &client.LiveSubAssetTakerOps{
			Swap:    xchain.NewSwapBitcoin(btcChain, nil, htlcLock),
			AssetLN: xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset),
			BTC:     btcChain,
			Plain:   *invoiceMode == "plain" && *takerLnNodeID == "",
		}
	}
	res, err := client.RunTakerSubAsset(client.TakerSubAssetParams{
		Ops:                  takerOps,
		Crypter:              crypter,
		BtcAmount:            btcSats,
		AssetAmount:          assetAtoms,
		MinBTCConf:           *minBTCConf,
		SpendFeeSats:         *spendFee,
		BtcRefundKey:         btcRefundKey,
		Preimage:             secret,
		ExternalHashH:        extHashH,
		ExternalBolt11:       *extBolt11,
		TakerLNNodeID:        *takerLnNodeID,
		ExternalBtcLeg:       extBtcLeg,
		ExternalBtcRefundPub: extBtcRefundPubB,
		OnBtcLegFunded: func(r *client.TakerSubAssetResult) {
			if r.BtcLeg != nil {
				st.BtcLegTxid = r.BtcLeg.Funded.TxID
				st.BtcLegVout = r.BtcLeg.Funded.Vout
				st.BtcLegAmount = r.BtcLeg.Funded.Amount
				st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
				st.BtcLocktime = r.BtcLocktime
				st.Status = "btc_funded"
				st.save(*stateFile)
				fmt.Printf("BTC HTLC funded: %s (refund after T_btc=%d; refund needs -state-file %s)\n",
					r.BtcLeg.Funded.TxID, r.BtcLocktime, *stateFile)
			}
		},
		Timing: client.XcTiming{TermsWait: *termsWait, SeqLockWait: *settleWait},
		Log:    func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	}, send, recv)

	if err != nil {
		fmt.Printf("sub-asset swap ended: %v\n", err)
		// If we funded but did not receive the asset, the BTC HTLC is refundable.
		if res != nil && res.BtcLeg != nil && !res.Received {
			st.Status = "aborted_refundable"
			st.save(*stateFile)
			if *refundWait {
				fmt.Printf("refunding the on-chain HTLC after T=%d (polling)...\n", res.BtcLocktime)
				txid, rerr := client.RefundTakerOnchainLeg(takerOps, res.BtcLeg, btcRefundKey, res.BtcLocktime, *spendFee, true, 15*time.Second)
				if rerr != nil {
					fatal("refund: %v", rerr)
				}
				st.Status = "refunded: " + txid
				st.save(*stateFile)
				fmt.Printf("BTC HTLC refunded: %s\n", txid)
			} else {
				fmt.Printf("recover the BTC HTLC after parent height %d with:\n  seqob-cli xsubas-refund -state-file %s -btc-rpc <url> -btc-wallet %s -btc-chain %s -wait\n",
					res.BtcLocktime, *stateFile, *btcWallet, *btcChainName)
			}
		}
		return
	}

	if res.Held {
		// DEVICE-HODL BUY: the maker's asset payment is HELD at our hosted node. We do NOT
		// settle here — the DEVICE settles (POST /node/settle) to release the asset and
		// reveal P to the maker, who then claims our BTC HTLC.
		st.Status = "held_device_settle"
		st.save(*stateFile)
		fmt.Printf("HODL BUY: the maker's asset payment is HELD on H=%s. Settle from the DEVICE (POST /node/settle {payment_hash, preimage:P}) to release the asset and reveal P to the maker (who then claims your BTC HTLC).\n", st.HashHex)
		return
	}

	st.Status = "settled"
	st.SeqPreimageHex = hex.EncodeToString(res.Preimage)
	st.save(*stateFile)
	fmt.Printf("SUB-ASSET SWAP SETTLED: paid %d BTC sats on-chain, received %d %s over Lightning; preimage %s\n",
		btcSats, assetAtoms, *asset, hex.EncodeToString(res.Preimage))
	fmt.Printf("  (the maker now claims your on-chain BTC HTLC with the revealed preimage)\n")
}

// cmdXSubAsRefund recovers the BTC HTLC of an aborted xsubas after T_btc.
func cmdXSubAsRefund(args []string) {
	fs := newFlagSet("xsubas-refund")
	stateFile := fs.String("state-file", "xsubas-session.json", "session state written by xsubas")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required for a BTC leg)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	xseqRPCURL := envString(fs, "xseq-rpc", "SEQOB_XSEQ_RPC", "mixed same-chain (state has quote_asset): Sequentia node RPC URL")
	xseqWallet := fs.String("xseq-wallet", "", "mixed same-chain: Sequentia node wallet")
	spendFee := fs.Uint64("spend-fee", 1000, "refund fee (sats)")
	wait := fs.Bool("wait", false, "poll until T_btc passes instead of failing when early")
	_ = fs.Parse(args)

	raw, err := ioutil.ReadFile(*stateFile)
	if err != nil {
		fatal("read state: %v", err)
	}
	st, err := loadXSubAsState(raw)
	if err != nil {
		fatal("parse state: %v", err)
	}
	if st.BtcLegTxid == "" || st.BtcLocktime == 0 {
		fatal("state has no funded BTC leg to refund")
	}
	if st.Status == "settled" {
		fatal("session settled (the maker owns the BTC leg); nothing to refund")
	}
	script, err := hex.DecodeString(st.BtcLegScriptHex)
	if err != nil {
		fatal("state btc_leg_script_hex: %v", err)
	}
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil {
		fatal("state hash_hex: %v", err)
	}
	keyBytes, err := hex.DecodeString(st.BtcRefundPrivHex)
	if err != nil {
		fatal("state btc_refund_priv_hex: %v", err)
	}
	leg := &xchain.LegLock{
		Script:   script,
		Funded:   &xchain.FundedHTLC{TxID: st.BtcLegTxid, Vout: st.BtcLegVout, Amount: st.BtcLegAmount},
		Locktime: st.BtcLocktime,
	}
	var txid string
	if st.QuoteAsset != "" {
		// Mixed same-chain: the funded leg is a Sequentia HTLC on the quote asset.
		if *xseqRPCURL == "" {
			fatal("state has quote_asset %s: xsubas-refund requires -xseq-rpc", st.QuoteAsset)
		}
		seqRPC, rerr := xliftRPCFromURL(*xseqRPCURL)
		if rerr != nil {
			fatal("-xseq-rpc: %v", rerr)
		}
		seqChain := xchain.NewChain(seqRPC, *xseqWallet)
		ops := client.NewLiveSubAssetTakerOpsSeq(seqChain, st.QuoteAsset, nil, xchain.NewHashLockFromHash(hashH), false)
		txid, err = client.RefundTakerOnchainLeg(ops, leg, xchain.KeyFromBytes(keyBytes), st.BtcLocktime, *spendFee, *wait, 15*time.Second)
	} else {
		if *btcRPCURL == "" {
			fatal("xsubas-refund requires -btc-rpc")
		}
		btcRPC, rerr := xliftRPCFromURL(*btcRPCURL)
		if rerr != nil {
			fatal("-btc-rpc: %v", rerr)
		}
		params, perr := xchain.BitcoinChainParams(*btcChainName)
		if perr != nil {
			fatal("-btc-chain: %v", perr)
		}
		btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
		txid, err = refundSubAsBTC(btcChain, leg, xchain.KeyFromBytes(keyBytes), hashH, st.BtcLocktime, *spendFee, *wait)
	}
	if err != nil {
		if errors.Is(err, client.ErrXcRefundNotDue) {
			fatal("%v (re-run with -wait to poll until due)", err)
		}
		fatal("refund: %v", err)
	}
	st.Status = "refunded: " + txid
	st.save(*stateFile)
	fmt.Printf("BTC HTLC refunded: %s\n", txid)
}

// refundSubAsBTC reclaims the funded BTC HTLC via the CLTV refund branch, reusing
// the proven polling RefundTakerBTC over a LiveXcOps (BTC side only).
func refundSubAsBTC(btcChain *xchain.BitcoinChain, leg *xchain.LegLock, key *xchain.Key, hashH []byte, locktime uint32, spendFee uint64, wait bool) (string, error) {
	ops := &client.LiveXcOps{
		Swap: xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashH)),
		BTC:  btcChain,
	}
	return client.RefundTakerBTC(ops, leg, key, locktime, spendFee, wait, 15*time.Second)
}
