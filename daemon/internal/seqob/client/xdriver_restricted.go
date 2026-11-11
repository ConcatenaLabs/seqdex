package client

// xdriver_restricted.go runs a restricted (OpenAMP) asset's swap for on-chain
// BTC over the relay courier (offer member 29, RestrictedTerms, with the BTC
// sentinel as quote). The Bitcoin side is the proven pkg/xchain HTLC, with a
// time lock T_btc. The Sequentia side is the asset's policy server's swap HTLC
// and fee pot (pkg/restricted, pkg/covenant/htlc.go): the seller locks it as a
// move of its own coin, co-signed by the policy server; the buyer claims it
// with the secret and the policy server's signature, which is the transfer
// decision; after T_seq anyone sends it home, with no key.
//
// The buyer of the asset pays the BTC, holds the secret and locks first, in
// both directions:
//
//	BUY   (TRADE_DIR_SELL: the maker sells; the taker buys and holds s)
//	  taker -> terms_request{taker_aid}
//	  maker  : the taker may receive the amount from it (eligibility), else fail
//	  maker -> terms{maker_btc_claim_pub, btc_locktime T_btc, amounts, pot}
//	  taker  : may itself receive (eligibility); locks BTC, waits its confirmation
//	  taker -> btc_leg_funded{hash_h, leg(BTC), taker_btc_refund_pub}
//	  maker  : verifies the BTC leg; waits for its anchor to reach it; locks the
//	           HTLC and pot with T_seq (PickTSeq)
//	  maker -> seq_leg_locked{leg(HTLC, T_seq), pot_vout, payout}
//	  taker  : rebuilds both from the terms; anchor gate; claims before the
//	           cut-off (revealing s in the pot's witness)
//	  maker  : reads s from the pot's spend and claims the BTC at once; or, with
//	           no claim by T_seq, sends both home
//
//	SELL  (TRADE_DIR_BUY: the maker buys and holds s; the taker sells)
//	  taker -> terms_request{taker_aid, taker_btc_claim_pub}
//	  maker  : may receive the amount from the taker, else fail; locks BTC
//	  maker -> btc_leg_locked{hash_h, leg(BTC), maker_refund_pub, T_btc, amounts, pot}
//	  taker  : verifies the BTC leg; waits for its anchor; locks the HTLC and pot
//	  taker -> seq_leg_funded{leg(HTLC, T_seq), pot_vout, payout}
//	  maker  : rebuilds; anchor gate; claims before the cut-off
//	  maker -> secret_revealed{preimage}
//	  taker  : claims the BTC at once (s also read from the pot's spend); or
//	           sends both home after T_seq
//
// Every lock is a Unix time, on one clock for both chains, with
// T_btc - T_seq >= 24 h. The buyer never reveals s after T_seq - 1 h, nor once
// Bitcoin's median time is within 12 h of T_btc.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// MinBTCLegDepth is the least burial of the BTC leg before the seller locks
// the asset. At 0 the payer can replace its lock in the mempool after the
// seller locks; at 1 a one-block reorg, the common orphan, can remove it.
// Deeper reorgs are rarer, and the seller re-verifies the leg in the best
// chain after every wait and just before its lock, whose block anchors to a
// Bitcoin block at or above the leg's: a Bitcoin reorg that removes the leg
// reorgs that lock out with it.
const MinBTCLegDepth = 2

// RestrictedSwap is what a restricted swap's driver needs on either side.
type RestrictedSwap struct {
	Ops     XcOps             // the BTC leg and the anchor reads
	Party   *restricted.Party // this side's account, enclave key, node and wallet
	Crypter *Crypter
	Ctx     context.Context

	// BtcMedianTime reads Bitcoin's median time past.
	BtcMedianTime func() (int64, error)

	Value      uint64 // the asset's atoms traded
	BtcAmount  uint64 // sats
	PotAsset   string // display hex, from the signed offer
	PotAmount  uint64
	RfaPerVb   uint64 // the Sequentia fee rate, reference atoms per vbyte
	BtcFee     uint64 // sats for a BTC claim or refund
	MinBTCConf int

	Timing XcTiming
	Log    func(format string, args ...interface{})

	// OnUpdate is called whenever the swap gains recoverable state: persist it.
	OnUpdate func(*RestrictedResult)
}

// RestrictedResult is a restricted swap's state as its side persists it.
type RestrictedResult struct {
	HashH        string
	Secret       string // the buyer's from the start; the seller's once read
	TBtc, TSeq   uint32
	BtcLeg       *xchain.LegLock
	BtcLegHeight int64
	BtcKeyHex    string // this side's BTC key: the buyer's refund key, the seller's claim key
	Terms        *restricted.SwapTerms
	Locked       *restricted.Locked // the seller's lock
	// HTLCAt and PotAt are the lock's outputs, as the buyer learnt them.
	HTLCAt, PotAt *restricted.Outpoint
	SeqClaimTxid  string
	BtcClaimTxid  string
	Refunds       []string
	Settled       bool
	// Role is this side's part: "seller" (it locks the asset and claims the
	// BTC) or "buyer" (it locks the BTC and claims the asset).
	Role string
	// Asset is the restricted asset traded (display hex).
	Asset string
	// Ended says why a swap that did not settle needs nothing more: every
	// coin it locked is home. ResumeRestricted skips it.
	Ended string
}

// Pending reports whether a saved swap may still need this side to act.
func (res *RestrictedResult) Pending() bool { return !res.Settled && res.Ended == "" }

func (r *RestrictedSwap) logf(f string, a ...interface{}) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

func (r *RestrictedSwap) update(res *RestrictedResult) {
	if res.Asset == "" && r.Party != nil {
		res.Asset = r.Party.Asset
	}
	if r.OnUpdate != nil {
		r.OnUpdate(res)
	}
}

func (r *RestrictedSwap) defaults() error {
	r.Timing.setDefaults()
	if r.Ctx == nil {
		r.Ctx = context.Background()
	}
	if r.Ops == nil || r.Party == nil || r.Crypter == nil || r.BtcMedianTime == nil {
		return errors.New("restricted swap: incomplete params")
	}
	if r.Value == 0 || r.BtcAmount == 0 || r.PotAsset == "" || r.PotAmount == 0 || r.RfaPerVb == 0 {
		return errors.New("restricted swap: amounts, pot and fee rate required")
	}
	if r.BtcFee == 0 {
		r.BtcFee = 1000
	}
	if r.MinBTCConf < MinBTCLegDepth {
		r.MinBTCConf = MinBTCLegDepth
	}
	return nil
}

// checkTBtc is what the BTC payer's counterparty and the BTC payer each check
// of T_btc before anything is locked: far enough ahead for a T_seq at least
// MinTSeqAhead ahead and a day before T_btc, with an hour to spare for the BTC
// leg's confirmation, and no further than 48 h.
func checkTBtc(now time.Time, tBtc uint32) error {
	min := now.Add(restricted.BtcAfterSeq + restricted.MinTSeqAhead + time.Hour).Unix()
	max := now.Add(48 * time.Hour).Unix()
	if int64(tBtc) < min || int64(tBtc) > max {
		return fmt.Errorf("%w: T_btc %d is not between %d and %d", ErrXcBadTerms, tBtc, min, max)
	}
	return nil
}

// eligible asks the policy server whether aid may receive the amount from
// sender, and returns a refusal sentence when it may not.
func (r *RestrictedSwap) eligible(aid, sender string) error {
	e, err := r.Party.Policy.Eligible(r.Party.Asset, aid, sender, r.Value)
	if err != nil {
		return err
	}
	if !e.Eligible {
		return fmt.Errorf("not_eligible: %s", e.Reason)
	}
	return nil
}

// fundDeadline is the context the seller's anchor wait runs under: past
// T_btc - 24 h - MinTSeqAhead, no T_seq is possible.
func (r *RestrictedSwap) fundDeadline(tBtc uint32) (context.Context, context.CancelFunc) {
	at := time.Unix(int64(tBtc), 0).Add(-restricted.BtcAfterSeq - restricted.MinTSeqAhead)
	return context.WithDeadline(r.Ctx, at)
}

// sellerLock waits until our node's anchor reaches the BTC leg's height, then
// locks the HTLC and pot, and waits for the lock's confirmation.
//
// reverify checks the BTC leg again in the best chain (same outpoint, script
// and amount, buried MinBTCConf deep) and returns its height: it runs after
// every anchor wait, so the asset is never locked against a leg a reorg or a
// replacement removed meanwhile.
func (r *RestrictedSwap) sellerLock(res *RestrictedResult, terms restricted.SwapTerms, hp int64, reverify func() (int64, error)) (*restricted.Locked, string, error) {
	ctx, cancel := r.fundDeadline(res.TBtc)
	defer cancel()
	for {
		if err := waitSeqAnchorReachesBTCLeg(ctx, r.Ops, hp, fundWindow{}, r.Timing, r.logf); err != nil {
			return nil, "", fmt.Errorf("asset not locked: %w", err)
		}
		h, err := reverify()
		if err != nil {
			return nil, "", fmt.Errorf("asset not locked: the BTC leg no longer verifies: %w", err)
		}
		if h == hp {
			break
		}
		r.logf("the BTC leg moved from height %d to %d: waiting for the anchor again", hp, h)
		hp, res.BtcLegHeight = h, h
		r.update(res)
	}
	tSeq, err := restricted.PickTSeq(r.Party.Node.Now(), res.TBtc)
	if err != nil {
		return nil, "", err
	}
	terms.TSeq = tSeq
	locked, err := r.Party.Lock(terms, r.RfaPerVb)
	if err != nil {
		return nil, "", fmt.Errorf("lock: %w", err)
	}
	res.TSeq, res.Locked, res.Terms = tSeq, locked, &locked.Terms
	r.update(res)
	r.logf("locked %d of %s in the HTLC %s:%d, T_seq %d", r.Value, r.Party.Asset, locked.Txid, locked.HTLCVout, tSeq)
	for {
		if bh, err := r.Ops.SeqBlockHashOfTx(locked.Txid); err == nil && bh != "" {
			return locked, bh, nil
		}
		if r.Party.Node.Now().Unix() >= int64(tSeq) {
			return locked, "", fmt.Errorf("the lock %s was not mined before T_seq", locked.Txid)
		}
		time.Sleep(r.Timing.Poll)
	}
}

// buyerClaim checks the lock against the terms, runs the anchor gate, and
// claims before the cut-off.
func (r *RestrictedSwap) buyerClaim(res *RestrictedResult, terms restricted.SwapTerms, htlc, pot restricted.Outpoint, hp int64, secret []byte) error {
	if err := restricted.CheckLocks(terms.TSeq, res.TBtc); err != nil {
		return fmt.Errorf("%w: %v", ErrXcBadTerms, err)
	}
	if now := r.Party.Node.Now(); int64(terms.TSeq) > now.Add(restricted.TSeqAhead+10*time.Minute).Unix() {
		return fmt.Errorf("%w: T_seq %d is further ahead than a lock gives", ErrXcBadTerms, terms.TSeq)
	}
	cutoff := time.Unix(int64(terms.TSeq), 0).Add(-restricted.ClaimCutoff)
	ctx, cancel := context.WithDeadline(r.Ctx, cutoff)
	defer cancel()
	// The window inside gateSeqLeg counts blocks; the cut-off here is a time,
	// carried by the context, so the block window is left open.
	if _, err := gateSeqLeg(ctx, r.Ops, htlc.Txid, hp, claimWindow{SeqLocktime: math.MaxUint32, Margin: 1}, r.Timing, r.logf); err != nil {
		return fmt.Errorf("anchor gate did not pass before the cut-off: %w (the BTC refunds at T_btc %d)", err, res.TBtc)
	}
	btcMTP, err := r.BtcMedianTime()
	if err != nil {
		return err
	}
	if err := restricted.ClaimAllowed(r.Party.Node.Now(), btcMTP, terms.TSeq, res.TBtc); err != nil {
		return err
	}
	txid, err := r.Party.Claim(terms, htlc, pot, secret, r.RfaPerVb)
	if err != nil {
		return fmt.Errorf("claim: %w (the BTC refunds at T_btc %d)", err, res.TBtc)
	}
	res.SeqClaimTxid = txid
	r.update(res)
	r.logf("claimed the HTLC: %s (the secret is on the chain)", txid)
	return nil
}

// sellerSettle watches the pot for the buyer's claim and claims the BTC at
// once with s; with no claim by T_seq, it sends the HTLC and the pot home.
func (r *RestrictedSwap) sellerSettle(res *RestrictedResult, btcKey *xchain.Key, hint <-chan []byte) error {
	l := res.Locked
	for {
		if err := r.Ctx.Err(); err != nil {
			return fmt.Errorf("%w: the lock %s is unsettled; resume this swap's state to finish it", ErrXcCanceled, l.Txid)
		}
		var secret []byte
		select {
		case s := <-hint:
			if sha256.Sum256(s) == l.Terms.Hash {
				secret = s
			}
		default:
		}
		if secret == nil {
			s, txid, err := r.Party.WatchSecret(l)
			if err != nil {
				r.logf("watching the pot: %v", err)
			}
			if s != nil {
				secret = s
				res.SeqClaimTxid = txid
			}
		}
		if secret != nil {
			res.Secret = hex.EncodeToString(secret)
			r.update(res)
			if err := r.Ops.InjectSecret(secret); err != nil {
				return err
			}
			for {
				txid, err := r.Ops.ClaimBTCLeg(res.BtcLeg, btcKey, r.BtcFee)
				if err == nil {
					res.BtcClaimTxid, res.Settled = txid, true
					r.update(res)
					r.logf("claimed the BTC: %s", txid)
					return nil
				}
				r.logf("BTC claim: %v; retrying", err)
				if btcMTP, e := r.BtcMedianTime(); e == nil && btcMTP >= int64(res.TBtc) {
					return fmt.Errorf("the BTC refund has opened before the claim landed: %w", err)
				}
				if r.Ctx.Err() != nil {
					return fmt.Errorf("%w: the BTC claim is unmade; resume this swap's state to finish it", ErrXcCanceled)
				}
				time.Sleep(r.Timing.Poll)
			}
		}
		if r.Party.Node.Now().Unix() > int64(res.TSeq) {
			txids, err := r.Party.Refund(l, r.RfaPerVb)
			res.Refunds = append(res.Refunds, txids...)
			r.update(res)
			if err == nil {
				// One last look: a claim mined in the same moment wins.
				if s, _, _ := r.Party.WatchSecret(l); s != nil {
					continue
				}
				r.logf("no claim by T_seq: the HTLC and the pot went home (%v)", txids)
				res.Ended = "refunded after T_seq"
				r.update(res)
				return fmt.Errorf("no claim by T_seq; refunded %v", txids)
			}
			r.logf("refund: %v; retrying", err)
		}
		time.Sleep(r.Timing.Poll)
	}
}

// verifyBuried retries the seller's first verification of the BTC leg while
// it is only too shallow, until BtcConfWait: every other failure is final.
func (r *RestrictedSwap) verifyBuried(verify func() (*xchain.VerifiedBTCLeg, error)) (*xchain.VerifiedBTCLeg, error) {
	deadline := time.Now().Add(r.Timing.BtcConfWait)
	for {
		v, err := verify()
		if err == nil || !errors.Is(err, xchain.ErrBTCLegUnconfirmed) || time.Now().After(deadline) {
			return v, err
		}
		time.Sleep(r.Timing.Poll)
	}
}

func btcLegHeight(r *RestrictedSwap, txid string) (int64, error) {
	deadline := time.Now().Add(r.Timing.BtcConfWait)
	for {
		h, _, err := btcLegConfirmedHeight(r.Ops, txid, r.MinBTCConf)
		if err == nil {
			return h, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("btc leg %s: no %d-conf within %s", txid, r.MinBTCConf, r.Timing.BtcConfWait)
		}
		time.Sleep(r.Timing.Poll)
	}
}

func decodePub(h string) ([]byte, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 33 {
		return nil, fmt.Errorf("%w: bad public key %q", ErrXcBadTerms, h)
	}
	return b, nil
}

// --- the taker buys -------------------------------------------------------

// RunTakerRestrictedBuy is the taker buying the asset for BTC from a maker
// whose account is makerAID. secret is the taker's s, persisted by the caller
// before this runs.
func RunTakerRestrictedBuy(r *RestrictedSwap, makerAID string, secret []byte, btcRefund *xchain.Key, send XcSend, recv XcRecv) (*RestrictedResult, error) {
	if err := r.defaults(); err != nil {
		return nil, err
	}
	if len(secret) != 32 || btcRefund == nil {
		return nil, errors.New("restricted buy: a 32-byte secret and a refund key")
	}
	h := sha256.Sum256(secret)
	res := &RestrictedResult{Role: "buyer", HashH: hex.EncodeToString(h[:]), Secret: hex.EncodeToString(secret), BtcKeyHex: hex.EncodeToString(btcRefund.Bytes())}
	// Approved before anything moves: never lock BTC for an asset we could not receive.
	if err := r.eligible(r.Party.AID, makerAID); err != nil {
		return res, err
	}
	if err := sendXc(&XcMsg{Type: XcTermsRequest, TakerAID: r.Party.AID}, r.Crypter, send); err != nil {
		return res, err
	}
	terms, err := recvXcType(recv, r.Crypter, XcTerms, r.Timing.TermsWait)
	if err != nil {
		return res, err
	}
	if terms.SeqAmount != r.Value || terms.BtcAmount != r.BtcAmount || terms.PotAsset != r.PotAsset || terms.PotAmount != r.PotAmount {
		sendXcFail(r.Crypter, send, "terms_mismatch", "the terms are not the signed offer's")
		return res, fmt.Errorf("%w: terms %d/%d/%s/%d are not the offer's", ErrXcBadTerms, terms.SeqAmount, terms.BtcAmount, terms.PotAsset, terms.PotAmount)
	}
	if err := checkTBtc(r.Party.Node.Now(), terms.BtcLocktime); err != nil {
		sendXcFail(r.Crypter, send, "terms_mismatch", err.Error())
		return res, err
	}
	makerClaim, err := decodePub(terms.MakerBtcClaimPub)
	if err != nil {
		return res, err
	}
	res.TBtc = terms.BtcLocktime
	leg, hp, err := r.Ops.LockBTCLeg(makerClaim, btcRefund.PubKey(), atomsToCoins(r.BtcAmount), terms.BtcLocktime)
	if err != nil {
		sendXcFail(r.Crypter, send, "btc_lock_failed", err.Error())
		return res, err
	}
	res.BtcLeg = leg
	r.update(res)
	{ // buried MinBTCConf deep before the seller is told: it locks against nothing shallower
		if hp, err = btcLegHeight(r, leg.Funded.TxID); err != nil {
			sendXcFail(r.Crypter, send, "btc_conf_timeout", err.Error())
			return res, err
		}
	}
	res.BtcLegHeight = hp
	r.update(res)
	if err := sendXc(&XcMsg{Type: XcBtcLegFunded, HashH: res.HashH, TakerBtcRefundPub: hex.EncodeToString(btcRefund.PubKey()),
		SeqAmount: r.Value, BtcAmount: r.BtcAmount,
		Leg: &XcLeg{Txid: leg.Funded.TxID, Vout: leg.Funded.Vout, Amount: leg.Funded.Amount,
			RedeemScript: hex.EncodeToString(leg.Script), Locktime: terms.BtcLocktime, Height: hp}}, r.Crypter, send); err != nil {
		return res, err
	}
	cutoff := time.Unix(int64(terms.BtcLocktime), 0).Add(-restricted.BtcAfterSeq - restricted.ClaimCutoff)
	locked, err := recvXcTypeWhile(recv, r.Crypter, XcSeqLegLocked, r.Timing.SeqLockWait, func() error {
		if r.Ctx.Err() != nil {
			return fmt.Errorf("%w (the BTC refunds at T_btc %d)", ErrXcCanceled, terms.BtcLocktime)
		}
		if r.Party.Node.Now().After(cutoff) {
			return fmt.Errorf("no asset locked before any claim could be made (the BTC refunds at T_btc %d)", terms.BtcLocktime)
		}
		return nil
	}, r.logf)
	if err != nil {
		return res, err
	}
	if locked.Leg == nil || locked.PotVout == nil {
		return res, errors.New("seq_leg_locked without the HTLC and the pot")
	}
	payout, err := hex.DecodeString(locked.Payout)
	if err != nil {
		return res, fmt.Errorf("%w: payout", ErrXcBadTerms)
	}
	st := restricted.SwapTerms{Hash: h, SellerAID: makerAID, BuyerAID: r.Party.AID, TSeq: locked.Leg.Locktime,
		Value: r.Value, PotAsset: r.PotAsset, PotValue: r.PotAmount, Payout: payout}
	res.TSeq, res.Terms = st.TSeq, &st
	res.HTLCAt = &restricted.Outpoint{Txid: locked.Leg.Txid, Vout: locked.Leg.Vout}
	res.PotAt = &restricted.Outpoint{Txid: locked.Leg.Txid, Vout: *locked.PotVout}
	r.update(res)
	if err := r.buyerClaim(res, st, *res.HTLCAt, *res.PotAt, hp, secret); err != nil {
		return res, err
	}
	res.Settled = true
	r.update(res)
	return res, nil
}

// --- the maker sells --------------------------------------------------------

// RunMakerRestrictedSell is the maker selling the asset for BTC. NewOps binds
// the BTC engine to the taker's hash once it arrives.
func RunMakerRestrictedSell(r *RestrictedSwap, newOps func(hashH []byte) (XcOps, error), in <-chan []byte, send XcSend) (*RestrictedResult, error) {
	if r.Ops == nil {
		r.Ops = noOps{}
	}
	if err := r.defaults(); err != nil {
		return nil, err
	}
	recv := chanRecv(in)
	res := &RestrictedResult{Role: "seller"}
	req, err := recvXcType(recv, r.Crypter, XcTermsRequest, r.Timing.TermsReqWait)
	if err != nil {
		return res, err
	}
	if err := r.eligible(req.TakerAID, r.Party.AID); err != nil {
		sendXcFail(r.Crypter, send, "not_eligible", err.Error())
		return res, err
	}
	claimKey, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	res.BtcKeyHex = hex.EncodeToString(claimKey.Bytes())
	res.TBtc = uint32(r.Party.Node.Now().Add(restricted.TBtcAhead).Unix())
	r.update(res)
	if err := sendXc(&XcMsg{Type: XcTerms, MakerBtcClaimPub: hex.EncodeToString(claimKey.PubKey()), BtcLocktime: res.TBtc,
		BtcAmount: r.BtcAmount, SeqAmount: r.Value, PotAsset: r.PotAsset, PotAmount: r.PotAmount, FeeBtc: r.BtcFee}, r.Crypter, send); err != nil {
		return res, err
	}
	funded, err := recvXcType(recv, r.Crypter, XcBtcLegFunded, r.Timing.BtcConfWait)
	if err != nil {
		return res, err
	}
	hb, err := hex.DecodeString(funded.HashH)
	if err != nil || len(hb) != 32 || funded.Leg == nil {
		sendXcFail(r.Crypter, send, "bad_leg", "hash_h and the BTC leg are required")
		return res, fmt.Errorf("%w: btc_leg_funded", ErrXcBadTerms)
	}
	refundPub, err := decodePub(funded.TakerBtcRefundPub)
	if err != nil {
		return res, err
	}
	ops, err := newOps(hb)
	if err != nil {
		return res, err
	}
	r.Ops = ops
	script, err := hex.DecodeString(funded.Leg.RedeemScript)
	if err != nil {
		return res, err
	}
	v, err := r.verifyBuried(func() (*xchain.VerifiedBTCLeg, error) {
		return ops.VerifyBTCLeg(hb, claimKey.PubKey(), refundPub, script, res.TBtc, funded.Leg.Txid, funded.Leg.Vout, r.BtcAmount, r.MinBTCConf)
	})
	if err != nil {
		sendXcFail(r.Crypter, send, "btc_leg_invalid", err.Error())
		return res, err
	}
	res.HashH, res.BtcLeg, res.BtcLegHeight = funded.HashH, v.Leg, v.Height
	r.update(res)
	var h [32]byte
	copy(h[:], hb)
	reverify := func() (int64, error) {
		v, err := ops.VerifyBTCLeg(hb, claimKey.PubKey(), refundPub, script, res.TBtc, funded.Leg.Txid, funded.Leg.Vout, r.BtcAmount, r.MinBTCConf)
		if err != nil {
			return 0, err
		}
		return v.Height, nil
	}
	locked, blockHash, err := r.sellerLock(res, restricted.SwapTerms{Hash: h, SellerAID: r.Party.AID, BuyerAID: req.TakerAID,
		Value: r.Value, PotAsset: r.PotAsset, PotValue: r.PotAmount}, v.Height, reverify)
	if err != nil {
		sendXcFail(r.Crypter, send, "lock_failed", err.Error())
		if locked == nil {
			return res, err
		}
		return res, r.sellerSettle(res, claimKey, nil)
	}
	anchorH, _ := ops.SeqAnchorHeightOf(blockHash)
	pv := locked.PotVout
	if err := sendXc(&XcMsg{Type: XcSeqLegLocked, PotVout: &pv, Payout: hex.EncodeToString(locked.Terms.Payout),
		Leg: &XcLeg{Txid: locked.Txid, Vout: locked.HTLCVout, Amount: r.Value, Asset: r.Party.Asset, Locktime: locked.Terms.TSeq,
			BlockHash: blockHash, AnchorHeight: anchorH}}, r.Crypter, send); err != nil {
		r.logf("announcing the lock: %v (settling regardless)", err)
	}
	return res, r.sellerSettle(res, claimKey, nil)
}

// --- the taker sells -------------------------------------------------------

// RunTakerRestrictedSell is the taker selling the asset for BTC to a maker
// whose account is makerAID; the maker holds s.
func RunTakerRestrictedSell(r *RestrictedSwap, makerAID string, send XcSend, recv XcRecv) (*RestrictedResult, error) {
	if err := r.defaults(); err != nil {
		return nil, err
	}
	claimKey, err := xchain.NewKey()
	if err != nil {
		return nil, err
	}
	res := &RestrictedResult{Role: "seller", BtcKeyHex: hex.EncodeToString(claimKey.Bytes())}
	r.update(res)
	if err := r.eligible(makerAID, r.Party.AID); err != nil {
		return res, err
	}
	if err := sendXc(&XcMsg{Type: XcTermsRequest, TakerAID: r.Party.AID, TakerBtcClaimPub: hex.EncodeToString(claimKey.PubKey())}, r.Crypter, send); err != nil {
		return res, err
	}
	m, err := recvXcType(recv, r.Crypter, XcBtcLegLocked, r.Timing.BtcConfWait)
	if err != nil {
		return res, err
	}
	if m.SeqAmount != r.Value || m.BtcAmount != r.BtcAmount || m.PotAsset != r.PotAsset || m.PotAmount != r.PotAmount || m.Leg == nil {
		sendXcFail(r.Crypter, send, "terms_mismatch", "the terms are not the signed offer's")
		return res, fmt.Errorf("%w: btc_leg_locked does not match the offer", ErrXcBadTerms)
	}
	if err := checkTBtc(r.Party.Node.Now().Add(-r.Timing.BtcConfWait), m.BtcLocktime); err != nil {
		sendXcFail(r.Crypter, send, "terms_mismatch", err.Error())
		return res, err
	}
	hb, err := hex.DecodeString(m.HashH)
	if err != nil || len(hb) != 32 {
		return res, fmt.Errorf("%w: hash_h", ErrXcBadTerms)
	}
	refundPub, err := decodePub(m.MakerRefundPub)
	if err != nil {
		return res, err
	}
	script, err := hex.DecodeString(m.Leg.RedeemScript)
	if err != nil {
		return res, err
	}
	v, err := r.verifyBuried(func() (*xchain.VerifiedBTCLeg, error) {
		return r.Ops.VerifyBTCLeg(hb, claimKey.PubKey(), refundPub, script, m.BtcLocktime, m.Leg.Txid, m.Leg.Vout, r.BtcAmount, r.MinBTCConf)
	})
	if err != nil {
		sendXcFail(r.Crypter, send, "btc_leg_invalid", err.Error())
		return res, err
	}
	res.HashH, res.TBtc, res.BtcLeg, res.BtcLegHeight = m.HashH, m.BtcLocktime, v.Leg, v.Height
	r.update(res)
	var h [32]byte
	copy(h[:], hb)
	reverify := func() (int64, error) {
		v, err := r.Ops.VerifyBTCLeg(hb, claimKey.PubKey(), refundPub, script, m.BtcLocktime, m.Leg.Txid, m.Leg.Vout, r.BtcAmount, r.MinBTCConf)
		if err != nil {
			return 0, err
		}
		return v.Height, nil
	}
	locked, blockHash, err := r.sellerLock(res, restricted.SwapTerms{Hash: h, SellerAID: r.Party.AID, BuyerAID: makerAID,
		Value: r.Value, PotAsset: r.PotAsset, PotValue: r.PotAmount}, v.Height, reverify)
	if err != nil {
		sendXcFail(r.Crypter, send, "lock_failed", err.Error())
		if locked == nil {
			return res, err
		}
		return res, r.sellerSettle(res, claimKey, nil)
	}
	anchorH, _ := r.Ops.SeqAnchorHeightOf(blockHash)
	pv := locked.PotVout
	if err := sendXc(&XcMsg{Type: XcSeqLegFunded, PotVout: &pv, Payout: hex.EncodeToString(locked.Terms.Payout),
		Leg: &XcLeg{Txid: locked.Txid, Vout: locked.HTLCVout, Amount: r.Value, Asset: r.Party.Asset, Locktime: locked.Terms.TSeq,
			BlockHash: blockHash, AnchorHeight: anchorH}}, r.Crypter, send); err != nil {
		r.logf("announcing the lock: %v (settling regardless)", err)
	}
	// The maker's secret_revealed is a hint only; the pot's spend is the source.
	hint := make(chan []byte, 1)
	go func() {
		if sm, err := recvXcType(recv, r.Crypter, XcSecretRevealed, time.Until(time.Unix(int64(locked.Terms.TSeq), 0))); err == nil {
			if s, err := hex.DecodeString(sm.Preimage); err == nil {
				hint <- s
			}
		}
	}()
	return res, r.sellerSettle(res, claimKey, hint)
}

// --- the maker buys ----------------------------------------------------------

// RunMakerRestrictedBuy is the maker buying the asset for BTC; it holds s.
// newOps binds the BTC engine to the maker's own hash.
func RunMakerRestrictedBuy(r *RestrictedSwap, newOps func(hashH []byte) (XcOps, error), in <-chan []byte, send XcSend) (*RestrictedResult, error) {
	if r.Ops == nil {
		r.Ops = noOps{}
	}
	if err := r.defaults(); err != nil {
		return nil, err
	}
	recv := chanRecv(in)
	req, err := recvXcType(recv, r.Crypter, XcTermsRequest, r.Timing.TermsReqWait)
	if err != nil {
		return nil, err
	}
	if err := r.eligible(r.Party.AID, req.TakerAID); err != nil {
		sendXcFail(r.Crypter, send, "not_eligible", "the maker cannot receive this asset from you now")
		return nil, err
	}
	takerClaim, err := decodePub(req.TakerBtcClaimPub)
	if err != nil {
		sendXcFail(r.Crypter, send, "terms_mismatch", err.Error())
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	h := sha256.Sum256(secret)
	refundKey, err := xchain.NewKey()
	if err != nil {
		return nil, err
	}
	res := &RestrictedResult{Role: "buyer", HashH: hex.EncodeToString(h[:]), Secret: hex.EncodeToString(secret), BtcKeyHex: hex.EncodeToString(refundKey.Bytes()),
		TBtc: uint32(r.Party.Node.Now().Add(restricted.TBtcAhead).Unix())}
	r.update(res)
	ops, err := newOps(h[:])
	if err != nil {
		return res, err
	}
	r.Ops = ops
	leg, hp, err := ops.LockBTCLeg(takerClaim, refundKey.PubKey(), atomsToCoins(r.BtcAmount), res.TBtc)
	if err != nil {
		sendXcFail(r.Crypter, send, "btc_lock_failed", err.Error())
		return res, err
	}
	res.BtcLeg = leg
	r.update(res)
	{ // buried MinBTCConf deep before the seller is told: it locks against nothing shallower
		if hp, err = btcLegHeight(r, leg.Funded.TxID); err != nil {
			sendXcFail(r.Crypter, send, "btc_conf_timeout", err.Error())
			return res, err
		}
	}
	res.BtcLegHeight = hp
	r.update(res)
	if err := sendXc(&XcMsg{Type: XcBtcLegLocked, HashH: res.HashH, MakerRefundPub: hex.EncodeToString(refundKey.PubKey()),
		BtcLocktime: res.TBtc, BtcAmount: r.BtcAmount, SeqAmount: r.Value, PotAsset: r.PotAsset, PotAmount: r.PotAmount,
		Leg: &XcLeg{Txid: leg.Funded.TxID, Vout: leg.Funded.Vout, Amount: leg.Funded.Amount, RedeemScript: hex.EncodeToString(leg.Script),
			Locktime: res.TBtc, Height: hp}}, r.Crypter, send); err != nil {
		return res, err
	}
	cutoff := time.Unix(int64(res.TBtc), 0).Add(-restricted.BtcAfterSeq - restricted.ClaimCutoff)
	funded, err := recvXcTypeWhile(recv, r.Crypter, XcSeqLegFunded, r.Timing.SeqLockWait, func() error {
		if r.Ctx.Err() != nil {
			return fmt.Errorf("%w (the BTC refunds at T_btc %d)", ErrXcCanceled, res.TBtc)
		}
		if r.Party.Node.Now().After(cutoff) {
			return fmt.Errorf("no asset locked before any claim could be made (the BTC refunds at T_btc %d)", res.TBtc)
		}
		return nil
	}, r.logf)
	if err != nil {
		return res, err
	}
	if funded.Leg == nil || funded.PotVout == nil {
		return res, errors.New("seq_leg_funded without the HTLC and the pot")
	}
	payout, err := hex.DecodeString(funded.Payout)
	if err != nil {
		return res, fmt.Errorf("%w: payout", ErrXcBadTerms)
	}
	st := restricted.SwapTerms{Hash: h, SellerAID: req.TakerAID, BuyerAID: r.Party.AID, TSeq: funded.Leg.Locktime,
		Value: r.Value, PotAsset: r.PotAsset, PotValue: r.PotAmount, Payout: payout}
	res.TSeq, res.Terms = st.TSeq, &st
	res.HTLCAt = &restricted.Outpoint{Txid: funded.Leg.Txid, Vout: funded.Leg.Vout}
	res.PotAt = &restricted.Outpoint{Txid: funded.Leg.Txid, Vout: *funded.PotVout}
	r.update(res)
	if err := r.buyerClaim(res, st, *res.HTLCAt, *res.PotAt, hp, secret); err != nil {
		return res, err
	}
	_ = sendXc(&XcMsg{Type: XcSecretRevealed, Preimage: res.Secret}, r.Crypter, send)
	res.Settled = true
	r.update(res)
	return res, nil
}

// noOps stands in before the BTC engine is bound to a hash; nothing calls it.
type noOps struct{ XcOps }

// RefundRestrictedBTC sends a restricted swap's BTC leg back to its payer once
// Bitcoin's median time has passed T_btc, a time (BIP113), not a height. With
// wait=false it returns ErrXcRefundNotDue when early.
func RefundRestrictedBTC(r *RestrictedSwap, leg *xchain.LegLock, key *xchain.Key, tBtc uint32, wait bool) (string, error) {
	r.Timing.setDefaults()
	for {
		mtp, err := r.BtcMedianTime()
		if err != nil {
			return "", err
		}
		if mtp >= int64(tBtc) {
			break
		}
		if !wait {
			return "", fmt.Errorf("%w: Bitcoin's median time %d < T_btc %d", ErrXcRefundNotDue, mtp, tBtc)
		}
		time.Sleep(r.Timing.Poll)
	}
	fee := r.BtcFee
	if fee == 0 {
		fee = 1000
	}
	return r.Ops.RefundBTCLeg(leg, key, tBtc, xcSafeFee(fee, leg.Funded.Amount))
}
