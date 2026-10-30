// Command seqob-cli drives the SeqOB relay: post a signed offer, list a market's
// book, and lift an offer. The lift path drives internal/seqob/client (the
// taker proposer + the E2E courier) against a running seqobd. Phase-1 settlement
// uses the in-memory StubWallet, so a lift builds and couriers an encrypted
// SwapRequest; completing it requires a maker process (see internal/seqob/client).
package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"github.com/thanhpk/randstr"
	"github.com/vulpemventures/go-elements/network"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/explorer/esplora"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true}
var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "post":
		cmdPost(os.Args[2:])
	case "book":
		cmdBook(os.Args[2:])
	case "lift":
		cmdLift(os.Args[2:])
	case "covfill":
		cmdCovFill(os.Args[2:])
	case "xlift":
		cmdXLift(os.Args[2:])
	case "xrefund":
		cmdXRefund(os.Args[2:])
	case "xsell":
		cmdXSell(os.Args[2:])
	case "xrefund-seq":
		cmdXRefundSeq(os.Args[2:])
	case "xsublift":
		cmdXSubLift(os.Args[2:])
	case "xsubbuy":
		cmdXSubBuy(os.Args[2:])
	case "xpln":
		cmdXPln(os.Args[2:])
	case "xsubas":
		cmdXSubAs(os.Args[2:])
	case "xsubas-refund":
		cmdXSubAsRefund(os.Args[2:])
	case "xsubas-fund-btc":
		cmdXSubAsFundBtc(os.Args[2:])
	case "xsubas-sell":
		cmdXSubAsSell(os.Args[2:])
	case "xsubas-claim-btc":
		cmdXSubAsClaimBTC(os.Args[2:])
	case "xsubas-refund-btc":
		cmdXSubAsRefundBTC(os.Args[2:])
	case "xsubas-htlc-spend-status":
		cmdXSubAsHtlcSpendStatus(os.Args[2:])
	case "xsubas-node-caps":
		cmdXSubAsNodeCaps(os.Args[2:])
	case "xhtlc-observe":
		cmdXHtlcObserve(os.Args[2:])
	case "xfund-seq":
		cmdXFundSeq(os.Args[2:])
	case "xseq-refund":
		cmdXSeqRefund(os.Args[2:])
	case "keygen":
		cmdKeygen(os.Args[2:])
	case "taker-key":
		cmdTakerKey(os.Args[2:])
	default:
		usage()
	}
	settleBTC()
}

// settleBTC waits, before the command exits, for every Bitcoin HTLC claim or
// refund it broadcast to confirm, bumping each to the going rate while it
// waits (xchain.SettleBTCSpends): no one bumps a spend once its process is
// gone, and a claim that confirms after the other side's timelock, or a refund
// after the other side could still claim, loses the swap.
// SEQOB_BTC_SETTLE bounds the wait (a Go duration; default 6h).
func settleBTC() {
	d := 6 * time.Hour
	if v := os.Getenv("SEQOB_BTC_SETTLE"); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			d = p
		}
	}
	if err := xchain.SettleBTCSpends(d, func(f string, a ...interface{}) { fmt.Fprintf(os.Stderr, f+"\n", a...) }); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; bump it with seqob-cli xsubas-refund-btc -feerate, or wait\n", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `seqob-cli <command> [flags]

commands:
  post    post a signed offer        (flags: -relay -priv -base -quote -dir -base-amount -quote-amount -expiry -fee-asset -recv-addr -id
          -confidential -blinding-pub          blinded-book offer: blech32 -recv-addr + maker blinding pubkey, same-chain co-sign rail
          -ln-direction -ln-node -ln-cltv      Lightning offer: ln_direction 0-5 (2/3 = pure-LN), maker LN node pubkey)
  book    list a market's order book (flags: -relay -base -quote)
  lift    lift a resting offer       (flags: -relay -base -quote -offer-id -maker-pubkey -amount -priv -fee-asset)
  covfill fill resting passive-CLOB COVENANT orders: one atomic wallet-funded FILL tx, order n at input n-1 (no maker liveness, no refund state)
          (flags: -relay -offer-id -maker-pubkey -amount(asset-A atoms, 0=whole; each a comma list) -seq-rpc -seq-wallet [-base -quote -fee-asset -spend-fee])
  xlift   lift a CROSS-CHAIN offer: buy the asset with real BTC over the HTLC courier
          (flags: -relay -asset -offer-id -maker-pubkey -btc-rpc -btc-wallet -btc-chain -seq-rpc -seq-wallet -state-file)
  xrefund recover the BTC leg of an aborted xlift after T_btc (flags: -state-file -btc-rpc -btc-wallet -btc-chain -wait)
  xsell   sell the asset for real BTC over a REVERSE cross offer (maker holds the secret, funds BTC first)
          (flags: -relay -asset -offer-id -maker-pubkey -btc-rpc -btc-wallet -btc-chain -seq-rpc -seq-wallet -state-file)
  xrefund-seq  recover the asset leg of an aborted xsell after T_seq (flags: -state-file -seq-rpc -seq-wallet -wait)
  xsublift  sell the asset for BTC over LIGHTNING (submarine swap): fund the asset HTLC, receive BTC-LN
          (flags: -relay -asset -offer-id -maker-pubkey -seq-rpc -seq-wallet -ln-socket -state-file)
  xsubbuy   buy the asset with BTC over LIGHTNING (reverse submarine): anchor-gate + pay the maker's invoice, claim the asset
          (flags: -relay -asset -offer-id -maker-pubkey -seq-rpc -seq-wallet -ln-socket -min-anchor-depth -state-file)
  xpln    trade the asset against BTC with BOTH legs over LIGHTNING (pure-LN, no on-chain leg / no anchor wait)
          (flags: -relay -side buy|sell -asset -offer-id -maker-pubkey -asset-ln-socket -ln-socket -final-cltv -terms-wait -hold-wait)
  xsubas  buy the asset by paying BTC ON-CHAIN and receiving the asset OVER LIGHTNING (submarine's mirror) [-taker-ln-node-id <hosted node id> for a non-custodial HODL buy: relay H + node id, maker pays the bare hash, the device settles]
          (flags: -relay -asset -offer-id -maker-pubkey -btc-rpc -btc-wallet -btc-chain -asset-ln-socket -min-btc-conf -state-file -refund-wait)
  xsubas-refund  recover the BTC HTLC of an aborted xsubas after T_btc (flags: -state-file -btc-rpc -btc-wallet -btc-chain -wait)
  xsubas-refund-btc  LSP recoup: spend the REFUND/ELSE (CLTV) branch of the payer-bridge BTC HTLC back to the LSP wallet after T_btc (mirror of xsubas-claim-btc)
          (flags: -btc-rpc -btc-wallet -btc-chain -txid -vout -amount -redeem-script -t-btc -refund-priv -spend-fee)
  xsubas-htlc-spend-status  LSP recoup-vs-refund ORACLE: read the AUTHORITATIVE on-chain fate of the payer-bridge BTC HTLC — UNSPENT | SPENT_VIA_CLAIM (returns P) | SPENT_VIA_REFUND — fail-closed on uncertainty (replaces the racy persisted intent flag; needs txindex + unpruned to read a non-wallet maker claim)
          (flags: -btc-rpc -btc-wallet -btc-chain -txid -vout -redeem-script -require-node-caps)
  xsubas-node-caps  LSP payer-bridge BRING-UP gate: assert the BTC node is UNPRUNED + has a SYNCED txindex (so the HTLC-spend classifier can read a non-wallet maker claim). Exit 0 = capable, 1 = refuse payer bridges
          (flags: -btc-rpc -btc-wallet -btc-chain)
  xfund-seq  fund an on-chain Sequentia asset HTLC (claim = the counterparty with P, refund = your own key after T_seq)
          (flags: -seq-rpc -seq-wallet -asset -seq-amount -hash -maker-claim-pub -refund-priv -seq-locktime -no-wait)
  xseq-refund  reclaim an asset HTLC by OUTPOINT after T_seq (the state-file-free mirror of xsubas-refund-btc; recovers an LSP FRONTED asset leg a taker never claimed)
          (flags: -seq-rpc -seq-wallet -txid -vout -amount -asset -redeem-script -t-seq -refund-priv -spend-fee -wait)`)
	os.Exit(2)
}

// --- post ---

func cmdPost(args []string) {
	fs := newFlagSet("post")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	priv := fs.String("priv", "", "maker secret key (32-byte hex); generated if empty")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	dir := fs.String("dir", "sell", "trade direction: sell|buy")
	baseAmt := fs.Uint64("base-amount", 100, "base size (base atoms)")
	quoteAmt := fs.Uint64("quote-amount", 45, "quote size (quote atoms)")
	expiry := fs.Duration("expiry", time.Hour, "time until the offer expires")
	feeAsset := fs.String("fee-asset", "", "preferred fee asset hint (any-asset fee market)")
	recvAddr := fs.String("recv-addr", "", "maker receive address (required; blinded only for a confidential offer)")
	id := fs.String("id", "", "offer id (random 16-byte hex if empty)")
	confidential := fs.Bool("confidential", false, "post to the blinded book: requires a blech32 -recv-addr and -blinding-pub")
	blindingPub := fs.String("blinding-pub", "", "maker blinding pubkey (33-byte compressed hex; required with -confidential)")
	lnDirection := fs.Int("ln-direction", -1, "post a Lightning offer with this ln_direction (0-5); -1 = same-chain settlement")
	lnNode := fs.String("ln-node", "", "maker LN node pubkey (33-byte compressed hex; required for pure-LN and reverse-submarine directions)")
	lnCltv := fs.Uint("ln-cltv", 240, "advisory CLTV delta for the Lightning offer's on-chain leg")
	_ = fs.Parse(args)
	if *recvAddr == "" {
		fatal("post requires -recv-addr (a placeholder address posts an offer nobody can settle)")
	}

	k := loadOrGenKey(*priv)
	o := &seqobv1.Offer{
		OfferId:           orDefault(*id, randstr.Hex(16)),
		SchemaVersion:     1,
		Pair:              &seqobv1.AssetPair{BaseAsset: *base, QuoteAsset: *quote},
		BaseAmount:        *baseAmt,
		AllowPartial:      true,
		CreatedAtUnix:     uint64(time.Now().Unix()),
		ExpiresAtUnix:     uint64(time.Now().Add(*expiry).Unix()),
		FeeAssetHint:      *feeAsset,
		Confidential:      *confidential,
		MakerLnNodePubkey: *lnNode,
	}
	if *lnDirection >= 0 {
		// The resting claim/refund keys are advisory (the validator checks
		// well-formedness only; the load-bearing keys are minted per-lift), so
		// the maker key doubles as both, exactly like the resting pure-LN makers.
		mk := k.PubKey().SerializeCompressed()
		o.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
			LnDirection:    uint32(*lnDirection),
			MakerClaimPub:  mk,
			MakerRefundPub: mk,
			OnchainCltv:    uint32(*lnCltv),
		}}
	} else {
		o.Settlement = &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{
			MakerRecvAddress: *recvAddr,
			MakerBlindingPub: *blindingPub,
		}}
	}
	switch strings.ToLower(*dir) {
	case "sell":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = *base, *baseAmt
		o.WantAsset, o.WantAmount = *quote, *quoteAmt
	case "buy":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = *quote, *quoteAmt
		o.WantAsset, o.WantAmount = *base, *baseAmt
	default:
		fatal("dir must be sell or buy")
	}

	if err := offer.SignOffer(o, k); err != nil {
		fatal("sign offer: %v", err)
	}

	var status seqobv1.OrderStatus
	if err := postJSON(*relay+"/v1/offers", o, &status); err != nil {
		fatal("submit: %v", err)
	}
	fmt.Printf("posted offer %s by maker %s (status %s, active %d)\n",
		status.GetOfferId(), status.GetMakerPubkey(), status.GetStatus(), status.GetActiveAmount())
}

// --- book ---

func cmdBook(args []string) {
	fs := newFlagSet("book")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	_ = fs.Parse(args)

	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *base, *quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	fmt.Printf("order book %s/%s (%d offers)\n", *base, *quote, len(book.GetOffers()))
	for i, o := range book.GetOffers() {
		// SECURITY (ITEM B): the relay is untrusted and serves this list WITHOUT
		// proving the maker signed each row, so a malicious relay can inject
		// fabricated offers. Verify the maker's signature over every offer before
		// displaying it as genuine; flag and skip any that fail (don't render a
		// forged row as if it were a real resting offer).
		if err := offer.VerifyOffer(o); err != nil {
			fmt.Printf("  [UNVERIFIED: bad/missing maker_sig, skipping] id=%s maker=%s: %v\n",
				o.GetOfferId(), short(o.GetMakerPubkey()), err)
			continue
		}
		// What the order can be filled for now (statuses[i], beside the signed offer):
		// a covenant order resting at its remainder holds less than it was signed for.
		listed := o.GetBaseAmount()
		if sts := book.GetStatuses(); i < len(sts) && sts[i].GetOfferId() == o.GetOfferId() {
			listed = sts[i].GetActiveAmount()
		}
		// A Simplicity covenant order names its program by the template hash it
		// signs: say which version it is.
		program := ""
		if sc := o.GetSimplicityCovenant(); sc != nil {
			if t, err := covenant.SimTemplateOf(sc.GetTemplateHash()); err == nil {
				program = "  program=" + t.Name()
			} else {
				program = "  program=unknown template " + sc.GetTemplateHash()
			}
		}
		fmt.Printf("  %s  dir=%s base=%d listed=%d  give %d %s  want %d %s  maker=%s%s\n",
			o.GetOfferId(), shortDir(o.GetTradeDir()), o.GetBaseAmount(), listed,
			o.GetOfferAmount(), o.GetOfferAsset(), o.GetWantAmount(), o.GetWantAsset(),
			short(o.GetMakerPubkey()), program)
	}
}

// --- lift ---

func cmdLift(args []string) {
	fs := newFlagSet("lift")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	offerID := fs.String("offer-id", "", "offer id to lift")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer")
	amount := fs.Uint64("amount", 0, "base atoms to take (<= active)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	feeAsset := fs.String("fee-asset", "", "taker fee asset (any-asset fee market)")
	// STEP C: live taker seams. When -esplora + -taker-priv + -taker-blinding are
	// set, the taker builds and broadcasts a REAL on-chain lift via pkg/explorer +
	// pkg/trade; otherwise it runs the in-memory StubWallet demo.
	esploraURL := fs.String("esplora", "", "esplora API URL (enables a REAL on-chain lift)")
	netName := fs.String("net", "testnet", "network: testnet|mainnet")
	takerPriv := fs.String("taker-priv", "", "taker on-chain signing key (32-byte hex)")
	takerBlinding := fs.String("taker-blinding", "", "taker on-chain blinding key (32-byte hex)")
	takerKeyFile := fs.String("taker-key-file", "", "a file holding the taker's two keys (taker-key makes one): kept off the command line")
	confidential := fs.Bool("confidential", false, "build a confidential (blinded) taker half; default explicit")
	timeout := fs.Duration("timeout", 90*time.Second, "how long to await the maker co-sign")
	_ = fs.Parse(args)

	if *offerID == "" || *makerPub == "" {
		fatal("lift requires -offer-id and -maker-pubkey (see `seqob-cli book`)")
	}

	// Fetch the full offer so the taker can build the proposer SwapRequest.
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *base, *quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if o.GetOfferId() == *offerID && o.GetMakerPubkey() == *makerPub {
			target = o
			break
		}
	}
	if target == nil {
		fatal("offer %s by %s not found in %s/%s", *offerID, short(*makerPub), *base, *quote)
	}
	// SECURITY (unverified offers): the relay is untrusted, so verify the maker's
	// signature over the served offer before acting on it. This rejects forged or
	// tampered offers and makes target.MakerPubkey trustworthy for the E2E key.
	if err := offer.VerifyOffer(target); err != nil {
		fatal("offer %s failed maker signature verification (refusing to lift): %v", *offerID, err)
	}
	take := *amount
	if take == 0 {
		take = target.GetBaseAmount()
	}

	// Select the taker wallet: real (LiveWallet + RealBackend over esplora) or the
	// in-memory StubWallet demo.
	if *takerKeyFile != "" {
		if *takerPriv != "" || *takerBlinding != "" {
			fatal("-taker-key-file and -taker-priv/-taker-blinding are one or the other")
		}
		p, b, err := readTakerKeyFile(*takerKeyFile)
		if err != nil {
			fatal("-taker-key-file: %v", err)
		}
		*takerPriv, *takerBlinding = p, b
	}
	liftWallet := buildTakerWallet(*esploraURL, *netName, *takerPriv, *takerBlinding, *confidential)

	takerKey := loadOrGenKey(*priv) // ephemeral E2E session key (distinct from the on-chain key)

	// Open the lift session over WS so this connection is bound as the taker, then
	// courier the encrypted SwapRequest. The relay never sees the plaintext.
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()

	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            *offerID,
		MakerPubkey:        *makerPub,
		TakeAmount:         take,
		TakerFeeAsset:      *feeAsset,
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}})

	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sessionID := la.GetLiftAccepted().GetSessionId()
	fmt.Printf("lift session %s opened for offer %s\n", sessionID, *offerID)

	// SECURITY (relay MITM / CT leak): derive the E2E key from the maker pubkey in
	// the SIGNED, VERIFIED offer (the maker's offer key doubles as its session
	// key), NOT the relay's lift_accepted echo. A malicious relay could substitute
	// MakerSessionPubkey with its own key and decrypt the confidential swap; the
	// authentic value is already known from the offer, so the echo is only
	// cross-checked, never trusted.
	makerOfferPub, err := hex.DecodeString(target.GetMakerPubkey())
	if err != nil {
		fatal("decode maker pubkey from offer: %v", err)
	}
	pk, err := btcec.ParsePubKey(makerOfferPub)
	if err != nil {
		fatal("parse maker pubkey from offer: %v", err)
	}
	if echo := la.GetLiftAccepted().GetMakerSessionPubkey(); len(echo) > 0 && !bytes.Equal(echo, makerOfferPub) {
		fatal("relay MakerSessionPubkey echo does not match the signed offer's maker pubkey (possible MITM); aborting")
	}
	crypter, err := client.NewCrypter(takerKey, pk)
	if err != nil {
		fatal("crypter: %v", err)
	}

	taker := &client.Taker{Wallet: liftWallet}
	sealed, req, err := taker.Propose(target, take, *feeAsset, crypter)
	if err != nil {
		fatal("build request: %v", err)
	}
	fmt.Printf("proposer legs: pay %d %s, receive %d %s (taker is proposer)\n",
		req.GetAmountP(), req.GetAssetP(), req.GetAmountR(), req.GetAssetR())

	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: sealed}}})
	fmt.Printf("couriered encrypted SwapRequest (%d bytes) to session %s\n", len(sealed), sessionID)

	// Await the maker's sealed SwapAccept, then finalize: sign our inputs, validate,
	// and broadcast (real backend) or produce a demo txid (stub).
	fmt.Printf("awaiting maker co-sign (timeout %s)...\n", *timeout)
	from, err := readWSUntilSwap(conn, *timeout)
	if err != nil {
		fatal("no maker co-sign received: %v", err)
	}
	sealedComplete, txid, err := taker.Finalize(from.GetSwapMsg().GetCiphertext(), crypter)
	if err != nil {
		fatal("finalize/broadcast: %v", err)
	}
	// Courier the SwapComplete back so the maker learns the swap settled.
	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: sealedComplete}}})
	fmt.Printf("SWAP SETTLED: txid %s\n", txid)
}

// buildTakerWallet returns a real LiveWallet (over esplora) when the on-chain
// flags are provided, else the in-memory StubWallet demo.
func buildTakerWallet(esploraURL, netName, takerPriv, takerBlinding string, confidential bool) client.Wallet {
	if esploraURL == "" || takerPriv == "" || takerBlinding == "" {
		fmt.Println("(demo mode: in-memory StubWallet; pass -esplora -taker-priv -taker-blinding for a REAL lift)")
		return &client.StubWallet{Name: "taker"}
	}
	net := selectNet(netName)
	svc, err := esplora.NewService(esploraURL, 15000) // esplora request timeout is in MILLISECONDS (15s)
	if err != nil {
		fatal("esplora: %v", err)
	}
	rb := client.NewRealBackend(net, mustHex32(takerPriv, "taker-priv"), mustHex32(takerBlinding, "taker-blinding"))
	rb.FetchUtxos = svc.GetUnspents
	rb.BroadcastFn = svc.BroadcastTransaction
	rb.RejectedTxSink = func(txHex string) {
		path := filepath.Join(os.TempDir(), fmt.Sprintf("seqob-rejected-%d.hex", time.Now().UnixNano()))
		if err := os.WriteFile(path, []byte(txHex+"\n"), 0o600); err == nil {
			fmt.Printf("rejected tx hex saved to %s (decoderawtransaction it to see why)\n", path)
		}
	}
	fmt.Printf("LIVE taker (confidential=%v) — fund this address with the pay asset: %s\n", confidential, rb.TakerAddress())
	return &client.LiveWallet{Backend: rb, TakerInputsConfidential: confidential, TakerRecvConfidential: confidential}
}

func selectNet(name string) *network.Network {
	switch strings.ToLower(name) {
	case "mainnet":
		return &seqnet.SequentiaMainnet
	default:
		return &seqnet.SequentiaTestnet
	}
}

func mustHex32(s, label string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		fatal("%s must be 32-byte hex", label)
	}
	return b
}

// readWSUntilSwap reads From frames until a swap_msg arrives or the deadline hits.
func readWSUntilSwap(c *websocket.Conn, timeout time.Duration) (*seqobv1.From, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.SetReadDeadline(deadline)
		_, data, err := c.ReadMessage()
		if err != nil {
			return nil, err
		}
		var from seqobv1.From
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		if from.GetSwapMsg() != nil {
			return &from, nil
		}
		if e := from.GetError(); e != nil {
			return nil, fmt.Errorf("relay error %d: %s", e.GetCode(), e.GetMessage())
		}
	}
}

// startCourierReader spawns the SINGLE background reader for a courier WS session and
// returns a recv closure (the shape the RunTaker* drivers expect: ciphertext bytes for
// the next swap_msg, or an error). The on-demand readWSUntilSwap pattern only calls
// ReadMessage when the driver is actively waiting; during a multi-minute on-chain
// confirmation wait the driver is in its own poll loop and NEVER reads, so gorilla never
// auto-pongs the relay's keepalive pings and the relay reaps the idle socket at pongWait
// (60s) — the later announce write then hits a closed socket (broken pipe) and the lift
// aborts AFTER funding. A dedicated goroutine that stays in ReadMessage keeps gorilla
// auto-ponging for the whole wait, so the session survives to the announce. gorilla
// permits one concurrent reader + one concurrent writer, and this is the only reader
// (the send closure is the only writer), so it is race-free. Must be started AFTER the
// one-shot readWS(lift_accepted) handshake, so nothing else touches ReadMessage.
func startCourierReader(c *websocket.Conn) func(timeout time.Duration) ([]byte, error) {
	frames := make(chan *seqobv1.From, 16)
	errc := make(chan error, 1)
	go func() {
		c.SetReadDeadline(time.Time{}) // no idle deadline: block in ReadMessage (auto-ponging) until a frame or a real close
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				errc <- err
				close(frames)
				return
			}
			var from seqobv1.From
			if jsonUnmarshal.Unmarshal(data, &from) != nil {
				continue
			}
			frames <- &from
		}
	}()
	return func(timeout time.Duration) ([]byte, error) {
		deadline := time.After(timeout)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					select {
					case e := <-errc:
						return nil, e
					default:
						return nil, fmt.Errorf("courier connection closed")
					}
				}
				if f.GetSwapMsg() != nil {
					return f.GetSwapMsg().GetCiphertext(), nil
				}
				if e := f.GetError(); e != nil {
					return nil, fmt.Errorf("relay error %d: %s", e.GetCode(), e.GetMessage())
				}
				// a non-swap, non-error frame (out-of-band notice): keep waiting for the swap_msg
			case <-deadline:
				return nil, fmt.Errorf("courier recv timeout")
			case e := <-errc:
				return nil, e
			}
		}
	}
}

// --- helpers ---

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func loadOrGenKey(hexKey string) *btcec.PrivateKey {
	if hexKey == "" {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			fatal("gen key: %v", err)
		}
		// The pub identifies the session in logs; the priv is announced to NOBODY —
		// this line's tail used to ride a failed swap's error text all the way to
		// the user's wallet.
		fmt.Printf("generated key: pub=%s\n", hex.EncodeToString(k.PubKey().SerializeCompressed()))
		return k
	}
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		fatal("priv must be 32-byte hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

func postJSON(url string, in, out proto.Message) error {
	b, err := jsonMarshal.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return jsonUnmarshal.Unmarshal(body, out)
}

func getJSON(url string, out proto.Message) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return jsonUnmarshal.Unmarshal(body, out)
}

func writeWS(c *websocket.Conn, to *seqobv1.To) {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		fatal("marshal To: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		fatal("ws write: %v", err)
	}
}

func readWS(c *websocket.Conn) *seqobv1.From {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		fatal("ws read: %v", err)
	}
	var from seqobv1.From
	if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
		fatal("unmarshal From: %v", err)
	}
	return &from
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:6] + ".." + s[len(s)-6:]
}

// partialNote annotates a cross lift log line when the taker takes a slice of a
// larger resting offer (a partial fill), and is empty for a whole-offer lift.
func partialNote(take, whole uint64) string {
	if take < whole {
		return fmt.Sprintf(" (PARTIAL of %d)", whole)
	}
	return ""
}

func shortDir(d seqobv1.TradeDir) string {
	switch d {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		return "SELL"
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		return "BUY"
	default:
		return "?"
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	settleBTC()
	os.Exit(1)
}
