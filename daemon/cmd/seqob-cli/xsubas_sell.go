package main

// xsubas-sell.go: the SUB-ASSET SELL taker — pay an asset OVER LIGHTNING and receive
// BTC ON-CHAIN. It matches a maker offer where the maker BUYS the asset (ln_direction
// LnBTCForAssetLN): the maker locks BTC on-chain + holds an asset invoice on H; this
// taker verifies the BTC HTLC, pays the asset invoice from its OWN asset LN node (device
// co-signs), learns P when the maker settles, and claims the BTC on-chain (device-signed).
// It drives internal/seqob/client.RunTakerSubAssetSell over the relay courier.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xsubasSellBtcHtlc mirrors the CLI's success-JSON btc_htlc block so a recovering reader
// can reconstruct the settled result verbatim (the wallet claims the BTC on-chain from it).
type xsubasSellBtcHtlc struct {
	Txid              string `json:"txid"`
	Vout              uint32 `json:"vout"`
	Amount            uint64 `json:"amount"`
	RedeemScript      string `json:"redeem_script"`
	TBtc              uint32 `json:"t_btc"`
	TakerClaimPubkey  string `json:"taker_claim_pubkey"`
	MakerRefundPubkey string `json:"maker_refund_pubkey"`
}

// xsubasSellState is the SELL taker's crash-recovery record. It is NOT a refund record like
// the BUY's (the SELL taker never funds the BTC HTLC — the maker does): it is the material an
// LSP needs to reconstruct the settled result (P + the BTC HTLC terms for the wallet to claim)
// if this process dies AFTER paying the asset but before the JSON success line is captured.
// Phase "verified" is written before paying (P unknown); Phase "paid" adds P. The BUY refund
// record and this SELL recovery record are deliberately separate structs.
type xsubasSellState struct {
	CreatedAt     string             `json:"created_at"`
	UpdatedAt     string             `json:"updated_at"`
	Phase         string             `json:"phase"` // "verified" (asset NOT yet paid) | "paid" (P learned)
	Asset         string             `json:"asset,omitempty"`
	HashH         string             `json:"hash_h"`
	Preimage      string             `json:"preimage,omitempty"` // set once Phase == "paid"
	MakerLNNodeID string             `json:"maker_ln_node_id,omitempty"`
	BtcHtlc       *xsubasSellBtcHtlc `json:"btc_htlc"`
}

// save writes the state ATOMICALLY (temp file + rename) so a crash mid-write can never leave
// a torn/partial file for the recovering LSP to misread. It RETURNS an error instead of
// calling fatal: the OnState callback must never abort the swap or suppress the primary JSON
// output. Losing this recovery aid is not fund-fatal (the LSP falls back to querying the node
// for the settled payment), but killing the swap after paying would be.
func (st *xsubasSellState) save(path string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := ioutil.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cmdXSubAsSell(args []string) {
	fs := newFlagSet("xsubas-sell")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex); the pair is <asset>/BTC (required)")
	offerID := fs.String("offer-id", "", "offer id to take (empty: first verified matching sub-asset-sell offer)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoind RPC URL http://user:pass@host:port (required; verifies + claims the maker's BTC HTLC)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet that RECEIVES the claimed BTC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	assetLnSocket := fs.String("asset-ln-socket", "", "the taker's asset LN node (pays the asset over LN) (required)")
	quoteAsset := fs.String("quote-asset", "", "MIXED SAME-CHAIN (rails 7/8): the QUOTE asset id (hex). The maker's on-chain HTLC is on THIS Sequentia asset (verified + claimed via -xseq-rpc) instead of BTC; the pair is <asset>/<quote-asset>.")
	xseqRPCURL := envString(fs, "xseq-rpc", "SEQOB_XSEQ_RPC", "mixed same-chain: Sequentia node RPC URL (verifies + claims the on-chain quote-asset HTLC)")
	xseqWallet := fs.String("xseq-wallet", "", "mixed same-chain: Sequentia node wallet that receives the claimed quote asset")
	takeAmount := fs.Uint64("amount", 0, "PARTIAL FILL: asset atoms to pay over LN (<= the offer). 0 = the whole offer. The BTC you receive on-chain is the proportional price at the offer's rate, rounded UP (you receive the on-chain leg); the maker locks exactly that and re-rests the remainder.")
	minBTCConf := fs.Int("min-btc-conf", 1, "confirmations required on the maker's BTC HTLC before paying the asset")
	spendFee := fs.Uint64("spend-fee", 1000, "BTC HTLC claim fee target (sats)")
	claimPrivHex := fs.String("btc-claim-priv", "", "device BTC claim privkey (32-byte hex); generated if empty (the key that claims the BTC — never given to the LSP)")
	claimPubHex := fs.String("btc-claim-pub", "", "device BTC claim PUBKEY (33-byte hex). LSP-side/device-claim mode: drive the courier + pay the asset, then return P + the HTLC terms WITHOUT claiming — the wallet holds the matching privkey and claims itself. Mutually exclusive with -btc-claim-priv.")
	jsonOut := fs.Bool("json", false, "emit one JSON line {settled,preimage,hash_h,maker_ln_node_id,btc_htlc{...}} for programmatic (LSP) callers")
	stateFile := fs.String("state-file", "", "crash-recovery state file (written atomically). After verifying the maker's BTC HTLC this taker writes H + the full BTC HTLC terms, and after paying the asset it adds the preimage P, so an LSP can reconstruct the settled result if this process crashes after paying but before the JSON line is captured. Empty = disabled (does not change the JSON output).")
	termsWait := fs.Duration("terms-wait", 2*time.Minute, "max wait for the maker's terms")
	payWait := fs.Duration("pay-wait", 15*time.Minute, "max wait for the asset LN payment to settle")
	_ = fs.Parse(args)

	if *asset == "" || *assetLnSocket == "" || (*btcRPCURL == "" && *quoteAsset == "") {
		fatal("xsubas-sell requires -asset, -asset-ln-socket, and -btc-rpc (or -quote-asset + -xseq-rpc for the mixed same-chain shape)")
	}
	if *claimPubHex != "" && *claimPrivHex != "" {
		fatal("-btc-claim-pub (device-claim mode) and -btc-claim-priv are mutually exclusive")
	}

	// 1. Find + verify a matching sub-asset-SELL offer (maker BUYS: LnBTCForAssetLN).
	// Mixed same-chain: the pair's quote is the REAL asset the maker locked on-chain.
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
		// A named maker is honoured on its own (a retry must find "the same maker,
		// whatever its offer id is now": the HTLC's claim key is that maker's).
		if *makerPub != "" && o.GetMakerPubkey() != *makerPub {
			continue
		}
		if *offerID != "" && o.GetOfferId() != *offerID {
			continue
		}
		lt := o.GetLightning()
		if lt == nil || lt.GetLnDirection() != offer.LnBTCForAssetLN {
			continue
		}
		if err := offer.VerifyOffer(o); err != nil {
			continue
		}
		target = o
		break
	}
	if target == nil {
		fatal("no verified sub-asset-SELL offer found to sell %s for on-chain %s", *asset, quote)
	}
	wholeAsset := target.GetWantAmount() // the asset a WHOLE fill pays over LN
	wholeBtc := target.GetOfferAmount()  // the BTC sats a WHOLE fill receives on-chain
	assetAtoms := wholeAsset             // default: take the whole offer
	btcSats := wholeBtc
	// Partial fill: pay a slice and receive the PROPORTIONAL BTC (rounded up — the
	// taker RECEIVES the on-chain leg, so the ceil is in its favour; the maker locks
	// the same ceil-priced slice and re-rests the remainder).
	if *takeAmount > 0 && *takeAmount < wholeAsset {
		assetAtoms = *takeAmount
		btcSats = client.ProportionalBtc(wholeBtc, assetAtoms, wholeAsset)
	} else if *takeAmount > wholeAsset {
		fatal("-amount %d exceeds the offer's %d %s", *takeAmount, wholeAsset, *asset)
	}
	fmt.Printf("taking sub-asset-SELL offer %s by %s: pay %d %s OVER LIGHTNING, receive %d BTC sats ON-CHAIN%s\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), assetAtoms, *asset, btcSats,
		func() string {
			if assetAtoms < wholeAsset {
				return fmt.Sprintf(" (PARTIAL: %d of %d)", assetAtoms, wholeAsset)
			}
			return ""
		}())

	// 2. Wire the on-chain claim leg (bitcoind, or the Sequentia chain for the
	// mixed same-chain shape) + the taker's asset LN node (pays). Validate.
	var btcChain *xchain.BitcoinChain
	var seqChain *xchain.Chain
	if *quoteAsset != "" {
		if *xseqRPCURL == "" {
			fatal("-quote-asset requires -xseq-rpc")
		}
		seqRPC, rerr := xliftRPCFromURL(*xseqRPCURL)
		if rerr != nil {
			fatal("-xseq-rpc: %v", rerr)
		}
		seqChain = xchain.NewChain(seqRPC, *xseqWallet)
		if _, rerr := seqChain.BlockCount(); rerr != nil {
			fatal("sequentia node unreachable: %v", rerr)
		}
	} else {
		btcRPC, rerr := xliftRPCFromURL(*btcRPCURL)
		if rerr != nil {
			fatal("-btc-rpc: %v", rerr)
		}
		params, perr := xchain.BitcoinChainParams(*btcChainName)
		if perr != nil {
			fatal("-btc-chain: %v", perr)
		}
		btcChain = xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
		if _, rerr := btcChain.BlockCount(); rerr != nil {
			fatal("bitcoind unreachable: %v", rerr)
		}
	}
	if _, err := xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset).NodeID(); err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", *assetLnSocket, err)
	}
	// The device BTC claim key. In device-claim (LSP-side) mode only the PUBKEY is provided:
	// the driver returns P + the HTLC terms and the wallet claims with its own privkey.
	var claimKey *xchain.Key
	var externalClaimPub []byte
	if *claimPubHex != "" {
		pb, derr := hex.DecodeString(*claimPubHex)
		if derr != nil || len(pb) != 33 {
			fatal("-btc-claim-pub must be 33-byte compressed pubkey hex")
		}
		externalClaimPub = pb
	} else if *claimPrivHex != "" {
		kb, derr := hex.DecodeString(*claimPrivHex)
		if derr != nil || len(kb) != 32 {
			fatal("-btc-claim-priv must be 32-byte hex")
		}
		claimKey = xchain.KeyFromBytes(kb)
	} else if k, kerr := xchain.NewKey(); kerr != nil {
		fatal("mint claim key: %v", kerr)
	} else {
		claimKey = k
	}

	// 3. Open the swap session; E2E key from the SIGNED offer. The taker's BTC-leg swap
	//    uses a hash-only lock (it learns P from paying the asset invoice).
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
	fmt.Printf("sub-asset-SELL session %s opened\n", sid)

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

	createdAt := time.Now().UTC().Format(time.RFC3339)
	res, err := client.RunTakerSubAssetSell(client.TakerSubAssetSellParams{
		// Bind the BTC-leg swap to the maker's H once Terms arrive (VerifyBTCLeg/
		// ClaimBTCLeg recompute against it); a fresh asset LN leg pays the invoice.
		NewTakerOps: func(hashH []byte) client.SubAssetSellTakerOps {
			if *quoteAsset != "" {
				return client.NewLiveSubAssetSellTakerOpsSeq(seqChain, *quoteAsset,
					xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset), hashH)
			}
			return &client.LiveSubAssetSellTakerOps{
				Swap:    xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashH)),
				AssetLN: xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset),
				BTC:     btcChain,
			}
		},
		Crypter: crypter,
		// The WHOLE signed offer's two sides; the driver derives the priced slice
		// from them + TakeAssetAmount so the maker cannot re-price it.
		BtcAmount:        wholeBtc,
		AssetAmount:      wholeAsset,
		TakeAssetAmount:  assetAtoms,
		MinBTCConf:       *minBTCConf,
		SpendFeeSats:     *spendFee,
		BtcClaimKey:      claimKey,
		ExternalClaimPub: externalClaimPub,
		// Persist the crash-recovery state atomically at each checkpoint (verified, then paid).
		// Best-effort: a write failure is logged to stderr but NEVER aborts the swap or touches
		// the JSON output (that primary path still emits P). Disabled when -state-file is empty.
		OnState: func(s client.SubAssetSellState) {
			if *stateFile == "" {
				return
			}
			st := &xsubasSellState{
				CreatedAt:     createdAt,
				UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
				Phase:         s.Phase,
				Asset:         *asset,
				HashH:         hex.EncodeToString(s.HashH),
				MakerLNNodeID: s.MakerLNNodeID,
				BtcHtlc: &xsubasSellBtcHtlc{
					Txid:              s.BtcTxid,
					Vout:              s.BtcVout,
					Amount:            s.BtcAmount,
					RedeemScript:      s.BtcRedeemHex,
					TBtc:              s.BtcLocktime,
					TakerClaimPubkey:  hex.EncodeToString(s.TakerClaimPub),
					MakerRefundPubkey: hex.EncodeToString(s.MakerRefundPub),
				},
			}
			if len(s.Preimage) > 0 {
				st.Preimage = hex.EncodeToString(s.Preimage)
			}
			if err := st.save(*stateFile); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not persist sub-asset-SELL state to %s: %v\n", *stateFile, err)
			}
		},
		Timing: client.XcTiming{TermsWait: *termsWait, BtcFundWait: *termsWait, SeqLockWait: *payWait},
		Log:    func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	}, send, recv)
	if err != nil {
		if *jsonOut {
			fmt.Printf(`{"settled":false,"error":%q}`+"\n", err.Error())
		} else {
			fmt.Printf("sub-asset-SELL swap ended: %v\n", err)
		}
		return
	}
	if *jsonOut {
		// Device-claim mode: emit P + the HTLC terms for the wallet to claim on-chain itself.
		fmt.Printf(`{"settled":true,"preimage":%q,"hash_h":%q,"maker_ln_node_id":%q,"btc_htlc":{"txid":%q,"vout":%d,"amount":%d,"redeem_script":%q,"t_btc":%d,"taker_claim_pubkey":%q,"maker_refund_pubkey":%q},"btc_claim_txid":%q}`+"\n",
			hex.EncodeToString(res.Preimage), hex.EncodeToString(res.HashH), res.MakerLNNodeID,
			res.BtcTxid, res.BtcVout, btcSats, res.BtcRedeemHex, res.BtcLocktime,
			hex.EncodeToString(res.TakerClaimPub), hex.EncodeToString(res.MakerRefundPub), res.BtcClaimTxid)
		return
	}
	if res.Received {
		fmt.Printf("SUB-ASSET SELL SWAP SETTLED: paid %d %s over Lightning, received %d BTC sats on-chain in %s; preimage %s\n",
			assetAtoms, *asset, btcSats, res.BtcClaimTxid, hex.EncodeToString(res.Preimage))
	} else {
		fmt.Printf("SUB-ASSET SELL: paid %d %s over Lightning + learned P (%s); wallet claims %d BTC sats from HTLC %s:%d\n",
			assetAtoms, *asset, hex.EncodeToString(res.Preimage), btcSats, res.BtcTxid, res.BtcVout)
	}
}
