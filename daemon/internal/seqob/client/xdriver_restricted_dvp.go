package client

// A restricted asset for another asset over the relay courier: offer member
// 29 (RestrictedTerms) whose quote is an asset. One transaction moves both
// legs (delivery against payment); the asset's policy server checks it before
// anyone signs and broadcasts it once both have, so its signature never
// exists apart from a broadcast (pkg/restricted/dvp.go).
//
//	taker -> maker  dvp_request  its account, the amount
//	maker -> taker  dvp_coins    the coins the maker puts in, and its change script
//	taker -> maker  dvp_tx       the trade, built by the taker and checked at the policy server
//	maker -> taker  dvp_signed   the seller of the asset: its input and expiry signatures;
//	                             the payer: the trade with its own inputs signed
//	taker -> maker  dvp_done     the txid the policy server broadcast
//
// Each side checks the trade against what it agreed before signing: its coins
// spent only as offered (none other of its wallet's), what it gives bounded,
// what it gets paid. The seller's signatures carry an expiry the server
// enforces. The payer's do not, so a payer whose trade is not broadcast in
// time spends its coin elsewhere, and the signed trade can never land.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

const (
	XcDvpRequest XcMsgType = "dvp_request"
	XcDvpCoins   XcMsgType = "dvp_coins"
	XcDvpTx      XcMsgType = "dvp_tx"
	XcDvpSigned  XcMsgType = "dvp_signed"
	XcDvpDone    XcMsgType = "dvp_done"
)

// RestrictedDvP is what either side of a delivery against payment needs.
type RestrictedDvP struct {
	Party    *restricted.Party // this side's account, key, node and wallet
	Crypter  *Crypter
	Value    uint64 // the restricted asset's atoms
	Price    uint64 // AssetB's atoms paid for Value
	AssetB   string // display hex
	FeeAsset string // display hex: the taker pays the fee in it
	RfaPerVb uint64
	// Expiry is how long the seller's signatures stay usable (default 5 min).
	Expiry time.Duration
	// Wait is how long the maker waits for the trade to be mined.
	Wait   time.Duration
	Timing XcTiming
	Log    func(format string, args ...interface{})
	// BeforeBroadcast, when set, runs on the taker just before it asks the
	// policy server to broadcast (tests hold a trade there).
	BeforeBroadcast func()
}

func (d *RestrictedDvP) logf(f string, a ...interface{}) {
	if d.Log != nil {
		d.Log(f, a...)
	}
}

func (d *RestrictedDvP) defaults() error {
	d.Timing.setDefaults()
	if d.Expiry == 0 {
		d.Expiry = 5 * time.Minute
	}
	if d.Wait == 0 {
		d.Wait = d.Expiry + 10*time.Minute
	}
	if d.Party == nil || d.Crypter == nil || d.Value == 0 || d.Price == 0 || d.AssetB == "" || d.RfaPerVb == 0 {
		return errors.New("restricted trade: incomplete params")
	}
	if d.AssetB == d.Party.Asset {
		return errors.New("restricted trade: the price is in the restricted asset itself")
	}
	return nil
}

// coinsAt reads the outpoints as unspent explicit coins.
func coinsAt(n *restricted.Node, ops []restricted.Outpoint) ([]restricted.Coin, error) {
	var cs []restricted.Coin
	for _, o := range ops {
		c, err := n.CoinAt(o.Txid, o.Vout)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, fmt.Errorf("%s:%d is not an unspent explicit coin", o.Txid, o.Vout)
		}
		cs = append(cs, *c)
	}
	return cs, nil
}

func outpoints(cs []restricted.Coin) []restricted.Outpoint {
	var ops []restricted.Outpoint
	for _, c := range cs {
		ops = append(ops, restricted.Outpoint{Txid: c.Txid, Vout: c.Vout})
	}
	return ops
}

// unsignedFrom reads a trade's hex with the coins its inputs spend.
func unsignedFrom(n *restricted.Node, raw string) (*restricted.Unsigned, error) {
	u, err := restricted.ParseTx(raw)
	if err != nil {
		return nil, err
	}
	var ops []restricted.Outpoint
	for _, in := range u.Tx.Inputs {
		ops = append(ops, restricted.InputOutpoint(in))
	}
	if u.Coins, err = coinsAt(n, ops); err != nil {
		return nil, err
	}
	return u, nil
}

// inputsAt are the indices of the inputs spending coins at spk.
func inputsAt(u *restricted.Unsigned, spk []byte) []int {
	var ins []int
	for i, c := range u.Coins {
		if bytes.Equal(c.Script, spk) {
			ins = append(ins, i)
		}
	}
	return ins
}

// RunMakerRestrictedDvP serves one lift of a maker's asset quote. sell: the
// maker sells the restricted asset, paid to recv; otherwise it buys it, paying
// from its wallet. It returns the trade's txid once mined.
func RunMakerRestrictedDvP(d *RestrictedDvP, sell bool, recv []byte, in <-chan []byte, send XcSend) (string, error) {
	if err := d.defaults(); err != nil {
		return "", err
	}
	rcv := chanRecv(in)
	p := d.Party
	req, err := recvXcType(rcv, d.Crypter, XcDvpRequest, d.Timing.TermsReqWait)
	if err != nil {
		return "", err
	}
	if req.SeqAmount != d.Value {
		sendXcFail(d.Crypter, send, "whole_fill_only", fmt.Sprintf("this quote trades %d", d.Value))
		return "", fmt.Errorf("%w: amount %d", ErrXcBadTerms, req.SeqAmount)
	}
	takerEnclave, err := p.Policy.EnclaveScript(req.TakerAID, p.Asset)
	if err != nil {
		sendXcFail(d.Crypter, send, "not_eligible", "your account is not known to the asset's policy server")
		return "", err
	}
	myEnclave, _, _, err := p.Enclave()
	if err != nil {
		return "", err
	}
	seller, buyer := p.AID, req.TakerAID
	if !sell {
		seller, buyer = req.TakerAID, p.AID
	}
	if e, err := p.Policy.Eligible(p.Asset, buyer, seller, d.Value); err != nil || !e.Eligible {
		sendXcFail(d.Crypter, send, "not_eligible", "the asset's policy server does not admit this trade now")
		return "", fmt.Errorf("not eligible: %v %+v", err, e)
	}
	var mine []restricted.Coin
	var change []byte
	if sell {
		cs, err := p.Node.EnclaveCoins(myEnclave, p.Asset)
		if err != nil {
			return "", err
		}
		var sum uint64
		for _, c := range cs {
			if sum >= d.Value {
				break
			}
			mine = append(mine, c)
			sum += c.Value
		}
		if sum < d.Value {
			sendXcFail(d.Crypter, send, "unavailable", "the maker no longer holds this amount")
			return "", fmt.Errorf("the enclave holds %d of %d", sum, d.Value)
		}
	} else {
		if mine, err = p.Node.WalletCoins(d.AssetB, d.Price); err != nil {
			sendXcFail(d.Crypter, send, "unavailable", "the maker cannot pay now")
			return "", err
		}
		defer func() { _ = p.Node.Unlock(mine) }()
		if change, err = p.Node.NewScript(); err != nil {
			return "", err
		}
	}
	if err := sendXc(&XcMsg{Type: XcDvpCoins, DvpCoins: outpoints(mine), DvpChange: hex.EncodeToString(change)}, d.Crypter, send); err != nil {
		return "", err
	}
	m, err := recvXcType(rcv, d.Crypter, XcDvpTx, d.Timing.TermsReqWait)
	if err != nil {
		return "", err
	}
	u, err := unsignedFrom(p.Node, m.DvpTx)
	if err != nil {
		sendXcFail(d.Crypter, send, "bad_tx", err.Error())
		return "", err
	}
	owns, err := p.Node.WalletOutpoints()
	if err != nil {
		return "", err
	}
	a, _ := restricted.ReverseHex(p.Asset)
	b, _ := restricted.ReverseHex(d.AssetB)
	if sell {
		err = restricted.CheckDvP(u, mine, owns, [][]byte{myEnclave, recv}, map[[32]byte]uint64{a: d.Value},
			[]restricted.Out{{Asset: b, Value: d.Price, Script: recv}})
	} else {
		err = restricted.CheckDvP(u, mine, owns, [][]byte{myEnclave, change}, map[[32]byte]uint64{b: d.Price},
			[]restricted.Out{{Asset: a, Value: d.Value, Script: myEnclave}})
	}
	if err != nil {
		sendXcFail(d.Crypter, send, "bad_tx", err.Error())
		return "", err
	}
	sellerEnclave := myEnclave
	if !sell {
		sellerEnclave = takerEnclave
	}
	sellerIns := inputsAt(u, sellerEnclave)
	if err := p.Policy.CheckDvPWithServer(u, p.Asset, seller, sellerIns); err != nil {
		sendXcFail(d.Crypter, send, "refused", err.Error())
		return "", err
	}
	txid := u.Txid()
	if sell {
		exp := p.Node.Now().Add(d.Expiry).Unix()
		sigs, es, err := p.SellerSigs(u, sellerIns, exp)
		if err != nil {
			return "", err
		}
		if err := sendXc(&XcMsg{Type: XcDvpSigned, UserSigs: sigs, ExpiresAt: exp, ExpirySig: es}, d.Crypter, send); err != nil {
			return "", err
		}
	} else {
		if err := p.Node.SignWallet(u); err != nil {
			return "", err
		}
		raw, _ := u.Hex()
		if err := sendXc(&XcMsg{Type: XcDvpSigned, DvpTx: raw}, d.Crypter, send); err != nil {
			return "", err
		}
	}
	d.logf("signed the trade %s; waiting for it to be mined", txid)
	deadline := time.Now().Add(d.Wait)
	for time.Now().Before(deadline) {
		if bh, err := p.Node.Chain.BlockHashOfTx(txid); err == nil && bh != "" {
			return txid, nil
		}
		time.Sleep(d.Timing.Poll)
	}
	if sell {
		// The server refuses the trade from the expiry on.
		return "", fmt.Errorf("the trade %s was not mined; its signatures expired", txid)
	}
	// The payer's signatures do not expire: spend a coin of the trade
	// elsewhere, so the signed trade can never land.
	rt, err := p.Node.Respend(mine[0], d.FeeAsset, d.RfaPerVb)
	if err != nil {
		return "", fmt.Errorf("the trade %s was not mined, and respending its coin failed: %v", txid, err)
	}
	return "", fmt.Errorf("the trade %s was not mined; its coin was respent in %s", txid, rt)
}

// RunTakerRestrictedDvP lifts a maker's asset quote. makerSells: the taker
// buys the restricted asset, paying to makerRecv; otherwise it sells it. It
// returns the txid the policy server broadcast.
func RunTakerRestrictedDvP(d *RestrictedDvP, makerAID string, makerSells bool, makerRecv []byte, send XcSend, recv XcRecv) (string, error) {
	if err := d.defaults(); err != nil {
		return "", err
	}
	p := d.Party
	if d.FeeAsset == "" || d.FeeAsset == p.Asset {
		return "", errors.New("restricted trade: the taker's fee asset is required, and never the restricted asset")
	}
	myEnclave, _, _, err := p.Enclave()
	if err != nil {
		return "", err
	}
	makerEnclave, err := p.Policy.EnclaveScript(makerAID, p.Asset)
	if err != nil {
		return "", err
	}
	if err := sendXc(&XcMsg{Type: XcDvpRequest, TakerAID: p.AID, SeqAmount: d.Value}, d.Crypter, send); err != nil {
		return "", err
	}
	m, err := recvXcType(recv, d.Crypter, XcDvpCoins, d.Timing.TermsReqWait)
	if err != nil {
		return "", err
	}
	theirs, err := coinsAt(p.Node, m.DvpCoins)
	if err != nil {
		return "", err
	}
	a, _ := restricted.ReverseHex(p.Asset)
	b, err := restricted.ReverseHex(d.AssetB)
	if err != nil {
		return "", err
	}
	fa, err := restricted.ReverseHex(d.FeeAsset)
	if err != nil {
		return "", err
	}
	fee, err := p.Node.FeeAtoms(d.FeeAsset, 1200, d.RfaPerVb)
	if err != nil {
		return "", err
	}
	var locked []restricted.Coin
	defer func() { _ = p.Node.Unlock(locked) }()
	feeCoins, err := p.Node.WalletCoins(d.FeeAsset, fee)
	if err != nil {
		return "", err
	}
	locked = append(locked, feeCoins...)
	feeChange, err := p.Node.NewScript()
	if err != nil {
		return "", err
	}
	plan := restricted.DvP{Asset: a, Value: d.Value, AssetB: b, Price: d.Price, FeeCoins: feeCoins, FeeAsset: fa, Fee: fee, FeeChange: feeChange}
	if makerSells {
		for _, c := range theirs {
			if c.Asset != a || !bytes.Equal(c.Script, makerEnclave) {
				return "", fmt.Errorf("%w: the maker's coins are not its enclave's", ErrXcBadTerms)
			}
		}
		pay, err := p.Node.WalletCoins(d.AssetB, d.Price)
		if err != nil {
			return "", err
		}
		locked = append(locked, pay...)
		ch, err := p.Node.NewScript()
		if err != nil {
			return "", err
		}
		plan.SellerCoins, plan.SellerEnclave, plan.SellerRecv = theirs, makerEnclave, makerRecv
		plan.BuyerCoins, plan.BuyerEnclave, plan.BuyerChange = pay, myEnclave, ch
	} else {
		for _, c := range theirs {
			if c.Asset != b {
				return "", fmt.Errorf("%w: the maker's coins are not the price's asset", ErrXcBadTerms)
			}
		}
		mc, err := hex.DecodeString(m.DvpChange)
		if err != nil || len(mc) == 0 {
			return "", fmt.Errorf("%w: the maker's change script", ErrXcBadTerms)
		}
		cs, err := p.Node.EnclaveCoins(myEnclave, p.Asset)
		if err != nil {
			return "", err
		}
		var sell []restricted.Coin
		var sum uint64
		for _, c := range cs {
			if sum >= d.Value {
				break
			}
			sell = append(sell, c)
			sum += c.Value
		}
		recvB, err := p.Node.NewScript()
		if err != nil {
			return "", err
		}
		plan.SellerCoins, plan.SellerEnclave, plan.SellerRecv = sell, myEnclave, recvB
		plan.BuyerCoins, plan.BuyerEnclave, plan.BuyerChange = theirs, makerEnclave, mc
	}
	u, err := restricted.BuildDvP(plan)
	if err != nil {
		return "", err
	}
	seller := makerAID
	if !makerSells {
		seller = p.AID
	}
	sellerIns := plan.SellerInputs()
	if err := p.Policy.CheckDvPWithServer(u, p.Asset, seller, sellerIns); err != nil {
		sendXcFail(d.Crypter, send, "refused", err.Error())
		return "", err
	}
	raw, _ := u.Hex()
	if err := sendXc(&XcMsg{Type: XcDvpTx, DvpTx: raw}, d.Crypter, send); err != nil {
		return "", err
	}
	sm, err := recvXcType(recv, d.Crypter, XcDvpSigned, d.Timing.TermsReqWait)
	if err != nil {
		return "", err
	}
	if d.BeforeBroadcast != nil {
		d.BeforeBroadcast()
	}
	var txid string
	if makerSells {
		if sm.ExpiresAt <= p.Node.Now().Unix() {
			return "", fmt.Errorf("%w: the maker's signatures have already expired", ErrXcBadTerms)
		}
		if err := p.Node.SignWallet(u); err != nil {
			return "", err
		}
		txid, err = p.Policy.BroadcastDvP(u, p.Asset, seller, sellerIns, sm.UserSigs, sm.ExpiresAt, sm.ExpirySig)
	} else {
		signed, perr := restricted.ParseTx(sm.DvpTx)
		if perr != nil || signed.Txid() != u.Txid() {
			return "", fmt.Errorf("%w: the maker signed another transaction", ErrXcBadTerms)
		}
		for i := range u.Tx.Inputs {
			u.Tx.Inputs[i].Witness = signed.Tx.Inputs[i].Witness
		}
		for _, i := range sellerIns {
			u.Tx.Inputs[i].Witness = nil
		}
		if err := p.Node.SignWallet(u); err != nil {
			return "", err
		}
		exp := p.Node.Now().Add(d.Expiry).Unix()
		sigs, es, serr := p.SellerSigs(u, sellerIns, exp)
		if serr != nil {
			return "", serr
		}
		txid, err = p.Policy.BroadcastDvP(u, p.Asset, seller, sellerIns, sigs, exp, es)
	}
	if err != nil {
		sendXcFail(d.Crypter, send, "broadcast_refused", err.Error())
		return "", err
	}
	locked = nil
	_ = sendXc(&XcMsg{Type: XcDvpDone, SettleTxid: txid}, d.Crypter, send)
	d.logf("traded: %s", txid)
	return txid, nil
}
