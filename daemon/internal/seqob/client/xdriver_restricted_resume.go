package client

// Resuming a restricted swap from its saved state (RestrictedResult): what a
// side still owes after its process stopped mid-swap. Nothing here needs the
// counterparty or the relay; every step reads the chains.
//
// The seller (it locked the asset):
//   - no lock: nothing of its own is locked; the swap ends.
//   - a lock: it watches the pot for the buyer's claim and claims the BTC at
//     once with the secret read from it (or the one already saved); with no
//     claim by T_seq, it sends the HTLC and the pot home with the reserved fee
//     coins (sellerSettle, as in the live swap).
//
// The buyer (it locked the BTC and holds the secret):
//   - no BTC lock: the swap ends.
//   - the lock's outputs known and the cut-off not passed: it claims the
//     asset (buyerClaim, with the anchor gate and the claim rules).
//   - its claim already on the chain: the swap is settled.
//   - otherwise: it refunds the BTC once Bitcoin's median time passes T_btc.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// ResumeRestricted finishes a saved swap. r carries this side's party, the
// BTC median time reader, the fees and OnUpdate; its Ops are bound here from
// the saved hash (newOps). It returns the state it leaves, settled or ended,
// or an error while something is still owed (run it again later).
func ResumeRestricted(r *RestrictedSwap, res *RestrictedResult, newOps func(hashH []byte) (XcOps, error)) (*RestrictedResult, error) {
	if !res.Pending() {
		return res, nil
	}
	r.Timing.setDefaults()
	if r.Party == nil || r.BtcMedianTime == nil {
		return res, errors.New("resume: incomplete params")
	}
	if r.BtcFee == 0 {
		r.BtcFee = 1000
	}
	if res.Asset != "" && res.Asset != r.Party.Asset {
		p := *r.Party
		p.Asset = res.Asset
		r.Party = &p
	}
	if r.Ctx == nil {
		r.Ctx = context.Background()
	}
	end := func(why string) (*RestrictedResult, error) {
		res.Ended = why
		r.update(res)
		r.logf("resume: %s", why)
		return res, nil
	}
	key, err := hex.DecodeString(res.BtcKeyHex)
	if err != nil || len(key) != 32 {
		return res, fmt.Errorf("resume: the saved BTC key does not read")
	}
	btcKey := xchain.KeyFromBytes(key)
	bind := func() error {
		h, err := hex.DecodeString(res.HashH)
		if err != nil || len(h) != 32 {
			return fmt.Errorf("resume: the saved hash does not read")
		}
		ops, err := newOps(h)
		if err != nil {
			return err
		}
		r.Ops = ops
		return nil
	}

	switch res.Role {
	case "seller":
		if res.Locked == nil {
			return end("nothing of ours was locked")
		}
		if res.BtcLeg == nil {
			return res, errors.New("resume: a lock without the BTC leg it answered")
		}
		if err := bind(); err != nil {
			return res, err
		}
		hint := make(chan []byte, 1)
		if s, err := hex.DecodeString(res.Secret); err == nil && len(s) == 32 {
			hint <- s
		}
		if err := r.sellerSettle(res, btcKey, hint); err != nil {
			if res.Ended != "" {
				return res, nil
			}
			return res, err
		}
		return res, nil

	case "buyer":
		if res.BtcLeg == nil {
			return end("no BTC was locked")
		}
		if err := bind(); err != nil {
			return res, err
		}
		secret, err := hex.DecodeString(res.Secret)
		if err != nil || len(secret) != 32 {
			return res, errors.New("resume: the buyer's saved secret does not read")
		}
		if res.HTLCAt != nil {
			// The HTLC spent by a claim (its witness carries our secret) is
			// settled; spent by the refund, the BTC must come home.
			spender, w, err := r.Party.Node.SpendWitness(res.HTLCAt.Txid, res.HTLCAt.Vout)
			if err != nil {
				return res, err
			}
			if spender != "" {
				if claimedWith(w, secret) {
					res.SeqClaimTxid, res.Settled = spender, true
					r.update(res)
					return res, nil
				}
				r.logf("resume: the HTLC went home in %s", spender)
			} else if res.SeqClaimTxid != "" {
				r.logf("resume: our claim %s is not mined and the HTLC is unspent: claiming again", res.SeqClaimTxid)
			}
		}
		if res.Terms != nil && res.HTLCAt != nil && res.PotAt != nil {
			btcMTP, err := r.BtcMedianTime()
			if err != nil {
				return res, err
			}
			if restricted.ClaimAllowed(r.Party.Node.Now(), btcMTP, res.Terms.TSeq, res.TBtc) == nil {
				if hc, _ := r.Party.Node.CoinAt(res.HTLCAt.Txid, res.HTLCAt.Vout); hc != nil {
					if err := r.buyerClaim(res, *res.Terms, *res.HTLCAt, *res.PotAt, res.BtcLegHeight, secret); err == nil {
						res.Settled = true
						r.update(res)
						return res, nil
					} else if !errors.Is(err, restricted.ErrNoClaim) {
						r.logf("resume: claim: %v; the BTC refunds at T_btc", err)
					}
				}
			}
		}
		// No claim: the BTC comes home at T_btc. Never reveal s from here on.
		txid, err := RefundRestrictedBTC(r, res.BtcLeg, btcKey, res.TBtc, false)
		if err != nil {
			return res, err
		}
		res.Refunds = append(res.Refunds, txid)
		return end("the BTC went home at T_btc: " + txid)
	}
	return res, fmt.Errorf("resume: unknown role %q", res.Role)
}

// claimedWith reports whether a spend's witness carries the secret: the
// HTLC's claim leaf takes <policy sig> <buyer sig> <s> <leaf> <control>.
func claimedWith(w [][]byte, secret []byte) bool {
	for _, it := range w {
		if len(it) == 32 && hex.EncodeToString(it) == hex.EncodeToString(secret) {
			return true
		}
	}
	return false
}
