package main

// restricted_dvp.go is `-mode restricted` with an asset as the quote
// (-quote-asset): an online maker's quote for a restricted (OpenAMP) asset
// against another asset (offer member 29 with maker_recv_address). Each lift
// is one transaction moving both legs, checked by the asset's policy server
// before anyone signs and broadcast by it once both have
// (client.RunMakerRestrictedDvP). Whole fills, one lift at a time; after a
// fill the maker retires the offer and exits for its supervisor to quote again.

import (
	"fmt"
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
	asset, err := party.Policy.Asset(c.asset)
	if err != nil {
		notPosted("the policy server does not list %s: %v", c.asset, err)
	}
	policyX, _, err := asset.Keys()
	if err != nil {
		notPosted("%v", err)
	}
	enclave, _, _, err := party.Enclave()
	if err != nil {
		notPosted("%v", err)
	}
	if side == "sell" {
		coins, err := party.Node.EnclaveCoins(enclave, c.asset)
		if err != nil {
			notPosted("the enclave does not read: %v", err)
		}
		var have uint64
		for _, x := range coins {
			have += x.Value
		}
		if have < c.assetAmt {
			notPosted("the enclave holds %d of the %d this offer sells", have, c.assetAmt)
		}
	} else {
		cs, err := party.Node.WalletCoins(assetB, c.btcAmt)
		if err != nil {
			notPosted("%v", err)
		}
		_ = party.Node.Unlock(cs)
	}
	recvAddr, recv, err := party.Node.NewAddress()
	if err != nil {
		notPosted("%v", err)
	}
	o := buildRestrictedDvPOffer(cfg, assetB, recvAddr, policyX)
	if err := offer.SignOffer(o, c.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}
	wsURL := "ws" + strings.TrimPrefix(c.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	fmt.Printf("seqob-maker up (RESTRICTED, asset quote): posted %s offer %s: %d %s for %d %s, account %s at %s\n",
		side, o.GetOfferId(), c.assetAmt, c.asset, c.btcAmt, assetB, cfg.aid, cfg.policyServer)

	var mu sync.Mutex
	inboxes := map[string]chan []byte{}
	inFlight, filled := 0, false
	setBusyFn(func() int { mu.Lock(); defer mu.Unlock(); return inFlight })
	armRequoteExit(o, func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 })
	refuse := func(sid string, cr *client.Crypter, code, msg string) {
		m := &client.XcMsg{Type: client.XcFail, Code: code, Message: msg}
		if sealed, err := m.Seal(cr); err == nil {
			_ = ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		}
	}
	for {
		_, data, err := ws.current().ReadMessage()
		if err != nil {
			mu.Lock()
			done := filled && inFlight == 0
			mu.Unlock()
			if done {
				fmt.Println("offer filled and no lift in flight; exiting (restart to quote again)")
				return
			}
			var resubmit *seqobv1.Offer
			if !filled {
				resubmit = o
				_ = refreshOfferForRequote(o, c.expiry, c.makerKey)
			}
			fmt.Printf("ws read error: %v; reconnecting\n", err)
			ws.redialLoop(wsURL, resubmit)
			reattachSessions(ws, c.makerKey, &mu, inboxes)
			continue
		}
		var from seqobv1.From
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			sid := lr.GetSessionId()
			cr, err := client.NewMakerCrypterFromLift(c.makerKey, lr.GetTakerSessionPubkey())
			if err != nil {
				continue
			}
			if lr.GetOfferId() != o.GetOfferId() {
				refuse(sid, cr, "stale_offer", "this maker process serves offer "+o.GetOfferId()+", not "+lr.GetOfferId())
				continue
			}
			if lr.GetTakeAmount() != 0 && lr.GetTakeAmount() != c.assetAmt {
				refuse(sid, cr, "whole_fill_only", fmt.Sprintf("this offer fills whole: %d", c.assetAmt))
				continue
			}
			if refuseIfDraining(sid, cr, refuse) {
				continue
			}
			mu.Lock()
			if inFlight > 0 || filled {
				mu.Unlock()
				refuse(sid, cr, "busy", "a lift of this offer is in flight")
				continue
			}
			inFlight++
			in := make(chan []byte, 16)
			inboxes[sid] = in
			mu.Unlock()
			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			go func(sid string, in chan []byte) {
				d := &client.RestrictedDvP{Party: party, Crypter: cr, Value: c.assetAmt, Price: c.btcAmt, AssetB: assetB,
					FeeAsset: cfg.potAsset, RfaPerVb: cfg.rfaPerVb,
					Log: func(f string, a ...interface{}) { fmt.Printf("lift "+sid+": "+f+"\n", a...) }}
				txid, err := client.RunMakerRestrictedDvP(d, side == "sell", recv, in, send)
				mu.Lock()
				inFlight--
				delete(inboxes, sid)
				if err == nil {
					filled = true
				}
				mu.Unlock()
				if err != nil {
					fmt.Printf("lift %s ended: %v (the offer stays)\n", sid, err)
					return
				}
				reportSettledTrade(ws, o, c.makerKey, c.assetAmt, txid)
				fmt.Printf("lift %s traded in %s; retiring the offer\n", sid, txid)
				cancelOffer(c.relay, o, c.makerKey)
			}(sid, in)
		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in != nil {
				select {
				case in <- sm.GetCiphertext():
				default:
				}
			}
		case from.GetError() != nil:
			releaseDetachedSession(from.GetError(), &mu, inboxes)
		}
	}
}
