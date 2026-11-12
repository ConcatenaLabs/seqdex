package main

// rlift lifts a restricted asset's quote (offer member 29, RestrictedTerms):
// against an asset, one transaction moving both legs through the asset's
// policy server; against bitcoin, a swap whose Sequentia side is the policy
// server's swap HTLC and fee pot. The taker's account at the policy server
// and its enclave key are its own (-aid, SEQOB_OPENAMP_KEY).

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdRLift(args []string) {
	fs := newFlagSet("rlift")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "the restricted asset (display hex, required)")
	quote := fs.String("quote", offer.BTCSentinel, "the quote: an asset id (display hex) or "+offer.BTCSentinel)
	side := fs.String("side", "buy", "buy: the taker buys the restricted asset; sell: it sells it")
	offerID := fs.String("offer-id", "", "offer to lift (empty: the first restricted offer of that side)")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet paying the fee and the other asset (a descriptor wallet)")
	aid := fs.String("aid", "", "the taker's account at the asset's policy server (required)")
	key := envString(fs, "key", "SEQOB_OPENAMP_KEY", "the account's enclave key, 32-byte hex (required)")
	feeAsset := fs.String("fee-asset", "", "asset quote: the asset the taker pays the fee in (display hex)")
	rfaPerVb := fs.Uint64("rfa-per-vb", 2, "the Sequentia fee rate in reference atoms per vbyte")
	btcRPCURL := envString(fs, "btc-rpc", "SEQOB_BTC_RPC", "bitcoin quote: bitcoind RPC URL http://user:pass@host:port")
	btcWallet := fs.String("btc-wallet", "", "bitcoin quote: bitcoind wallet")
	btcChainName := fs.String("btc-chain", "testnet4", "bitcoin quote: testnet4 | regtest")
	stateFile := fs.String("state-file", "rlift-session.json", "bitcoin quote: the swap's state (its refund path)")
	_ = fs.Parse(args)
	if *asset == "" || *seqRPCURL == "" || *aid == "" || *key == "" {
		fatal("rlift requires -asset, -seq-rpc, -aid and -key")
	}
	buy := strings.ToLower(*side) == "buy"
	want := seqobv1.TradeDir_TRADE_DIR_SELL // the taker buys what the maker sells
	if !buy {
		want = seqobv1.TradeDir_TRADE_DIR_BUY
	}
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, *quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if o.GetRestricted() == nil || o.GetTradeDir() != want || (*offerID != "" && o.GetOfferId() != *offerID) {
			continue
		}
		if err := offer.VerifyOffer(o); err != nil {
			continue
		}
		target = o
		break
	}
	if target == nil {
		fatal("no restricted %s offer for %s/%s", strings.ToLower(*side), *asset, *quote)
	}
	rt := target.GetRestricted()
	value := target.GetBaseAmount()
	price := target.GetWantAmount()
	if !buy {
		price = target.GetOfferAmount()
	}
	kb, err := hex.DecodeString(*key)
	if err != nil || len(kb) != 32 {
		fatal("-key must be 32 bytes of hex")
	}
	ek, _ := btcec.PrivKeyFromBytes(kb)
	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	seqChain := xchain.NewChain(seqRPC, *seqWallet)
	party := &restricted.Party{Node: &restricted.Node{Chain: seqChain}, Policy: restricted.NewPolicy(rt.GetPolicyServer()),
		AID: *aid, Key: ek, Asset: *asset}
	a, err := party.Policy.Asset(*asset)
	if err != nil {
		fatal("the offer's policy server does not list %s: %v", *asset, err)
	}
	if px, _, err := a.Keys(); err != nil || !bytes.Equal(px[:], rt.GetPolicyX()) {
		fatal("the offer's policy key is not the one its policy server lists for the asset")
	}

	takerKey := loadOrGenKey("")
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()
	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{OfferId: target.GetOfferId(),
		MakerPubkey: target.GetMakerPubkey(), TakeAmount: value, TakerSessionPubkey: takerKey.PubKey().SerializeCompressed()}}})
	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sid := la.GetLiftAccepted().GetSessionId()
	mp, _ := hex.DecodeString(target.GetMakerPubkey())
	pk, err := btcec.ParsePubKey(mp)
	if err != nil {
		fatal("maker pubkey: %v", err)
	}
	cr, err := client.NewCrypter(takerKey, pk)
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
	recv := startCourierReader(conn)
	logf := func(f string, a ...interface{}) { fmt.Printf("  "+f+"\n", a...) }

	if !offer.IsBTCSentinel(*quote) {
		if *feeAsset == "" {
			fatal("an asset quote needs -fee-asset")
		}
		var makerRecv []byte
		if buy {
			if makerRecv, err = party.Node.ScriptOf(rt.GetMakerRecvAddress()); err != nil {
				fatal("the offer's maker_recv_address: %v", err)
			}
		}
		d := &client.RestrictedDvP{Party: party, Crypter: cr, Value: value, Price: price, AssetB: *quote, FeeAsset: *feeAsset,
			RfaPerVb: *rfaPerVb, Log: logf}
		txid, err := client.RunTakerRestrictedDvP(d, rt.GetMakerAid(), buy, makerRecv, send, recv)
		if err != nil {
			fatal("trade: %v", err)
		}
		fmt.Printf("traded %d %s for %d %s: %s\n", value, *asset, price, *quote, txid)
		return
	}

	if *btcRPCURL == "" || *btcWallet == "" {
		fatal("a bitcoin quote needs -btc-rpc and -btc-wallet")
	}
	if _, err := os.Stat(*stateFile); err == nil {
		fatal("state file %s exists: it may hold a swap still owed; pass another -state-file", *stateFile)
	}
	btcRPC, err := xliftRPCFromURL(*btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(*btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
	r := &client.RestrictedSwap{Party: party, Crypter: cr, Value: value, BtcAmount: price, PotAsset: rt.GetPotAsset(),
		PotAmount: rt.GetPotAmount(), RfaPerVb: *rfaPerVb, MinBTCConf: 1, Log: logf,
		BtcMedianTime: func() (int64, error) {
			var i struct {
				MedianTime int64 `json:"mediantime"`
			}
			return i.MedianTime, btcRPC.Call(&i, "getblockchaininfo")
		},
		OnUpdate: func(res *client.RestrictedResult) {
			b, _ := json.MarshalIndent(res, "", " ")
			_ = os.WriteFile(*stateFile, b, 0o600)
		}}
	var res *client.RestrictedResult
	if buy {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			fatal("secret: %v", err)
		}
		refund, err := xchain.NewKey()
		if err != nil {
			fatal("key: %v", err)
		}
		r.Ops = &client.LiveXcOps{Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLock(secret)), BTC: btcChain, SEQ: seqChain}
		res, err = client.RunTakerRestrictedBuy(r, rt.GetMakerAid(), secret, refund, send, recv)
	} else {
		r.Ops = &client.LiveXcOps{Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(make([]byte, 32))), BTC: btcChain, SEQ: seqChain}
		rebind := func(timeout time.Duration) ([]byte, error) {
			b, err := recv(timeout)
			if err == nil {
				if m, oerr := client.OpenXcMsg(b, cr); oerr == nil && m.Type == client.XcBtcLegLocked {
					if h, herr := hex.DecodeString(m.HashH); herr == nil {
						r.Ops = &client.LiveXcOps{Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(h)), BTC: btcChain, SEQ: seqChain}
					}
				}
			}
			return b, err
		}
		res, err = client.RunTakerRestrictedSell(r, rt.GetMakerAid(), send, rebind)
	}
	if res != nil {
		b, _ := json.MarshalIndent(res, "", " ")
		_ = os.WriteFile(*stateFile, b, 0o600)
	}
	if err != nil {
		fatal("swap: %v (state in %s)", err, *stateFile)
	}
	fmt.Printf("swapped %d %s for %d sats (state in %s)\n", value, *asset, price, *stateFile)
}
