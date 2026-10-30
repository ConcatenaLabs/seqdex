package main

// xsell.go: the CROSS-CHAIN reverse taker (sell a Sequentia asset for real BTC)
// over the order book. It drives internal/seqob/client.RunTakerReverse: the
// MAKER holds the secret and funds the BTC leg first; this taker verifies that
// leg, funds its asset leg, then claims the BTC once the maker's asset claim
// reveals the secret on-chain. The asset leg is persisted to -state-file BEFORE
// it is funded so `seqob-cli xrefund-seq` can recover it after T_seq.

import (
	"bytes"
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

// xsellState is the persisted reverse-taker session (recovery material for the
// asset-leg refund after T_seq).
type xsellState struct {
	CreatedAt        string `json:"created_at"`
	Relay            string `json:"relay"`
	OfferID          string `json:"offer_id"`
	MakerPubkey      string `json:"maker_pubkey"`
	Asset            string `json:"asset"`
	SeqAmount        uint64 `json:"seq_amount"`
	BtcAmount        uint64 `json:"btc_amount"`
	BtcClaimPrivHex  string `json:"btc_claim_priv_hex"`  // claims the maker's BTC leg with s
	SeqRefundPrivHex string `json:"seq_refund_priv_hex"` // refunds our asset leg after T_seq
	SessionID        string `json:"session_id,omitempty"`
	SeqLegTxid       string `json:"seq_leg_txid,omitempty"`
	SeqLegVout       uint32 `json:"seq_leg_vout"`
	SeqLegAmount     uint64 `json:"seq_leg_amount,omitempty"`
	SeqLegAsset      string `json:"seq_leg_asset,omitempty"`
	SeqLegScriptHex  string `json:"seq_leg_script_hex,omitempty"`
	SeqLocktime      uint32 `json:"seq_locktime,omitempty"`
	BtcClaimTxid     string `json:"btc_claim_txid,omitempty"`
	Status           string `json:"status"`
}

func (st *xsellState) save(path string) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fatal("marshal state: %v", err)
	}
	if err := ioutil.WriteFile(path, b, 0o600); err != nil {
		fatal("write state file %s: %v", path, err)
	}
}

func cmdXSell(args []string) {
	fs := newFlagSet("xsell")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex) to sell for BTC (required)")
	offerID := fs.String("offer-id", "", "offer id to lift (empty: first verified reverse cross offer for -asset)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "DEPRECATED and ignored: the taker session key is minted fresh per run. A reused session key derives the same courier key with the same maker for every lift, which lets a relay replay one session's frames into another")
	btcRPCURL := fs.String("btc-rpc", "", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet receiving the BTC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	seqRPCURL := fs.String("seq-rpc", "", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet funding the asset leg")
	minBTCConf := fs.Int("min-btc-conf", 1, "confirmations required on the maker's BTC leg before we fund the asset")
	takeAmount := fs.Uint64("amount", 0, "PARTIAL FILL: asset atoms to sell (<= the offer). 0 = the whole offer. The BTC you receive is the proportional price (floor, in the maker's favour); the maker re-rests the remainder.")
	spendFee := fs.Uint64("spend-fee", 1000, "HTLC spend fee target in native sats (converted per-asset)")
	btcFeeRate := fs.Float64("btc-fee-rate", 0, "the least sat/vB a BTC-side spend pays; it pays the live going rate, never under this (0: the chain's floor)")
	maxFeeBtc := fs.Uint64("max-fee-btc", 0, "max maker fee_btc (sats) we accept")
	stateFile := fs.String("state-file", "xsell-session.json", "session persistence (refund needs this)")
	_ = fs.Parse(args)

	if *asset == "" {
		fatal("xsell requires -asset (the hex asset id; the pair is <asset>/BTC)")
	}
	if *btcRPCURL == "" || *seqRPCURL == "" {
		fatal("xsell requires -btc-rpc and -seq-rpc")
	}

	// 1. Find + verify a reverse (ASSET_TO_BTC) cross offer.
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, offer.BTCSentinel), &book); err != nil {
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
		if o.GetCrossChain() == nil || o.GetCrossChain().GetDirection() != offer.DirAssetToBTC {
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
		fatal("no verified reverse cross offer found for %s/BTC", *asset)
	}
	expectAsset := target.GetPair().GetBaseAsset()
	wholeSeq := target.GetWantAmount()  // maker BUY: wants the asset (whole offer)
	wholeBtc := target.GetOfferAmount() // maker gives BTC (whole offer)
	// Partial fill: sell a slice and receive the PROPORTIONAL BTC (floor, so the
	// maker — who GIVES the BTC — never overpays; the maker requires the same and
	// re-rests the remainder).
	takeSeq := wholeSeq
	if *takeAmount > 0 && *takeAmount < wholeSeq {
		takeSeq = *takeAmount
	} else if *takeAmount > wholeSeq {
		fatal("-amount %d exceeds the offer's %d %s", *takeAmount, wholeSeq, expectAsset)
	}
	recvBtc := client.ProportionalBtcFloor(wholeBtc, takeSeq, wholeSeq)
	if recvBtc == 0 {
		fatal("-amount %d of %d prices to 0 sats (too small a slice for this offer)", takeSeq, wholeSeq)
	}
	// Minimum-slice guard: a partial that prices to a sub-dust BTC leg (or is below
	// the offer's advertised min_fill) would strand an unclaimable HTLC leg. Reject
	// it here, before any coins move; the driver re-checks defensively.
	if err := guardPartialBtcLeg(takeSeq, wholeSeq, recvBtc, target.GetMinFill(), *spendFee); err != nil {
		fatal("%v", err)
	}
	// The amounts THIS lift settles: the slice of asset, and its proportional BTC.
	expectSeq := takeSeq
	expectBtc := recvBtc
	fmt.Printf("selling into cross offer %s by %s: %d %s for %d sats%s\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), expectSeq, expectAsset, expectBtc,
		partialNote(takeSeq, wholeSeq))

	btcRPC, err := xliftRPCFromURL(*btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(*btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
	btcChain.SetFeeRate(*btcFeeRate)
	seqChain := xchain.NewChain(seqRPC, *seqWallet)

	// 2. Throwaway settlement keys, persisted before any coins move.
	btcClaimKey, err := xchain.NewKey()
	if err != nil {
		fatal("key: %v", err)
	}
	seqRefundKey, err := xchain.NewKey()
	if err != nil {
		fatal("key: %v", err)
	}
	if raw, err := ioutil.ReadFile(*stateFile); err == nil {
		var old xsellState
		if json.Unmarshal(raw, &old) == nil && old.Status != "" &&
			old.Status != "settled" && !strings.HasPrefix(old.Status, "refunded") {
			fatal("state file %s holds a session with status %q; refund it or use a different -state-file", *stateFile, old.Status)
		}
	}
	st := &xsellState{
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Relay: *relay,
		OfferID: target.GetOfferId(), MakerPubkey: target.GetMakerPubkey(),
		Asset: expectAsset, SeqAmount: expectSeq, BtcAmount: expectBtc,
		BtcClaimPrivHex:  hex.EncodeToString(btcClaimKey.Bytes()),
		SeqRefundPrivHex: hex.EncodeToString(seqRefundKey.Bytes()),
		Status:           "created",
	}
	st.save(*stateFile)

	// 3. Open the lift session.
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
		TakeAmount:         takeSeq, // the slice we're selling (== base_amount for a whole lift)
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
	fmt.Printf("reverse cross lift session %s opened\n", sid)

	makerOfferPub, err := hex.DecodeString(target.GetMakerPubkey())
	if err != nil {
		fatal("decode maker pubkey: %v", err)
	}
	pk, err := btcec.ParsePubKey(makerOfferPub)
	if err != nil {
		fatal("parse maker pubkey: %v", err)
	}
	if echo := la.GetLiftAccepted().GetMakerSessionPubkey(); len(echo) > 0 && !bytes.Equal(echo, makerOfferPub) {
		fatal("relay MakerSessionPubkey echo mismatch (possible MITM); aborting")
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
	recv := func(timeout time.Duration) ([]byte, error) {
		from, err := readWSUntilSwap(conn, timeout)
		if err != nil {
			return nil, err
		}
		return from.GetSwapMsg().GetCiphertext(), nil
	}
	res, err := client.RunTakerReverse(client.TakerReverseParams{
		NewOps: func(hashH []byte) (client.XcOps, error) {
			swp := xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(hashH))
			return &client.LiveXcOps{Swap: swp, BTC: btcChain, SEQ: seqChain}, nil
		},
		Crypter:         crypter,
		BtcClaimKey:     btcClaimKey,
		SeqRefundKey:    seqRefundKey,
		ExpectAsset:     expectAsset,
		ExpectSeqAmount: wholeSeq, // the WHOLE signed offer; the driver checks terms against it
		ExpectBtcAmount: wholeBtc,
		TakeSeqAmount:   takeSeq, // the slice; the driver requires the proportional BTC for it
		MaxFeeBtc:       *maxFeeBtc,
		MinBTCConf:      *minBTCConf,
		SpendFeeSats:    *spendFee,
		Log:             func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
		OnSeqLegFunded: func(r *client.TakerReverseResult) {
			if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
				st.SeqLegTxid = r.SeqLeg.Funded.TxID
				st.SeqLegVout = r.SeqLeg.Funded.Vout
				st.SeqLegAmount = r.SeqLeg.Funded.Amount
				st.SeqLegAsset = r.SeqLeg.Funded.AssetID
				st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
			}
			st.SeqLocktime = r.SeqLocktime
			st.Status = "seq_funded"
			st.save(*stateFile)
			fmt.Printf("asset leg funded and persisted (%s); watching for the maker's claim\n", st.SeqLegTxid)
		},
	}, send, recv)

	if res != nil {
		if res.SeqLeg != nil && res.SeqLeg.Funded != nil {
			st.SeqLegTxid = res.SeqLeg.Funded.TxID
			st.SeqLegVout = res.SeqLeg.Funded.Vout
			st.SeqLegAmount = res.SeqLeg.Funded.Amount
			st.SeqLegAsset = res.SeqLeg.Funded.AssetID
			st.SeqLegScriptHex = hex.EncodeToString(res.SeqLeg.Script)
		}
		st.SeqLocktime = res.SeqLocktime
		st.BtcClaimTxid = res.BtcClaimTxid
	}
	if err != nil {
		st.Status = "aborted: " + err.Error()
		st.save(*stateFile)
		if st.SeqLegTxid != "" {
			fmt.Printf("xsell aborted AFTER funding the asset leg %s.\n", st.SeqLegTxid)
			fmt.Printf("recover it after SEQ height %d with:\n  seqob-cli xrefund-seq -state-file %s -seq-rpc <url> -seq-wallet %s -wait\n",
				st.SeqLocktime, *stateFile, *seqWallet)
		}
		fatal("xsell: %v", err)
	}
	st.Status = "settled"
	st.save(*stateFile)
	fmt.Printf("REVERSE CROSS SWAP SETTLED: sold %d %s for BTC, claimed in %s\n", expectSeq, expectAsset, res.BtcClaimTxid)
}

func cmdXRefundSeq(args []string) {
	fs := newFlagSet("xrefund-seq")
	stateFile := fs.String("state-file", "xsell-session.json", "session state written by xsell")
	seqRPCURL := fs.String("seq-rpc", "", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet")
	spendFee := fs.Uint64("spend-fee", 1000, "refund fee target in native sats")
	wait := fs.Bool("wait", false, "poll until T_seq passes instead of failing when early")
	_ = fs.Parse(args)

	raw, err := ioutil.ReadFile(*stateFile)
	if err != nil {
		fatal("read state: %v", err)
	}
	var st xsellState
	if err := json.Unmarshal(raw, &st); err != nil {
		fatal("parse state: %v", err)
	}
	if st.SeqLegTxid == "" || st.SeqLocktime == 0 {
		fatal("state has no funded asset leg to refund")
	}
	if st.Status == "settled" {
		fatal("session settled (the maker owns the asset leg); nothing to refund")
	}
	if *seqRPCURL == "" {
		fatal("xrefund-seq requires -seq-rpc")
	}
	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, *seqWallet)

	script, err := hex.DecodeString(st.SeqLegScriptHex)
	if err != nil {
		fatal("state seq_leg_script_hex: %v", err)
	}
	keyBytes, err := hex.DecodeString(st.SeqRefundPrivHex)
	if err != nil {
		fatal("state seq_refund_priv_hex: %v", err)
	}
	leg := &xchain.LegLock{
		Script:   script,
		Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
		Locktime: st.SeqLocktime,
	}
	// The refund path only touches the SEQ side, but NewSwapBitcoin builds its BTC
	// backend EAGERLY and reads the chain's params, so passing nil segfaults at
	// construction — this recovery path would have panicked instead of returning the
	// asset. Hand it the same inert stand-in xfund-seq uses: never dialled, params only.
	btcParams, err := xchain.BitcoinChainParams("testnet4")
	if err != nil {
		fatal("chain params: %v", err)
	}
	dummyBtc := xchain.NewBitcoinChain(xchain.NewRPC("127.0.0.1", 1, "x", "x"), "", btcParams)
	ops := &client.LiveXcOps{
		Swap: xchain.NewSwapBitcoin(dummyBtc, seqChain, xchain.NewHashLockFromHash(make([]byte, 32))),
		SEQ:  seqChain,
	}
	txid, err := client.RefundTakerSEQ(ops, leg, xchain.KeyFromBytes(keyBytes), st.SeqLocktime, st.SeqLegAsset, *spendFee, *wait, 15*time.Second)
	if err != nil {
		if errors.Is(err, client.ErrXcRefundNotDue) {
			fatal("%v (re-run with -wait to poll until due)", err)
		}
		fatal("refund: %v", err)
	}
	st.Status = "refunded: " + txid
	st.save(*stateFile)
	fmt.Printf("asset leg refunded: %s\n", txid)
}
