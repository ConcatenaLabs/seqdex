package main

// xsublift.go: the NORMAL submarine-swap taker — SELL a Sequentia asset for BTC
// over Lightning. The maker (a BUY Lightning offer) pays the taker's BOLT11 and
// claims the asset; the taker funds the asset HTLC (claim=maker), mints the
// invoice on a chosen preimage P, and receives its BTC-LN. It drives
// internal/seqob/client.RunTakerSubmarineNormal over the relay courier, settling
// the asset leg with pkg/xchain against an anchored Sequentia node and the BTC-LN
// leg against the taker's own SeqLN-on-Bitcoin node.
//
// The asset HTLC is on-chain, so the session state (secret, refund key, funded
// leg, T_seq) is persisted to -state-file BEFORE it is funded: if the maker never
// pays, `seqob-cli xrefund-seq` (or RefundTakerSubmarine) reclaims the asset after
// T_seq.

import (
	"crypto/rand"
	"crypto/sha256"
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

type xsubliftState struct {
	CreatedAt        string `json:"created_at"`
	Relay            string `json:"relay"`
	SessionID        string `json:"session_id,omitempty"`
	OfferID          string `json:"offer_id"`
	MakerPubkey      string `json:"maker_pubkey"`
	Asset            string `json:"asset"`
	SeqAmount        uint64 `json:"seq_amount"`
	InvoiceMsat      uint64 `json:"invoice_msat"`
	SecretHex        string `json:"secret_hex"`
	HashHex          string `json:"hash_hex"`
	SeqRefundPrivHex string `json:"seq_refund_priv_hex"`
	InvoiceLabel     string `json:"invoice_label,omitempty"`
	SeqLegTxid       string `json:"seq_leg_txid,omitempty"`
	SeqLegVout       uint32 `json:"seq_leg_vout,omitempty"`
	SeqLegAmount     uint64 `json:"seq_leg_amount,omitempty"`
	SeqLegScriptHex  string `json:"seq_leg_script_hex,omitempty"`
	SeqLocktime      uint32 `json:"seq_locktime,omitempty"`
	Status           string `json:"status"`
}

func (st *xsubliftState) save(path string) {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := ioutil.WriteFile(path, b, 0o600); err != nil {
		fmt.Printf("WARN: could not save state to %s: %v\n", path, err)
	}
}

func cmdXSubLift(args []string) {
	fs := newFlagSet("xsublift")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex) to sell for BTC-LN (required)")
	offerID := fs.String("offer-id", "", "offer id to lift (empty: first verified NORMAL lightning offer for -asset)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet funding the asset leg")
	lnSocket := fs.String("ln-socket", "", "the taker's SeqLN-on-Bitcoin lightning-rpc unix socket (receives the BTC-LN) (required)")
	invoiceCltv := fs.Uint("invoice-cltv", 0, "min_final_cltv_expiry on the minted invoice (0 = node default)")
	spendFee := fs.Uint64("spend-fee", 1000, "asset-HTLC refund fee target in atoms")
	awaitWait := fs.Duration("await-wait", 30*time.Minute, "max wait for the maker to pay our invoice before we consider refunding")
	stateFile := fs.String("state-file", "xsublift-session.json", "session persistence (the asset refund path)")
	_ = fs.Parse(args)

	if *asset == "" {
		fatal("xsublift requires -asset (the hex asset id; the pair is <asset>/BTC)")
	}
	if *seqRPCURL == "" || *lnSocket == "" {
		fatal("xsublift requires -seq-rpc and -ln-socket")
	}

	// 1. Find + verify the offer: only a maker-signed NORMAL lightning offer
	// (ln_direction = LnAssetForBTC, trade_dir = BUY) is liftable here.
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
		if lt == nil || lt.GetLnDirection() != offer.LnAssetForBTC {
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
		fatal("no verified NORMAL lightning offer found for %s/BTC (use `seqob-cli book -base %s -quote BTC`)", *asset, *asset)
	}
	expectAsset := target.GetPair().GetBaseAsset()
	expectSeq := target.GetBaseAmount()          // asset atoms (BUY: want_amount == base_amount)
	expectMsat := target.GetOfferAmount() * 1000 // BTC sats the maker pays -> msat
	fmt.Printf("lifting NORMAL lightning offer %s by %s: sell %d %s, receive %d msat over Lightning\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), expectSeq, expectAsset, expectMsat)

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

	// 3. Secret + refund key, persisted BEFORE any coins move.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		fatal("secret: %v", err)
	}
	hashH := sha256.Sum256(secret)
	seqRefundKey, err := xchain.NewKey()
	if err != nil {
		fatal("key: %v", err)
	}
	if raw, err := ioutil.ReadFile(*stateFile); err == nil {
		var old xsubliftState
		if json.Unmarshal(raw, &old) == nil && old.Status != "" &&
			old.Status != "settled" && !strings.HasPrefix(old.Status, "refunded") {
			fatal("state file %s holds a session with status %q (possibly a refundable asset leg); refund it or pass a different -state-file", *stateFile, old.Status)
		}
	}
	st := &xsubliftState{
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Relay: *relay,
		OfferID: target.GetOfferId(), MakerPubkey: target.GetMakerPubkey(),
		Asset: expectAsset, SeqAmount: expectSeq, InvoiceMsat: expectMsat,
		SecretHex:        hex.EncodeToString(secret),
		HashHex:          hex.EncodeToString(hashH[:]),
		SeqRefundPrivHex: hex.EncodeToString(seqRefundKey.Bytes()),
		Status:           "created",
	}
	st.save(*stateFile)
	fmt.Printf("session state -> %s (keep it: it is the asset refund path)\n", *stateFile)

	// 4. Open the lift session; E2E key from the SIGNED offer.
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
		TakeAmount:         target.GetBaseAmount(), // whole-HTLC lift
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
	fmt.Printf("submarine lift session %s opened\n", sid)

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

	// 5. Run the taker submarine driver over the WS courier.
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
	ops := &client.LiveSubTakerOps{
		Sub: xchain.NewSubmarineSwap(seqChain, xchain.NewCLNLNLeg(*lnSocket), xchain.NewHashLock(secret)),
		SEQ: seqChain,
	}
	res, err := client.RunTakerSubmarineNormal(client.TakerSubmarineParams{
		Ops:               ops,
		Crypter:           crypter,
		Secret:            secret,
		SeqRefundKey:      seqRefundKey,
		ExpectAsset:       expectAsset,
		ExpectSeqAmount:   expectSeq,
		ExpectInvoiceMsat: expectMsat,
		InvoiceCLTV:       uint32(*invoiceCltv),
		SpendFeeAtoms:     *spendFee,
		Timing:            client.XcTiming{BtcConfWait: *awaitWait},
		Log:               func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
		// Persist the funded asset leg the moment it is locked, BEFORE the await:
		// a crash / a maker that never pays must leave the asset refundable.
		OnAssetFunded: func(r *client.TakerSubmarineResult) {
			if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
				st.SeqLegTxid = r.SeqLeg.Funded.TxID
				st.SeqLegVout = r.SeqLeg.Funded.Vout
				st.SeqLegAmount = r.SeqLeg.Funded.Amount
				st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
			}
			st.SeqLocktime = r.SeqLocktime
			st.InvoiceLabel = r.InvoiceLabel
			st.Status = "asset_funded"
			st.save(*stateFile)
		},
	}, send, recv)
	if err != nil {
		st.Status = "aborted"
		st.save(*stateFile)
		fmt.Printf("submarine lift ended: %v\n", err)
		if st.SeqLegTxid != "" {
			fmt.Printf("the asset HTLC %s:%d is refundable after T_seq=%d via `seqob-cli xrefund-seq -state-file %s ...` (or RefundTakerSubmarine)\n",
				st.SeqLegTxid, st.SeqLegVout, st.SeqLocktime, *stateFile)
		}
		return
	}
	st.Status = "settled"
	st.save(*stateFile)
	fmt.Printf("SUBMARINE SWAP SETTLED: received %d msat over Lightning; the maker claimed the asset.\n", res.PaidMsat)
}
