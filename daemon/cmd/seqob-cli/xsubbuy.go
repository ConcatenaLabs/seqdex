package main

// xsubbuy.go: the REVERSE submarine-swap taker — BUY a Sequentia asset with BTC
// over Lightning. The maker (a SELL Lightning offer, maker-secret mode) generates
// P, locks the asset HTLC (claim=taker), and issues a plain invoice on H. The
// taker verifies the HTLC, runs the anchor-depth gate BEFORE paying (a plain
// invoice is irreversible once paid), pays the invoice (learning P), and claims
// the asset. It drives internal/seqob/client.RunTakerReverseSubmarine over the
// relay courier.

import (
	"encoding/hex"
	"encoding/json"
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

type xsubbuyState struct {
	CreatedAt       string `json:"created_at"`
	Relay           string `json:"relay"`
	SessionID       string `json:"session_id,omitempty"`
	OfferID         string `json:"offer_id"`
	MakerPubkey     string `json:"maker_pubkey"`
	Asset           string `json:"asset"`
	SeqAmount       uint64 `json:"seq_amount"`
	InvoiceMsat     uint64 `json:"invoice_msat"`
	SeqClaimPrivHex string `json:"seq_claim_priv_hex"`
	// Filled once the invoice is paid (irreversible): the ONLY key to the asset.
	PreimageHex     string `json:"preimage_hex,omitempty"`
	SeqLegTxid      string `json:"seq_leg_txid,omitempty"`
	SeqLegVout      uint32 `json:"seq_leg_vout,omitempty"`
	SeqLegScriptHex string `json:"seq_leg_script_hex,omitempty"`
	Status          string `json:"status"`
}

func (st *xsubbuyState) save(path string) {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := ioutil.WriteFile(path, b, 0o600); err != nil {
		fmt.Printf("WARN: could not save state to %s: %v\n", path, err)
	}
}

func cmdXSubBuy(args []string) {
	fs := newFlagSet("xsubbuy")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex) to buy with BTC-LN (required)")
	offerID := fs.String("offer-id", "", "offer id to lift (empty: first verified REVERSE lightning offer for -asset)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "DEPRECATED and ignored: the taker session key is minted fresh per run. A reused session key derives the same courier key with the same maker for every lift, which lets a relay replay one session's frames into another")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet receiving the asset")
	lnSocket := fs.String("ln-socket", "", "the taker's SeqLN-on-Bitcoin lightning-rpc unix socket (pays the invoice) (required)")
	minAnchor := fs.Int64("min-anchor-depth", 3, "Bitcoin-anchor depth required on the maker's asset HTLC before we pay (>=2; the taker's cross-leg gate)")
	max0conf := fs.Uint64("max-0conf", 0, "0-conf LP-fronting cap (asset atoms): if the asset leg is <= this, pay WITHOUT waiting for the anchor bury (instant, accepting Bitcoin-reorg risk). 0 = use the maker offer's advertised cap; anything set overrides it.")
	spendFee := fs.Uint64("spend-fee", 1000, "asset-claim fee target in native sats (sized per-asset via the fee market)")
	anchorWait := fs.Duration("anchor-wait", 20*time.Minute, "max wait for the asset HTLC to bury before paying")
	stateFile := fs.String("state-file", "xsubbuy-session.json", "session persistence (holds P after paying)")
	_ = fs.Parse(args)

	// The anchor gate must wait for the maker's asset-HTLC SEQ block to be buried by
	// min-anchor-depth BITCOIN blocks. testnet4 blocks are irregular — up to ~20 min apart
	// under the 20-minute difficulty-reset rule — so a fixed 20 min wait CANNOT cover the
	// default depth of 3 (you can't bury 3 Bitcoin blocks in 20 min), which deadlocks the
	// swap: the taker times out ("anchor not buried to depth N") before the gate can open.
	// Scale the wait to the requested depth (~20 min/block budget), unless the caller asked
	// for a larger one explicitly. min-anchor-depth 0 (the max-0conf instant path) is exempt.
	if *minAnchor > 0 {
		if want := time.Duration(*minAnchor) * 20 * time.Minute; *anchorWait < want {
			*anchorWait = want
		}
	}

	if *asset == "" {
		fatal("xsubbuy requires -asset (the hex asset id; the pair is <asset>/BTC)")
	}
	if *seqRPCURL == "" || *lnSocket == "" {
		fatal("xsubbuy requires -seq-rpc and -ln-socket")
	}

	// 1. Find + verify a REVERSE lightning offer (ln_direction = LnBTCForAsset,
	// trade_dir = SELL): the maker sells the asset for BTC-LN.
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, offer.BTCSentinel), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if *offerID != "" && (o.GetOfferId() != *offerID || o.GetMakerPubkey() != *makerPub) {
			continue
		}
		lt := o.GetLightning()
		if lt == nil || lt.GetLnDirection() != offer.LnBTCForAsset {
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
		fatal("no verified REVERSE lightning offer found for %s/BTC (use `seqob-cli book -base %s -quote BTC`)", *asset, *asset)
	}
	expectAsset := target.GetPair().GetBaseAsset()
	expectSeq := target.GetBaseAmount()         // asset atoms (SELL: offer_amount == base_amount)
	expectMsat := target.GetWantAmount() * 1000 // BTC sats the maker wants -> msat
	// Effective 0-conf cap: an explicit -max-0conf overrides; otherwise honor the cap
	// the maker advertised in the offer's LightningTerms (single source of truth).
	effMax0conf := *max0conf
	if effMax0conf == 0 {
		effMax0conf = target.GetLightning().GetMax_0ConfAmount()
	}
	instant := effMax0conf > 0 && expectSeq <= effMax0conf
	fmt.Printf("lifting REVERSE lightning offer %s by %s: buy %d %s for %d msat over Lightning (0-conf cap=%d -> instant=%v)\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), expectSeq, expectAsset, expectMsat, effMax0conf, instant)

	// 2. Settlement: the anchored Sequentia node (asset leg) + our LN node.
	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, *seqWallet)
	if _, err := seqChain.BlockCount(); err != nil {
		fatal("sequentia node unreachable: %v", err)
	}
	if _, err := xchain.NewCLNLNLeg(*lnSocket).NodeID(); err != nil {
		fatal("lightning-rpc %s unreachable: %v", *lnSocket, err)
	}

	// 3. Fresh claim key, persisted BEFORE we pay.
	seqClaimKey, err := xchain.NewKey()
	if err != nil {
		fatal("key: %v", err)
	}
	if raw, err := ioutil.ReadFile(*stateFile); err == nil {
		var old xsubbuyState
		if json.Unmarshal(raw, &old) == nil && old.PreimageHex != "" && old.Status != "settled" {
			fatal("state file %s holds a PAID-but-unclaimed session (preimage present); claim it or pass a different -state-file", *stateFile)
		}
	}
	st := &xsubbuyState{
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Relay: *relay,
		OfferID: target.GetOfferId(), MakerPubkey: target.GetMakerPubkey(),
		Asset: expectAsset, SeqAmount: expectSeq, InvoiceMsat: expectMsat,
		SeqClaimPrivHex: hex.EncodeToString(seqClaimKey.Bytes()),
		Status:          "created",
	}
	st.save(*stateFile)
	fmt.Printf("session state -> %s\n", *stateFile)

	// 4. Open the lift session.
	_ = priv // deprecated: never reuse a session key across lifts
	takerKey := loadOrGenKey("")
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()
	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            target.GetOfferId(),
		MakerPubkey:        target.GetMakerPubkey(),
		TakeAmount:         target.GetBaseAmount(),
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}})
	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sid := la.GetLiftAccepted().GetSessionId()
	st.SessionID = sid
	st.Status = "session_open"
	st.save(*stateFile)
	fmt.Printf("reverse submarine lift session %s opened\n", sid)

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

	// 5. Run the reverse taker driver over the WS courier.
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
	res, err := client.RunTakerReverseSubmarine(client.TakerReverseSubmarineParams{
		NewTakerOps: func(hashH []byte) client.SubReverseTakerOps {
			return &client.LiveSubReverseTakerOps{Sub: xchain.NewSubmarineSwap(seqChain, xchain.NewCLNLNLeg(*lnSocket), xchain.NewHashLockFromHash(hashH))}
		},
		Crypter:           crypter,
		SeqClaimKey:       seqClaimKey,
		ExpectAsset:       expectAsset,
		ExpectSeqAmount:   expectSeq,
		ExpectInvoiceMsat: expectMsat,
		MinAnchorDepth:    *minAnchor,
		Max0ConfAmount:    effMax0conf,
		SpendFeeAtoms:     *spendFee,
		SeqTip:            seqChain.BlockCount,
		Timing:            client.XcTiming{AnchorWait: *anchorWait},
		Log:               func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
		// Persist P the instant it is learned (BEFORE the claim): a crash after
		// paying must leave the asset claimable.
		OnPaid: func(preimage []byte, leg *xchain.LegLock) {
			st.PreimageHex = hex.EncodeToString(preimage)
			if leg != nil && leg.Funded != nil {
				st.SeqLegTxid = leg.Funded.TxID
				st.SeqLegVout = leg.Funded.Vout
				st.SeqLegScriptHex = hex.EncodeToString(leg.Script)
			}
			st.Status = "paid"
			st.save(*stateFile)
		},
	}, send, recv)
	if err != nil {
		st.Status = "aborted"
		st.save(*stateFile)
		fmt.Printf("reverse submarine lift ended: %v\n", err)
		if st.PreimageHex != "" {
			fmt.Printf("PAID but not claimed — the asset HTLC %s:%d is claimable with the persisted preimage in %s (retry)\n",
				st.SeqLegTxid, st.SeqLegVout, *stateFile)
		}
		return
	}
	st.Status = "settled"
	st.save(*stateFile)
	fmt.Printf("REVERSE SUBMARINE SWAP SETTLED: paid %d msat over Lightning, claimed the asset in %s\n", expectMsat, res.SeqClaimTxid)
}
