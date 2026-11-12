package main

// restricted_dvp.go is `-mode restricted` with an asset as the quote
// (-quote-asset): an online maker's quote for a restricted (OpenAMP) asset
// against another asset (offer member 29 with maker_recv_address). Each lift
// is one transaction moving both legs, checked by the asset's policy server
// before anyone signs and broadcast by it once both have
// (client.RunMakerRestrictedDvP). Whole fills, one lift at a time; after a
// fill the maker retires the offer and posts its next quote (restricted_serve.go).

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func buildRestrictedDvPOffer(cfg restrictedMakerConfig, assetB, recvAddr string, policyX [32]byte) *seqobv1.Offer {
	c := cfg.cross
	o := &seqobv1.Offer{
		OfferId:       orDefault(c.offerID, randstr.Hex(16)),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: c.asset, QuoteAsset: assetB},
		BaseAmount:    c.assetAmt,
		MinFill:       c.assetAmt,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(c.expiry).Unix()),
		Settlement: &seqobv1.Offer_Restricted{Restricted: &seqobv1.RestrictedTerms{
			PolicyServer: cfg.policyServer, PolicyX: policyX[:], MakerAid: cfg.aid, MakerRecvAddress: recvAddr,
		}},
	}
	if strings.ToLower(c.side) == "sell" {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = c.asset, c.assetAmt
		o.WantAsset, o.WantAmount = assetB, c.btcAmt
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = assetB, c.btcAmt
		o.WantAsset, o.WantAmount = c.asset, c.assetAmt
	}
	return o
}

func runRestrictedDvPMaker(cfg restrictedMakerConfig, assetB string) {
	c := cfg.cross
	side := strings.ToLower(c.side)
	if side != "sell" && side != "buy" {
		fatal("-mode restricted -side must be sell or buy")
	}
	if c.seqRPCURL == "" || cfg.policyServer == "" || cfg.aid == "" || cfg.enclaveKey == nil || cfg.potAsset == "" {
		fatal("-mode restricted with -quote-asset requires -xseq-rpc, -policy-server, -openamp-aid, -openamp-key and -pot-asset (the fee asset a held trade's respend pays in)")
	}
	if assetB == c.asset {
		fatal("-quote-asset is the restricted asset itself")
	}
	seqRPC, err := rpcFromURL(c.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	party := &restricted.Party{Node: &restricted.Node{Chain: xchain.NewChain(seqRPC, c.seqWallet)},
		Policy: restricted.NewPolicy(cfg.policyServer), AID: cfg.aid, Key: cfg.enclaveKey, Asset: c.asset}
	notPosted := func(f string, a ...interface{}) {
		time.Sleep(time.Minute)
		fatal("not posted: "+f, a...)
	}
	// quote checks the maker can fill a new offer now (the enclave holds what a sell quote
	// sells; the wallet holds what a buy quote pays) and returns it signed, with the address
	// its proceeds are paid to.
	var qmu sync.Mutex
	recvOf := map[string][]byte{}
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
		} else {
			cs, err := party.Node.WalletCoins(assetB, c.btcAmt)
			if err != nil {
				return nil, err
			}
			_ = party.Node.Unlock(cs)
		}
		recvAddr, recv, err := party.Node.NewAddress()
		if err != nil {
			return nil, err
		}
		qc := cfg
		qmu.Lock()
		if !first {
			qc.cross.offerID = "" // -offer-id names the first quote only
		}
		first = false
		qmu.Unlock()
		o := buildRestrictedDvPOffer(qc, assetB, recvAddr, policyX)
		if err := offer.SignOffer(o, c.makerKey); err != nil {
			return nil, fmt.Errorf("sign offer: %v", err)
		}
		qmu.Lock()
		recvOf[o.GetOfferId()] = recv
		qmu.Unlock()
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
	fmt.Printf("seqob-maker up (RESTRICTED, asset quote): posted %s offer %s: %d %s for %d %s, account %s at %s\n",
		side, o.GetOfferId(), c.assetAmt, c.asset, c.btcAmt, assetB, cfg.aid, cfg.policyServer)
	srv := &restrictedServer{relay: c.relay, wsURL: wsURL, key: c.makerKey, expiry: c.expiry, whole: c.assetAmt, ws: ws,
		quote: quote, exit: os.Exit, retryPause: time.Minute,
		lift: func(sid string, o *seqobv1.Offer, cr *client.Crypter, in chan []byte, send func([]byte) error) (string, bool, error) {
			qmu.Lock()
			recv := recvOf[o.GetOfferId()]
			qmu.Unlock()
			d := &client.RestrictedDvP{Party: party, Crypter: cr, Value: c.assetAmt, Price: c.btcAmt, AssetB: assetB,
				FeeAsset: cfg.potAsset, RfaPerVb: cfg.rfaPerVb,
				Log: func(f string, a ...interface{}) { fmt.Printf("lift "+sid+": "+f+"\n", a...) }}
			txid, err := client.RunMakerRestrictedDvP(d, side == "sell", recv, in, send)
			return txid, err == nil, err
		}}
	srv.serve(o)
}
