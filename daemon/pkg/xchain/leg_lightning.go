package xchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// This file adds the LIGHTNING leg used by SeqLN Phase 2 submarine swaps
// (Sequentia/doc/sequentia/seqln-phase2-submarine-swaps.md). In a
// submarine swap the BTC leg is not an on-chain HTLC (btcBackend) but a Bitcoin
// Lightning payment bound to the SEQ on-chain HTLC by the SAME SHA256 preimage.
// The SEQ leg and the anchor-depth gate (VerifySeqLegSafe) are unchanged.
//
// Two directions (both Case A: Sequentia asset on-chain <-> BTC over vanilla LN):
//
//   - NORMAL (on-chain SEQ asset -> BTC-LN): the taker locks the SEQ asset HTLC
//     (claim=maker with P) and hands the maker a BOLT11 whose payment_hash = H.
//     The maker VERIFIES the SEQ leg is locked + anchor-deep, then PAYS the
//     BOLT11 -- which reveals P -- and uses P to CLAIM the SEQ leg. This needs
//     only CLN's core `pay`, so it works against a stock SeqLN/CLN node.
//
//   - REVERSE (BTC-LN -> on-chain SEQ asset): the maker issues a HOLD invoice on
//     H and locks the SEQ asset HTLC (claim=taker with P). The taker pays the
//     hold invoice (LN HTLC held), then claims the asset on-chain with P
//     (revealing it); the maker reads P off the SEQ claim and -- ONLY once that
//     claim is anchor-deep -- SETTLES the held invoice. Holding an HTLC needs the
//     `htlc_accepted` hook, provided by a holdinvoice plugin on the CLN node; the
//     hold methods below drive that plugin's RPC.
//
// The maker plays the Boltz role (non-custodial; worst case is a timelock refund).

// LNLeg abstracts the maker's Bitcoin-Lightning leg of a submarine swap. It is
// the LN analogue of btcBackend; it deliberately does NOT reuse btcBackend
// because an LN leg has no funded P2SH, txid or block height.
type LNLeg interface {
	// NodeID returns the node's Lightning id (hex pubkey).
	NodeID() (string, error)

	// --- NORMAL direction: the maker pays and thereby learns the preimage ---

	// Pay pays a BOLT11 whose payment_hash MUST equal wantHash and returns the
	// preimage the payee revealed. The caller uses that preimage to redeem the
	// SEQ leg. It fails if the invoice's hash != wantHash (so the maker never
	// pays an invoice not tied to the swap secret).
	Pay(bolt11 string, wantHash []byte, amountMsat uint64) (preimage []byte, err error)

	// PayHash pays a BARE payment_hash (no BOLT11) to destNodeID and returns the
	// preimage the payee reveals on settle. Used to pay a counterparty's hold leg
	// in a pure-LN swap, where the payee has registered the hash with a hold
	// plugin and there is no invoice. Routes in this leg's asset.
	PayHash(destNodeID string, paymentHash []byte, amountMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error)

	// --- REVERSE direction: the maker holds an invoice and settles on P ---

	// CreateHoldInvoice registers a hold invoice on paymentHash H for amountMsat
	// with the given cltv/label/description and returns the BOLT11 for the taker
	// to pay. Requires a holdinvoice plugin on the node.
	CreateHoldInvoice(paymentHash []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (bolt11 string, err error)

	// WaitHeld blocks until the taker's HTLC for paymentHash is accepted-and-held
	// (state "accepted") or the deadline. Returns the held amount (msat).
	WaitHeld(paymentHash []byte, timeout time.Duration) (heldMsat uint64, err error)

	// SettleHold resolves the held invoice with the preimage: the maker takes the
	// incoming BTC-LN. Only call once the SEQ-side reveal is anchor-deep.
	SettleHold(paymentHash, preimage []byte) error

	// CancelHold fails the held invoice back to the payer (timeout / refund path).
	CancelHold(paymentHash []byte) error

	// --- REVERSE (maker-secret / taker-gated) direction: plain invoice ---
	//
	// This is the plugin-free reverse mode: the MAKER generates the preimage P,
	// so a PLAIN invoice suffices (no hold). The taker verifies + anchor-gates the
	// asset HTLC BEFORE paying; paying reveals P (payer learns the preimage), and
	// the taker claims the asset with it. See SubmarineSwap.OfferReverseMakerSecret.

	// CreateInvoice creates a plain BOLT11 whose payment_hash = SHA256(preimage)
	// for amountMsat. The node knows the preimage, so it settles normally on
	// payment (revealing P to the payer).
	CreateInvoice(preimage []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (bolt11 string, err error)

	// WaitInvoicePaid blocks until the invoice with the given label is paid (or
	// the deadline / invoice expiry), returning the received amount (msat).
	WaitInvoicePaid(label string, timeout time.Duration) (paidMsat uint64, err error)

	// WaitPaidByHash blocks until the invoice with the given payment_hash is paid,
	// polling listinvoices (the invoice may have been created out-of-band, e.g. by
	// a device with its OWN preimage, so the caller has only the hash, not a label
	// and NOT the preimage). Returns the received amount (msat).
	WaitPaidByHash(paymentHash []byte, timeout time.Duration) (paidMsat uint64, err error)
}

// --- CLN implementation over the lightning-rpc unix socket ------------------

// clnLNLeg implements LNLeg against a Core Lightning (SeqLN) node via its
// lightning-rpc unix socket (JSON-RPC 2.0). Point it at a node running
// --network=testnet4/bitcoin for real BTC.
type clnLNLeg struct {
	rpc *lnRPC
	// assetID is the 32-byte hex Sequentia asset id this leg is denominated in
	// (as displayed), the Sequence token's included. "" means the node's network
	// has no assets: Bitcoin on a Bitcoin Lightning node. When set, Pay and
	// PayHash route in this asset, and holds and invoices are made in it, via
	// SeqLN's `asset` parameter. A leg with no asset on a Sequentia node makes no
	// hold and no invoice: no asset is ever a default.
	assetID string
	// timing is the chain this leg's HTLCs expire on: Bitcoin for the leg of a
	// real BTC-LN node, Sequentia slots for an asset leg. Every timelock this leg
	// pays with or holds against is converted through it.
	timing ChainTiming
}

// clnLNLeg must satisfy LNLeg.
var _ LNLeg = (*clnLNLeg)(nil)

// Timing reports the chain this leg's HTLCs expire on.
func (l *clnLNLeg) Timing() ChainTiming { return l.timing }

// NewCLNLNLeg builds a Lightning leg backed by the CLN node whose lightning-rpc
// socket is at socketPath (e.g. <lightning-dir>/<network>/lightning-rpc), for a
// Bitcoin Lightning node (--network=bitcoin/testnet4), whose channels hold
// Bitcoin and nothing else. On a Sequentia node use NewCLNAssetLNLeg, with the
// Sequence token's id for a leg in the token: this leg names no asset, so it
// refuses to make a hold or an invoice there.
func NewCLNLNLeg(socketPath string) *clnLNLeg {
	return &clnLNLeg{rpc: &lnRPC{socketPath: socketPath, timeout: 120 * time.Second}, timing: BTCTiming}
}

// NewCLNAssetLNLeg builds a Lightning leg denominated in a Sequentia asset
// (assetIDHex = the 32-byte hex asset id, as displayed; the Sequence token is
// named by its id like any other asset). Pay/PayHash route only over channels of
// that asset, and holds and invoices are made in it. This is the asset side of a
// pure-LN swap.
func NewCLNAssetLNLeg(socketPath, assetIDHex string) *clnLNLeg {
	return &clnLNLeg{
		rpc:     &lnRPC{socketPath: socketPath, timeout: 120 * time.Second},
		assetID: assetIDHex,
		timing:  SeqTiming,
	}
}

func (l *clnLNLeg) NodeID() (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	if err := l.rpc.call(&res, "getinfo", map[string]interface{}{}); err != nil {
		return "", err
	}
	return res.ID, nil
}

// networkHasAssets reports whether a node on this network holds channels in
// several assets: Sequentia (and Elements-derived Liquid) networks do, Bitcoin
// networks do not.
func networkHasAssets(network string) bool {
	return strings.HasPrefix(network, "sequentia") || strings.HasPrefix(network, "liquid")
}

// legAsset is the asset this leg's holds and invoices are made in, lower-case:
// the leg's asset id, or "" on a node whose network has no assets. On a Sequentia
// node a leg that names no asset is refused rather than letting the node choose
// one (a hold that names none is in whatever single asset the node's channels
// hold, which is the node's choice and not the swap's).
func (l *clnLNLeg) legAsset() (string, error) {
	if l.assetID != "" {
		a := strings.ToLower(l.assetID)
		if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
			return "", fmt.Errorf("%w: leg asset %q is not a 32-byte hex asset id", ErrLNHoldAsset, l.assetID)
		}
		return a, nil
	}
	var gi struct {
		Network string `json:"network"`
	}
	if err := l.rpc.call(&gi, "getinfo", map[string]interface{}{}); err != nil {
		return "", fmt.Errorf("getinfo: %w", err)
	}
	if networkHasAssets(gi.Network) {
		return "", fmt.Errorf("%w: the node is on %s, where channels hold several assets, and this leg names none; build the leg with the asset id (the Sequence token's too)", ErrLNHoldAsset, gi.Network)
	}
	return "", nil
}

// ReconnectPeers re-establishes the transport connection to any channel peer that
// has dropped. CLN silently tears down a peer's TCP/Noise connection when it goes
// idle (and a peer restart drops it too); a channel with a disconnected peer stays
// open on-chain but cannot route an HTLC, so a maker that re-quotes after a fill
// must reconnect first or the next leg fails to pay. It is best-effort: it lists
// peers and, for each disconnected one, issues `connect id` (CLN redials a known
// address from gossip / the peer's stored addr). Peers with no known address are
// skipped and surfaced in the returned error, but the ones that could reconnect
// still do. Returns the count reconnected. Idempotent — connecting an
// already-connected peer is a no-op — so it is safe to call before every re-quote.
func (l *clnLNLeg) ReconnectPeers() (int, error) {
	var res struct {
		Peers []struct {
			ID        string `json:"id"`
			Connected bool   `json:"connected"`
		} `json:"peers"`
	}
	if err := l.rpc.call(&res, "listpeers", map[string]interface{}{}); err != nil {
		return 0, fmt.Errorf("listpeers: %w", err)
	}
	reconnected := 0
	var firstErr error
	for _, p := range res.Peers {
		if p.Connected {
			continue
		}
		if err := l.rpc.call(nil, "connect", map[string]interface{}{"id": p.ID}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("connect %s: %w", p.ID, err)
			}
			continue
		}
		reconnected++
	}
	return reconnected, firstErr
}

// decodedInvoice is the subset of CLN's `decode` output the legs act on.
type decodedInvoice struct {
	Type        string `json:"type"`
	Valid       bool   `json:"valid"`
	PaymentHash string `json:"payment_hash"`
	AmountMsat  uint64 `json:"amount_msat"`
	// Only for the unannounced-channel fallback; absent fields simply disable it.
	Payee         string `json:"payee"`
	PaymentSecret string `json:"payment_secret"`
	MinFinalCltv  uint32 `json:"min_final_cltv_expiry"`
}

// decode runs CLN's `decode` (the older `decodepay` is not on all builds).
func (l *clnLNLeg) decode(bolt11 string) (*decodedInvoice, error) {
	var dec decodedInvoice
	if err := l.rpc.call(&dec, "decode", map[string]interface{}{"string": bolt11}); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !dec.Valid {
		return nil, fmt.Errorf("%w: invoice does not decode as valid (%s)", ErrLNLegInvalid, dec.Type)
	}
	return &dec, nil
}

// InvoiceMinFinalCltv returns the final-hop timelock the invoice demands, in
// blocks of this leg's chain. A payer's route adds its hops' deltas on top.
func (l *clnLNLeg) InvoiceMinFinalCltv(bolt11 string) (uint32, error) {
	dec, err := l.decode(bolt11)
	if err != nil {
		return 0, err
	}
	if dec.MinFinalCltv == 0 {
		return 18, nil // the BOLT11 default when the field is absent
	}
	return dec.MinFinalCltv, nil
}

func (l *clnLNLeg) Pay(bolt11 string, wantHash []byte, amountMsat uint64) ([]byte, error) {
	return l.pay(bolt11, wantHash, amountMsat, 0)
}

// PayCapped is Pay with an upper bound on the route's total timelock, in blocks of
// this leg's chain: CLN refuses any route whose delay exceeds it (maxdelay), and
// the direct-hop fallback is refused when the invoice's own final delay does not
// fit. A maker pays its outgoing leg against an incoming hold that expires at a
// known height; a route that outlasts that hold lets a counterparty who holds the
// outgoing HTLC be paid after the incoming one was failed back.
func (l *clnLNLeg) PayCapped(bolt11 string, wantHash []byte, amountMsat uint64, maxDelay uint32) ([]byte, error) {
	if maxDelay == 0 {
		return nil, fmt.Errorf("%w: zero maxdelay", ErrCltvUncapped)
	}
	return l.pay(bolt11, wantHash, amountMsat, maxDelay)
}

func (l *clnLNLeg) pay(bolt11 string, wantHash []byte, amountMsat uint64, maxDelay uint32) ([]byte, error) {
	// Decode first so we never pay an invoice whose hash is not the swap secret,
	// and to sanity-check the amount.
	dec, err := l.decode(bolt11)
	if err != nil {
		return nil, err
	}
	if !hexEq(dec.PaymentHash, wantHash) {
		return nil, fmt.Errorf("%w: invoice payment_hash %s != swap hash %x", ErrLNLegInvalid, dec.PaymentHash, wantHash)
	}
	if amountMsat == 0 && dec.AmountMsat == 0 {
		return nil, fmt.Errorf("%w: amountless invoice with no quoted amount", ErrLNLegInvalid)
	}
	if amountMsat != 0 && dec.AmountMsat != 0 && dec.AmountMsat != amountMsat {
		return nil, fmt.Errorf("%w: invoice amount %d msat != quoted %d", ErrLNLegInvalid, dec.AmountMsat, amountMsat)
	}
	minFinal := dec.MinFinalCltv
	if minFinal == 0 {
		minFinal = 18
	}
	if maxDelay != 0 && minFinal > maxDelay {
		return nil, fmt.Errorf("%w: invoice min_final_cltv %d exceeds the %d-block cap", ErrLNLegInvalid, minFinal, maxDelay)
	}
	var res struct {
		Status       string `json:"status"`
		PaymentPre   string `json:"payment_preimage"`
		AmountSentMs uint64 `json:"amount_sent_msat"`
	}
	// `pay` takes the invoice as `invstring` (renamed from `bolt11` to also cover
	// bolt12); pass `amount_msat` only for an amountless invoice.
	params := map[string]interface{}{"invstring": bolt11}
	if amountMsat != 0 && dec.AmountMsat == 0 {
		params["amount_msat"] = amountMsat // amountless invoice
	}
	if l.assetID != "" {
		params["asset"] = l.assetID // route in this leg's Sequentia asset (Step 1)
	}
	if maxDelay != 0 {
		params["maxdelay"] = maxDelay
	}
	if err := l.rpc.call(&res, "pay", params); err != nil {
		// A SIGNER'S DECLINE IS FINAL, AND NOTHING WAS SENT. `pay` asks the signer
		// (preapproveinvoice) before it offers any HTLC; on a keyless node the device
		// declines a payment over its limit for the asset. The direct-hop fallback
		// below would only ask again and be declined again, so report it as it is.
		if isDeclined(err) {
			return nil, fmt.Errorf("%w: pay: %v (on a keyless node this is the device's payment limit for the asset)", ErrLNPayDeclined, err)
		}
		// AN ERROR FROM `pay` IS NOT A FAILED PAYMENT. The socket read is deadline-bound
		// while `pay` legitimately runs longer (a retrying route, an HTLC held in flight),
		// and CLN itself answers code 200 for "timed out, still in progress". Treating
		// either as failure made the pure-LN maker cancel the taker's incoming hold while
		// its own outgoing HTLC was still live — and then that HTLC settled: asset paid,
		// nothing received. Resolve the payment's real state from the node before
		// deciding anything; only a payment the node has recorded as failed (or never
		// recorded) goes on to the fallback below.
		switch pre, state, rerr := l.resolvePay(wantHash, payResolveWait); state {
		case payComplete:
			return pre, nil
		case payPending:
			return nil, fmt.Errorf("%w: pay: %v (%v)", ErrLNPayUnresolved, err, rerr)
		}
		// UNANNOUNCED PAYEE FALLBACK. `pay` plans from the gossip graph plus the invoice's
		// routehints, so it cannot reach a payee whose only channels are PRIVATE -- it gives up with
		// "not reachable directly and all routehints were unusable" even when that payee is our own
		// direct peer. Every wallet-side node here is exactly that: JIT channels, unannounced.
		//
		// This broke the whole pure-LN rail. The maker registered its hold correctly, then failed to
		// pay the taker's asset invoice and cancelled the hold, so the swap ended with the taker's
		// payment refused and nothing said about why. Reproduced four times running against the same
		// taker node before the maker log made it visible.
		//
		// PayHash already solves this for bare-hash payments: asset-aware getroute, falling back to a
		// single hop over our own channel to a directly-connected payee. Paying a BOLT11 by its own
		// hash + payment_secret to its payee is the same payment, so reuse it rather than growing a
		// second router. The hash was already checked against the swap H above, and PayHash re-verifies
		// that the revealed preimage hashes to it.
		payee, secret, amt, ok := payFallbackPlan(dec.Payee, dec.PaymentSecret, dec.AmountMsat, amountMsat)
		if !ok {
			return nil, fmt.Errorf("pay: %w", err)
		}
		// The direct hop uses exactly the invoice's final delay, already checked
		// against the cap above.
		pre, perr := l.PayHash(payee, wantHash, amt, minFinal, secret)
		if perr != nil {
			return nil, fmt.Errorf("pay: %w (direct-hop fallback: %v)", err, perr)
		}
		return pre, nil
	}
	if res.Status != "complete" {
		return nil, fmt.Errorf("%w: pay status %q", ErrLNLegInvalid, res.Status)
	}
	pre, err := hex.DecodeString(res.PaymentPre)
	if err != nil {
		return nil, fmt.Errorf("bad preimage hex: %w", err)
	}
	// Defence in depth: the revealed preimage must actually hash to wantHash.
	if !hashEqualsPreimage(wantHash, pre) {
		return nil, fmt.Errorf("%w: revealed preimage does not hash to the swap H", ErrLNLegInvalid)
	}
	return pre, nil
}

// PayHash pays amountMsat to destNodeID for a BARE payment_hash, with NO BOLT11:
// the payee holds the hash via a hold plugin (there is no invoice on the payee).
// It routes in this leg's asset (getroute), sends the HTLC (sendpay), blocks
// until the payee settles (waitsendpay), and returns the revealed preimage.
//
// This is the taker's primitive for paying the maker's incoming hold leg in a
// pure-LN swap. paymentSecret is placed in the final-hop onion TLV; the payee's
// hold plugin resolves regardless of it, so an agreed-or-arbitrary value works
// (the maker may convey an expected secret over the courier). finalCltv is the
// final-hop cltv delta (0 => a small default).
// directHop builds a single-hop sendpay route over our OWN (possibly private/
// unannounced) channel to a directly-connected payee — the payee is the final hop,
// so it receives amountMsat with no routing fee. Used when getroute can't see the
// channel (private + asset-aware routing uses only the gossip graph). A channel
// that names another asset than the leg's is never used: listpeerchannels names a
// channel's asset in channel_asset, except for the Sequence token's channels.
func (l *clnLNLeg) directHop(destNodeID string, amountMsat uint64, finalCltv uint32) (json.RawMessage, error) {
	var res struct {
		Channels []struct {
			PeerID         string `json:"peer_id"`
			State          string `json:"state"`
			ShortChannelID string `json:"short_channel_id"`
			Direction      int    `json:"direction"`
			SpendableMsat  uint64 `json:"spendable_msat"`
			ChannelAsset   string `json:"channel_asset"`
		} `json:"channels"`
	}
	if err := l.rpc.call(&res, "listpeerchannels", map[string]interface{}{"id": destNodeID}); err != nil {
		return nil, fmt.Errorf("listpeerchannels: %w", err)
	}
	delay := finalCltv
	if delay == 0 {
		delay = 18
	}
	for _, c := range res.Channels {
		if l.assetID != "" && c.ChannelAsset != "" && !strings.EqualFold(c.ChannelAsset, l.assetID) {
			continue // a channel in another asset
		}
		if c.PeerID == destNodeID && c.State == "CHANNELD_NORMAL" && c.ShortChannelID != "" && c.SpendableMsat >= amountMsat {
			hop := map[string]interface{}{
				"id": destNodeID, "channel": c.ShortChannelID, "direction": c.Direction,
				"amount_msat": amountMsat, "delay": delay, "style": "tlv",
			}
			return json.Marshal(hop)
		}
	}
	return nil, fmt.Errorf("no direct CHANNELD_NORMAL channel to %s with >= %d msat spendable", destNodeID, amountMsat)
}

// payState is what the node knows about an outgoing payment by hash.
type payState int

const (
	payUnknown  payState = iota // no record, or failed: nothing is in flight
	payPending                  // an HTLC is still out; the outcome is undecided
	payComplete                 // settled; the preimage is known
)

// payResolveWait bounds how long Pay waits for a pending outgoing payment to
// resolve after the `pay` call itself returned an error. Past it the payment is
// reported as unresolved rather than failed.
var payResolveWait = 10 * time.Minute

// resolvePay reads the outgoing payment's state from listpays, waiting while it is
// pending up to the given bound. A complete payment yields its preimage (verified
// against the hash); a failed or unknown one yields payUnknown.
func (l *clnLNLeg) resolvePay(paymentHash []byte, wait time.Duration) ([]byte, payState, error) {
	phHex := hex.EncodeToString(paymentHash)
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		var res struct {
			Pays []struct {
				Status   string `json:"status"`
				Preimage string `json:"preimage"`
			} `json:"pays"`
		}
		rpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: l.rpc.timeout}
		lastErr = rpc.call(&res, "listpays", map[string]interface{}{"payment_hash": phHex})
		pending := false
		if lastErr == nil {
			for _, p := range res.Pays {
				switch p.Status {
				case "complete":
					pre, err := hex.DecodeString(p.Preimage)
					if err != nil || !hashEqualsPreimage(paymentHash, pre) {
						return nil, payComplete, fmt.Errorf("%w: listpays preimage does not hash to the payment hash", ErrLNLegInvalid)
					}
					return pre, payComplete, nil
				case "pending":
					pending = true
				}
			}
		}
		if !pending && lastErr == nil {
			return nil, payUnknown, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, payPending, fmt.Errorf("listpays: %w", lastErr)
			}
			return nil, payPending, nil
		}
		time.Sleep(5 * time.Second)
	}
}

func (l *clnLNLeg) PayHash(destNodeID string, paymentHash []byte, amountMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error) {
	phHex := hex.EncodeToString(paymentHash)

	// 1. Route to the destination in this leg's asset.
	rparams := map[string]interface{}{
		"id":          destNodeID,
		"amount_msat": amountMsat,
		"riskfactor":  10,
	}
	if finalCltv != 0 {
		rparams["cltv"] = finalCltv
	}
	if l.assetID != "" {
		rparams["asset"] = l.assetID
	}
	var route struct {
		Route []json.RawMessage `json:"route"`
	}
	rerr := l.rpc.call(&route, "getroute", rparams)
	if rerr != nil || len(route.Route) == 0 {
		// Asset-aware getroute uses only the gossip graph, so it misses an UNANNOUNCED
		// (private) channel to a directly-connected payee. Fall back to a single-hop
		// route over our own channel to the destination.
		hop, derr := l.directHop(destNodeID, amountMsat, finalCltv)
		if derr != nil {
			if rerr != nil {
				return nil, fmt.Errorf("getroute: %w (and no direct channel: %v)", rerr, derr)
			}
			return nil, fmt.Errorf("%w: no route to %s (asset %q): %v", ErrLNLegInvalid, destNodeID, l.assetID, derr)
		}
		route.Route = []json.RawMessage{hop}
	}

	// 2. Ask the node's signer to approve the payment. On a keyless node the
	//    device signs a commitment that adds an HTLC we offer only for a payment
	//    hash it approved (`pay` and `keysend` ask it themselves; a bare
	//    `sendpay` does not), and it approves only within its payment limit for
	//    the asset. Without this the device refuses the commitment, the payee is
	//    never paid, and the channel has no owner until the peer reconnects. A
	//    decline here comes before any HTLC exists, so nothing was sent. On a node
	//    with its own hsm_secret the signer approves every payment.
	if err := l.preapprove(destNodeID, phHex, amountMsat); err != nil {
		return nil, err
	}

	// 3. Send the HTLC to the bare hash along that route.
	sparams := map[string]interface{}{
		"route":        route.Route,
		"payment_hash": phHex,
		"amount_msat":  amountMsat,
	}
	if len(paymentSecret) > 0 {
		sparams["payment_secret"] = hex.EncodeToString(paymentSecret)
	}
	if err := l.rpc.call(nil, "sendpay", sparams); err != nil {
		// "FIRST PEER NOT READY" IS A DROPPED TRANSPORT, NOT A DEAD ROUTE.
		//
		// CLN keeps the channel but tears down the peer's TCP/Noise connection when it goes idle (and a
		// peer restart drops it too). The channel is fine, the balance is fine — the first hop simply has
		// no live connection, and sendpay refuses with WIRE_TEMPORARY_CHANNEL_FAILURE.
		//
		// ReconnectPeers already existed for exactly this and was called from nowhere, so a wallet whose
		// node had been idle failed its swap outright: "pure-LN swap ended: pay hold: sendpay: rpc error
		// 204: failed: WIRE_TEMPORARY_CHANNEL_FAILURE (First peer not ready)". Reconnect and try once
		// more; if it still refuses, report the ORIGINAL error, since that is the one that describes the
		// payment rather than the recovery.
		if isPeerNotReady(err) {
			if _, rerr := l.ReconnectPeers(); rerr == nil {
				if err2 := l.rpc.call(nil, "sendpay", sparams); err2 == nil {
					goto sent
				}
			}
		}
		return nil, fmt.Errorf("sendpay: %w", err)
	}
sent:

	// 4. Block until the payee resolves it (settle => complete, cancel => fail).
	//    A HODL payee holds the HTLC until its device settles, which can far
	//    outlast one socket read (seen live: the mixed same-chain maker died
	//    here at the 120s read deadline while the payment sat legitimately
	//    'pending', orphaning the swap). Ask waitsendpay itself to time-box
	//    each wait (CLN code 200 = still pending) and LOOP on a fresh
	//    connection until the payment RESOLVES: abandoning while pending is
	//    never safe — if it settles later the node holds P (listsendpays) but
	//    nothing would claim the on-chain leg with it.
	var res struct {
		Status     string `json:"status"`
		PaymentPre string `json:"payment_preimage"`
	}
	for {
		wrpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: l.rpc.timeout}
		err := wrpc.call(&res, "waitsendpay", map[string]interface{}{"payment_hash": phHex, "timeout": 60})
		if err == nil {
			break
		}
		if waitsendpayStillPending(err) {
			continue
		}
		return nil, fmt.Errorf("waitsendpay: %w", err)
	}
	if res.Status != "complete" {
		return nil, fmt.Errorf("%w: sendpay status %q", ErrLNLegInvalid, res.Status)
	}
	pre, err := hex.DecodeString(res.PaymentPre)
	if err != nil {
		return nil, fmt.Errorf("bad preimage hex: %w", err)
	}
	if !hashEqualsPreimage(paymentHash, pre) {
		return nil, fmt.Errorf("%w: revealed preimage does not hash to paymentHash", ErrLNLegInvalid)
	}
	return pre, nil
}

// preapprove asks the node's signer to approve paying amountMsat to destNodeID on
// paymentHash (`preapprovekeysend`), before a bare-hash `sendpay`. A decline is
// ErrLNPayDeclined. A node without the command (a Lightning implementation that
// predates it, so its signer checks nothing) is let through.
func (l *clnLNLeg) preapprove(destNodeID, paymentHashHex string, amountMsat uint64) error {
	err := l.rpc.call(nil, "preapprovekeysend", map[string]interface{}{
		"destination":  destNodeID,
		"payment_hash": paymentHashHex,
		"amount_msat":  amountMsat,
	})
	if err == nil || isUnknownMethod(err) {
		return nil
	}
	if isDeclined(err) {
		return fmt.Errorf("%w: %d msat to %s on hash %s, in asset %q: %v (on a keyless node this is the device's payment limit for the asset)",
			ErrLNPayDeclined, amountMsat, destNodeID, paymentHashHex, l.assetID, err)
	}
	return fmt.Errorf("preapprovekeysend: %w", err)
}

// isDeclined reports whether a CLN error is a signer's refusal to approve a
// payment: PAY_INVOICE_PREAPPROVAL_DECLINED (213) from `preapproveinvoice` or
// `pay`, PAY_KEYSEND_PREAPPROVAL_DECLINED (214) from `preapprovekeysend` or
// `keysend`.
func isDeclined(err error) bool {
	var re *rpcErr
	if errors.As(err, &re) {
		return re.Code == 213 || re.Code == 214
	}
	return false
}

func (l *clnLNLeg) CreateHoldInvoice(paymentHash []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (string, error) {
	// Drives the holdinvoice-seq plugin (create by payment_hash; the plugin owns
	// the hold via the htlc_accepted hook). On a Sequentia node the hold names
	// this leg's asset, so the plugin holds only HTLCs that arrive in it and
	// refuses any other; a leg that names no asset there makes no hold at all.
	asset, err := l.legAsset()
	if err != nil {
		return "", fmt.Errorf("holdinvoice: %w", err)
	}
	var res struct {
		Bolt11 string `json:"bolt11"`
		Asset  string `json:"asset"`
	}
	params := map[string]interface{}{
		"payment_hash": hex.EncodeToString(paymentHash),
		"amount_msat":  amountMsat,
		"label":        label,
		"description":  description,
	}
	if cltvExpiry != 0 {
		params["cltv"] = cltvExpiry
	}
	if asset != "" {
		params["asset"] = asset
	}
	if err := l.rpc.call(&res, "holdinvoice", params); err != nil {
		return "", fmt.Errorf("holdinvoice (plugin present?): %w", err)
	}
	// The plugin answers with the asset it registered the hold in. One that does
	// not (it predates assets and ignored the parameter) holds any asset, so it
	// cannot be trusted with this leg.
	if !strings.EqualFold(res.Asset, asset) {
		_ = l.CancelHold(paymentHash)
		return "", fmt.Errorf("%w: holdinvoice registered the hold in asset %q, not %q (is holdinvoice-seq current?)", ErrLNHoldAsset, res.Asset, asset)
	}
	return res.Bolt11, nil
}

func (l *clnLNLeg) WaitHeld(paymentHash []byte, timeout time.Duration) (uint64, error) {
	info, err := l.WaitHeldInfo(paymentHash, timeout)
	if err != nil {
		return 0, err
	}
	return info.ReceivedMsat, nil
}

// HeldInfo is what the holder learns about a held incoming payment: what actually
// arrived (the plugin's received sum, not the registered expectation) and the
// payer-chosen absolute expiry of the earliest held HTLC, with the tip it is
// measured against. CltvExpiry is 0 when the plugin predates the field.
type HeldInfo struct {
	ReceivedMsat uint64
	CltvExpiry   uint32
	Tip          uint32
	// Asset is the asset the hold is in (lower-case hex), "" on a network with
	// no assets. WaitHeldInfo returns a hold only when it is the leg's asset.
	Asset string
}

// WaitHeldInfo blocks until the hold on paymentHash is accepted and returns what
// was held. It asks the plugin to answer the moment the HTLC lands
// (holdinvoicewait, each call bounded so the socket read never outlives it);
// against a plugin without that method it polls holdinvoicelookup instead,
// tightly for the first second and then once a second.
//
// A hold that is accepted in another asset than the leg's, or that names no asset
// on a network with assets, is refused with ErrLNHoldAsset: the caller then
// cancels it and never settles, so the preimage is never revealed for a payment
// in the wrong asset.
func (l *clnLNLeg) WaitHeldInfo(paymentHash []byte, timeout time.Duration) (HeldInfo, error) {
	want, err := l.legAsset()
	if err != nil {
		return HeldInfo{}, err
	}
	start := time.Now()
	deadline := start.Add(timeout)
	phHex := hex.EncodeToString(paymentHash)
	canWait := true
	for {
		var res struct {
			State        string `json:"state"`
			AmountMsat   uint64 `json:"amount_msat"`
			ReceivedMsat uint64 `json:"received_msat"`
			CltvExpiry   uint32 `json:"cltv_expiry"`
			Blockheight  uint32 `json:"blockheight"`
			Asset        string `json:"asset"`
		}
		var err error
		if canWait {
			secs := int(time.Until(deadline)/time.Second) + 1
			if secs > 60 {
				secs = 60
			}
			wrpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: time.Duration(secs+10) * time.Second}
			err = wrpc.call(&res, "holdinvoicewait",
				map[string]interface{}{"payment_hash": phHex, "timeout": secs})
			if err != nil && isUnknownMethod(err) {
				canWait = false
				continue
			}
		} else {
			err = l.rpc.call(&res, "holdinvoicelookup",
				map[string]interface{}{"payment_hash": phHex})
		}
		if err == nil && res.State == "accepted" {
			if got := strings.ToLower(res.Asset); got != want {
				return HeldInfo{}, fmt.Errorf("%w: the hold on %s is accepted in asset %q, this leg is in %q; refusing it (the preimage stays withheld)", ErrLNHoldAsset, phHex, got, want)
			}
			info := HeldInfo{ReceivedMsat: res.ReceivedMsat, CltvExpiry: res.CltvExpiry, Tip: res.Blockheight, Asset: want}
			if info.ReceivedMsat == 0 {
				info.ReceivedMsat = res.AmountMsat // a plugin without the received field
			}
			if info.Tip == 0 { // a plugin without the blockheight field
				var gi struct {
					Blockheight uint32 `json:"blockheight"`
				}
				if err := l.rpc.call(&gi, "getinfo", map[string]interface{}{}); err != nil {
					return info, fmt.Errorf("getinfo: %w", err)
				}
				info.Tip = gi.Blockheight
			}
			return info, nil
		}
		if time.Now().After(deadline) {
			return HeldInfo{}, fmt.Errorf("%w: hold invoice not accepted within %s (last err: %v)", ErrLNLegTimeout, timeout, err)
		}
		if canWait {
			continue
		}
		if time.Since(start) < time.Second {
			time.Sleep(50 * time.Millisecond)
		} else {
			time.Sleep(time.Second)
		}
	}
}

// isUnknownMethod reports whether an RPC error means the node has no such
// method: JSON-RPC's -32601, which CLN answers for a command no plugin provides.
func isUnknownMethod(err error) bool {
	var re *rpcErr
	if errors.As(err, &re) {
		return re.Code == -32601
	}
	return false
}

func (l *clnLNLeg) SettleHold(paymentHash, preimage []byte) error {
	if len(preimage) != PreimageLen {
		return fmt.Errorf("%w: %d bytes", ErrBadPreimageLen, len(preimage))
	}
	if !hashEqualsPreimage(paymentHash, preimage) {
		return fmt.Errorf("%w: preimage does not hash to the invoice payment_hash", ErrLNLegInvalid)
	}
	// The hold plugin settles by revealing the preimage (the maker does not know
	// P until it learns it from the outgoing leg), so pass it explicitly.
	return l.rpc.call(nil, "holdinvoicesettle", map[string]interface{}{
		"payment_hash": hex.EncodeToString(paymentHash),
		"preimage":     hex.EncodeToString(preimage),
	})
}

func (l *clnLNLeg) CancelHold(paymentHash []byte) error {
	return l.rpc.call(nil, "holdinvoicecancel",
		map[string]interface{}{"payment_hash": hex.EncodeToString(paymentHash)})
}

// localScids lists the short_channel_ids of this node's LIVE channels, for
// forcing into an invoice's routehints. On a slow-confirming chain (testnet4)
// a PUBLIC channel can stay un-gossiped for hours (announcement needs depth 6),
// during which a payer relying on gossip cannot route even to a DIRECT peer
// ("Ran out of routes to try"). Exposing the channels explicitly makes every
// invoice self-routing regardless of announcement state.
func (l *clnLNLeg) localScids() []string {
	var res struct {
		Channels []struct {
			State string `json:"state"`
			Scid  string `json:"short_channel_id"`
		} `json:"channels"`
	}
	if err := l.rpc.call(&res, "listpeerchannels", map[string]interface{}{}); err != nil {
		return nil
	}
	var scids []string
	for _, c := range res.Channels {
		if c.State == "CHANNELD_NORMAL" && c.Scid != "" {
			scids = append(scids, c.Scid)
		}
	}
	return scids
}

func (l *clnLNLeg) CreateInvoice(preimage []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (string, error) {
	// The invoice names this leg's asset, so the node takes a payment for it only
	// in that asset; a leg that names no asset on a Sequentia node makes none.
	asset, err := l.legAsset()
	if err != nil {
		return "", fmt.Errorf("invoice: %w", err)
	}
	params := map[string]interface{}{
		"amount_msat": amountMsat,
		"label":       label,
		"description": description,
		"preimage":    hex.EncodeToString(preimage),
	}
	if asset != "" {
		params["asset"] = asset
	}
	if scids := l.localScids(); len(scids) > 0 {
		params["exposeprivatechannels"] = scids
	}
	if cltvExpiry != 0 {
		params["cltv"] = cltvExpiry
	}
	var res struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
	}
	if err := l.rpc.call(&res, "invoice", params); err != nil {
		// The explicit exposeprivatechannels list gets STRICTER validation than
		// CLN's own hint selection: on a leaf node whose only channels are
		// private, CLN rejects the whole list ("None of those hints were
		// suitable local channels", error 902) even when a NORMAL, connected,
		// sufficient-capacity channel is on it — seen live on a hosted per-user
		// node, killing the swap before any HTLC existed. CLN's default
		// selection only WARNS when it cannot hint, so retry without the list.
		_, had := params["exposeprivatechannels"]
		if !had || !strings.Contains(err.Error(), "hints were suitable") {
			return "", fmt.Errorf("invoice: %w", err)
		}
		delete(params, "exposeprivatechannels")
		if err2 := l.rpc.call(&res, "invoice", params); err2 != nil {
			return "", fmt.Errorf("invoice: %w", err2)
		}
	}
	// The created invoice's hash MUST equal SHA256(P) — otherwise the SEQ leg
	// (gated on the same H) and the LN leg would not be bound by one secret.
	want := sha256.Sum256(preimage)
	if !hexEq(res.PaymentHash, want[:]) {
		return "", fmt.Errorf("%w: created invoice hash %s != SHA256(P) %x", ErrLNLegInvalid, res.PaymentHash, want[:])
	}
	return res.Bolt11, nil
}

func (l *clnLNLeg) WaitInvoicePaid(label string, timeout time.Duration) (uint64, error) {
	// waitinvoice blocks server-side until the invoice is paid or expires; use a
	// dedicated connection whose deadline is the caller's timeout.
	rpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: timeout}
	var res struct {
		Status             string `json:"status"`
		AmountReceivedMsat uint64 `json:"amount_received_msat"`
		AmountMsat         uint64 `json:"amount_msat"`
	}
	if err := rpc.call(&res, "waitinvoice", map[string]interface{}{"label": label}); err != nil {
		return 0, fmt.Errorf("waitinvoice: %w", err)
	}
	if res.Status != "paid" {
		return 0, fmt.Errorf("%w: invoice status %q (not paid)", ErrLNLegInvalid, res.Status)
	}
	if res.AmountReceivedMsat != 0 {
		return res.AmountReceivedMsat, nil
	}
	return res.AmountMsat, nil
}

// WaitPaidByHash polls listinvoices for an invoice with the given payment_hash
// until it is "paid" (or the deadline). Unlike WaitInvoicePaid it needs no label
// and never touches the preimage — used by the sub-asset external-invoice mode,
// where a DEVICE created the invoice with its own preimage and the driver only
// learns that the payment arrived (settlement is signaled by "paid", the preimage
// stays device-held).
func (l *clnLNLeg) WaitPaidByHash(paymentHash []byte, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	hashHex := hex.EncodeToString(paymentHash)
	for {
		var res struct {
			Invoices []struct {
				Status             string `json:"status"`
				AmountReceivedMsat uint64 `json:"amount_received_msat"`
				AmountMsat         uint64 `json:"amount_msat"`
			} `json:"invoices"`
		}
		err := l.rpc.call(&res, "listinvoices", map[string]interface{}{"payment_hash": hashHex})
		if err == nil {
			for _, inv := range res.Invoices {
				if inv.Status == "paid" {
					if inv.AmountReceivedMsat != 0 {
						return inv.AmountReceivedMsat, nil
					}
					return inv.AmountMsat, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%w: invoice %s not paid within %s (last err: %v)", ErrLNLegTimeout, hashHex[:12], timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// --- minimal CLN lightning-rpc client (unix socket, JSON-RPC 2.0) -----------

type lnRPC struct {
	socketPath string
	timeout    time.Duration
}

func (c *lnRPC) call(out interface{}, method string, params interface{}) error {
	conn, err := net.DialTimeout("unix", c.socketPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial lightning-rpc %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "seqob-xchain",
		"method":  method,
		"params":  params,
	}
	// Encode WITHOUT Go's default HTML escaping: it turns <, >, & into \uXXXX
	// escapes, and CLN's JSON parser rejects \u escapes in string params
	// (e.g. a description containing "->"). A JSON-RPC client must send literal
	// <>&.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return err
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}

	// Read JSON objects until we get the response with our id (CLN may emit
	// notifications on the same socket).
	dec := json.NewDecoder(conn)
	for {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcErr         `json:"error"`
		}
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("%s: read response: %w", method, err)
		}
		if len(r.ID) == 0 || string(r.ID) == "null" {
			continue // a notification, not our reply
		}
		if r.Error != nil {
			return fmt.Errorf("%s: %w", method, r.Error)
		}
		if out != nil && len(r.Result) > 0 {
			if err := json.Unmarshal(r.Result, out); err != nil {
				return fmt.Errorf("%s: unmarshal result: %w", method, err)
			}
		}
		return nil
	}
}

// waitsendpayStillPending reports whether a waitsendpay error means the payment
// is STILL PENDING (so the caller should keep waiting on a fresh connection):
// CLN's own wait timeout (code 200), or a transport-level timeout/read error on
// the blocking socket. Anything else is a real resolution (failure) and must
// surface.
func waitsendpayStillPending(err error) bool {
	var re *rpcErr
	if errors.As(err, &re) {
		return re.Code == 200 // "Timed out while waiting"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// The deadline can also surface as a wrapped plain read error.
	return strings.Contains(err.Error(), "i/o timeout")
}

// payFallbackPlan decides whether a failed `pay` can be retried as a direct-hop PayHash, and
// with what arguments. Split out from Pay so the decision is testable without a live node.
//
// It is deliberately CONSERVATIVE: without a payee and a payment_secret there is no well-formed
// payment to send, so the original `pay` error is reported unchanged rather than replaced by a
// second, more confusing one from a request that could never have worked.
//
// The amount follows the invoice: a quoted amount wins (it was already checked against the
// invoice above), and an amountless invoice falls back to the invoice's own amount. Sending 0
// would be silently wrong, so both being absent disables the fallback.
func payFallbackPlan(payee, paymentSecretHex string, invoiceMsat, quotedMsat uint64) (string, []byte, uint64, bool) {
	if payee == "" || paymentSecretHex == "" {
		return "", nil, 0, false
	}
	secret, err := hex.DecodeString(paymentSecretHex)
	if err != nil || len(secret) == 0 {
		return "", nil, 0, false
	}
	amt := quotedMsat
	if amt == 0 {
		amt = invoiceMsat
	}
	if amt == 0 {
		return "", nil, 0, false
	}
	return payee, secret, amt, true
}

// isPeerNotReady reports whether a CLN error is the "the first hop's transport is down" case rather
// than a genuine routing/liquidity failure. CLN keeps the channel and the balance in this state; only
// the peer's TCP/Noise connection is gone, which a reconnect fixes.
//
// Deliberately narrow: it only triggers a reconnect-and-retry, and retrying a payment for the wrong
// reason is exactly the kind of thing that should not be loose.
func isPeerNotReady(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "first peer not ready") ||
		(strings.Contains(m, "temporary_channel_failure") && strings.Contains(m, "peer"))
}
