package main

// restricted.go is `-mode restricted`: an online maker's quote for a
// restricted (OpenAMP) asset against on-chain bitcoin (offer member 29,
// RestrictedTerms). The maker holds the asset at its own enclave at the
// asset's policy server, and bitcoin in a bitcoind wallet; the Sequentia node
// wallet pays the fee pot and the fees in -pot-asset. Each lift runs
// client.RunMakerRestrictedSell (side sell: the taker pays BTC) or
// RunMakerRestrictedBuy (side buy: the maker pays BTC), one at a time, whole
// fills only; the state of every lift is written under -xstate-dir before each
// step it depends on. After a fill the maker posts its next quote itself
// (restricted_serve.go).

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type restrictedMakerConfig struct {
	cross        crossMakerConfig
	policyServer string
	aid          string
	enclaveKey   *btcec.PrivateKey
	potAsset     string
	potAmount    uint64
	rfaPerVb     uint64
}

func buildRestrictedOffer(cfg restrictedMakerConfig, policyX [32]byte) *seqobv1.Offer {
	c := cfg.cross
	o := &seqobv1.Offer{
		OfferId:       orDefault(c.offerID, randstr.Hex(16)),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: c.asset, QuoteAsset: offer.BTCSentinel},
		BaseAmount:    c.assetAmt,
		MinFill:       c.assetAmt, // whole fills only
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(c.expiry).Unix()),
		FeeAssetHint:  cfg.potAsset,
		Settlement: &seqobv1.Offer_Restricted{Restricted: &seqobv1.RestrictedTerms{
			PolicyServer: cfg.policyServer, PolicyX: policyX[:], MakerAid: cfg.aid,
			PotAsset: cfg.potAsset, PotAmount: cfg.potAmount,
		}},
	}
	if strings.ToLower(c.side) == "sell" {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = c.asset, c.assetAmt
		o.WantAsset, o.WantAmount = offer.BTCSentinel, c.btcAmt
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = offer.BTCSentinel, c.btcAmt
		o.WantAsset, o.WantAmount = c.asset, c.assetAmt
	}
	return o
}

// writeRestrictedState persists a lift's state; the BTC key and the secret in
// it are the recovery material.
func writeRestrictedState(dir, sid string, r *client.RestrictedResult) {
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		fmt.Printf("lift %s: state: %v\n", sid, err)
		return
	}
	p := filepath.Join(dir, "restricted-"+sid+".json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

func runRestrictedMaker(cfg restrictedMakerConfig) {
	c := cfg.cross
	side := strings.ToLower(c.side)
	if side != "sell" && side != "buy" {
		fatal("-mode restricted -side must be sell (taker pays BTC) or buy (the maker pays BTC)")
	}
	if c.btcRPCURL == "" || c.seqRPCURL == "" || cfg.policyServer == "" || cfg.aid == "" || cfg.enclaveKey == nil ||
		cfg.potAsset == "" || cfg.potAmount == 0 {
		fatal("-mode restricted requires -btc-rpc, -xseq-rpc, -policy-server, -openamp-aid, -openamp-key, -pot-asset and -pot-amount")
	}
	btcRPC, err := rpcFromURL(c.btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	seqRPC, err := rpcFromURL(c.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(c.btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, c.btcWallet, params)
	btcChain.SetFeeRate(c.btcFeeRate)
	seqChain := xchain.NewChain(seqRPC, c.seqWallet)
	party := &restricted.Party{Node: &restricted.Node{Chain: seqChain}, Policy: restricted.NewPolicy(cfg.policyServer),
		AID: cfg.aid, Key: cfg.enclaveKey, Asset: c.asset}

	// Posted only when it can be filled: the enclave is this key's, and holds
	// the asset (sell); the wallets hold the pot and its fees, and the BTC (buy).
	notPosted := func(f string, a ...interface{}) {
		time.Sleep(time.Minute) // a supervisor's retry loop must not spin
		fatal("not posted: "+f, a...)
	}
	if err := btcChain.EnsureWalletLoaded(); err != nil {
		notPosted("%v", err)
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		fatal("create -xstate-dir %s: %v", c.stateDir, err)
	}
	var qmu sync.Mutex
	first := true
	quote := func() (*seqobv1.Offer, error) {
		asset, err := party.Policy.Asset(c.asset)
		if err != nil {
			return nil, fmt.Errorf("the policy server does not list %s: %v", c.asset, err)
		}
		policyX, _, err := asset.Keys()
		if err != nil {
			return nil, err
		}
		enclave, _, _, err := party.Enclave()
		if err != nil {
			return nil, err
		}
		if side == "sell" {
			coins, err := party.Node.EnclaveCoins(enclave, c.asset)
			if err != nil {
				return nil, fmt.Errorf("the enclave does not read: %v", err)
			}
			var have uint64
			for _, x := range coins {
				have += x.Value
			}
			if have < c.assetAmt {
				return nil, fmt.Errorf("the enclave holds %d of the %d this offer sells", have, c.assetAmt)
			}
		} else if have, err := btcChain.SpendableSats(); err != nil || have < c.btcAmt+20000 {
			return nil, fmt.Errorf("the bitcoin wallet %s can spend %d sats (%v), and this offer gives %d and its lock's fee", c.btcWallet, have, err, c.btcAmt)
		}
		if _, err := party.Node.FeeAtoms(cfg.potAsset, 1, cfg.rfaPerVb); err != nil {
			return nil, err
		}
		qc := cfg
		qmu.Lock()
		if !first {
			qc.cross.offerID = "" // -offer-id names the first quote only
		}
		first = false
		qmu.Unlock()
		o := buildRestrictedOffer(qc, policyX)
		if err := offer.SignOffer(o, c.makerKey); err != nil {
			return nil, fmt.Errorf("sign offer: %v", err)
		}
		return o, nil
	}
	o, err := quote()
	if err != nil {
		notPosted("%v", err)
	}
	wsURL := "ws" + strings.TrimPrefix(c.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	fmt.Printf("seqob-maker up (RESTRICTED): posted %s offer %s: %d %s for %d BTC sats, account %s at %s\n",
		side, o.GetOfferId(), c.assetAmt, c.asset, c.btcAmt, cfg.aid, cfg.policyServer)

	btcMTP := func() (int64, error) {
		var i struct {
			MedianTime int64 `json:"mediantime"`
		}
		return i.MedianTime, btcRPC.Call(&i, "getblockchaininfo")
	}
	newOps := func(h []byte) (client.XcOps, error) {
		return &client.LiveXcOps{Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(h)), BTC: btcChain, SEQ: seqChain}, nil
	}
	srv := &restrictedServer{relay: c.relay, wsURL: wsURL, key: c.makerKey, expiry: c.expiry, whole: c.assetAmt, ws: ws,
		quote: quote, exit: os.Exit, retryPause: time.Minute,
		lift: func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (string, bool, error) {
			r := &client.RestrictedSwap{Party: party, Crypter: cr, BtcMedianTime: btcMTP,
				Value: c.assetAmt, BtcAmount: c.btcAmt, PotAsset: cfg.potAsset, PotAmount: cfg.potAmount,
				RfaPerVb: cfg.rfaPerVb, BtcFee: c.spendFee, MinBTCConf: c.minBTCConf,
				Log:      func(f string, a ...interface{}) { fmt.Printf("lift "+sid+": "+f+"\n", a...) },
				OnUpdate: func(res *client.RestrictedResult) { writeRestrictedState(c.stateDir, sid, res) }}
			var res *client.RestrictedResult
			var err error
			if side == "sell" {
				res, err = client.RunMakerRestrictedSell(r, newOps, in, send)
			} else {
				res, err = client.RunMakerRestrictedBuy(r, newOps, in, send)
			}
			if res != nil {
				writeRestrictedState(c.stateDir, sid, res)
			}
			if err != nil || res == nil || !res.Settled {
				return "", false, err
			}
			txid := res.BtcClaimTxid
			if txid == "" {
				txid = res.SeqClaimTxid
			}
			return txid, true, nil
		}}
	srv.serve(o)
}

func hexKey(h string) *btcec.PrivateKey {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		fatal("-openamp-key must be 32 bytes of hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}
