package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xdriver_pureln.go runs the PURE-LN swap handshake (xcourier_pureln.go) over the
// opaque relay courier, settling with the proven pkg/xchain PureLNSwap engine.
// BOTH legs are off-chain Lightning, stitched by one shared secret — there is NO
// on-chain leg and NO anchor gate, so this is the simplest of the Lightning
// drivers. It is direction-parameterized because BUY and SELL are genuinely
// symmetric: the TAKER always originates the secret P, mints an invoice on its
// INCOMING leg (the leg it receives value on), and pays into the maker's hold on
// its OUTGOING leg; the MAKER always holds its incoming leg, then pays the taker's
// invoice on its outgoing leg (learning P) and settles the hold.
//
//	BUY  (LnBTCLNForAssetLN): taker receives asset, pays BTC -> maker holds BTC, pays asset
//	SELL (LnAssetLNForBTCLN): taker receives BTC, pays asset -> maker holds asset, pays BTC

// PlnDirection selects which leg the maker holds. The neutral driver derives the
// hold/pay amounts and picks the live ops from this.
type PlnDirection int

const (
	PlnBuy  PlnDirection = iota // taker buys the asset with BTC; maker holds BTC, pays the asset
	PlnSell                     // taker sells the asset for BTC; maker holds the asset, pays BTC
)

// PlnMakerOps is the narrow settlement seam the maker driver runs against. The
// method shapes are direction-neutral; the live ops differ per direction.
type PlnMakerOps interface {
	// HoldNodeID returns the maker's incoming (held) leg node id — advertised so
	// the taker can pay the hold by bare hash.
	HoldNodeID() (string, error)
	// RegisterHold registers the incoming hold on h for holdAmtMsat.
	RegisterHold(h []byte, holdAmtMsat uint64) error
	// Fulfill waits for the held incoming leg (at least holdAmtMsat), pays the
	// taker's invoice for payAmtMsat (learning P) with its timelock capped below the
	// hold's expiry, and settles the hold. Cancels on failure. Returns P.
	Fulfill(h []byte, takerInvoice string, payAmtMsat, holdAmtMsat uint64, holdTimeout time.Duration) ([]byte, error)
	// HoldCltvFor is the final-hop timelock (incoming-leg blocks) the taker must pay
	// the hold with for Fulfill to be able to pay takerInvoice safely. 0 = unknown
	// (a test double); the taker then pays with its own default.
	HoldCltvFor(takerInvoice string, holdTimeout time.Duration) (uint32, error)
}

// PlnTakerOps is the settlement seam the taker driver runs against.
type PlnTakerOps interface {
	// PrepareInvoice issues the invoice on the taker's INCOMING leg on preimage p
	// for invoiceAmtMsat; returns the invoice and h = SHA256(p).
	PrepareInvoice(p []byte, invoiceAmtMsat uint64) (invoice string, h []byte, err error)
	// PayHold pays the maker's hold by bare hash for holdAmtMsat and blocks until
	// the maker settles, returning the revealed preimage.
	PayHold(h []byte, makerHoldNodeID string, holdAmtMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error)
}

// --- live ops over a real *xchain.PureLNSwap (M3) ---------------------------

// BUY: maker holds the BTC leg and pays the asset leg.
type LivePlnMakerBuyOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnMakerBuyOps) HoldNodeID() (string, error) { return o.Swap.BtcLegNodeID() }
func (o *LivePlnMakerBuyOps) RegisterHold(h []byte, amt uint64) error {
	return o.Swap.MakerRegisterHold(h, amt)
}
func (o *LivePlnMakerBuyOps) Fulfill(h []byte, inv string, amt, hold uint64, to time.Duration) ([]byte, error) {
	return o.Swap.MakerFulfill(h, inv, amt, hold, to)
}
func (o *LivePlnMakerBuyOps) HoldCltvFor(inv string, to time.Duration) (uint32, error) {
	return o.Swap.HoldCltvForBuy(inv, to)
}

type LivePlnTakerBuyOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnTakerBuyOps) PrepareInvoice(p []byte, amt uint64) (string, []byte, error) {
	return o.Swap.PrepareTakerBuy(p, amt)
}
func (o *LivePlnTakerBuyOps) PayHold(h []byte, id string, amt uint64, cltv uint32, secret []byte) ([]byte, error) {
	return o.Swap.RunTakerBuy(h, id, amt, cltv, secret)
}

// SELL: maker holds the asset leg and pays the BTC leg.
type LivePlnMakerSellOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnMakerSellOps) HoldNodeID() (string, error) { return o.Swap.AssetLegNodeID() }
func (o *LivePlnMakerSellOps) RegisterHold(h []byte, amt uint64) error {
	return o.Swap.MakerRegisterHoldSell(h, amt)
}
func (o *LivePlnMakerSellOps) Fulfill(h []byte, inv string, amt, hold uint64, to time.Duration) ([]byte, error) {
	return o.Swap.MakerFulfillSell(h, inv, amt, hold, to)
}
func (o *LivePlnMakerSellOps) HoldCltvFor(inv string, to time.Duration) (uint32, error) {
	return o.Swap.HoldCltvForSell(inv, to)
}

type LivePlnTakerSellOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnTakerSellOps) PrepareInvoice(p []byte, amt uint64) (string, []byte, error) {
	return o.Swap.PrepareTakerSell(p, amt)
}
func (o *LivePlnTakerSellOps) PayHold(h []byte, id string, amt uint64, cltv uint32, secret []byte) ([]byte, error) {
	return o.Swap.RunTakerSell(h, id, amt, cltv, secret)
}

// holdPayAmts splits the offer's BTC/asset amounts into (holdAmt, payAmt) for a
// direction: the maker holds what the taker pays in, and pays the taker's invoice.
func holdPayAmts(dir PlnDirection, btcMsat, seqMsat uint64) (holdAmt, payAmt uint64) {
	if dir == PlnSell {
		return seqMsat, btcMsat // maker holds the asset, pays BTC
	}
	return btcMsat, seqMsat // BUY: maker holds BTC, pays the asset
}

// --- maker -----------------------------------------------------------------

// MakerPlnParams configures RunMakerPureLN. Amounts come from the SIGNED offer.
type MakerPlnParams struct {
	Direction   PlnDirection
	NewMakerOps func() PlnMakerOps // binds the settlement engine for this direction
	Crypter     *Crypter
	// BtcAmtMsat / SeqAmtMsat are the WHOLE signed offer's two sides. They are the
	// MAXIMUM a taker may take, not the amount every lift settles: the taker names
	// its slice in the terms request and this maker sizes both legs to it, which is
	// what lets the pure-LN rail do the partial fills every other rail does.
	BtcAmtMsat  uint64
	SeqAmtMsat  uint64
	HoldTimeout time.Duration // wait for the taker's hold + fulfill (default 2m)
	Timing      XcTiming
	Log         func(format string, args ...interface{})
}

type MakerPlnResult struct {
	HashH    []byte
	Preimage []byte
	Settled  bool

	// FilledSeqMsat / FilledBtcMsat are what THIS lift settled; RemainderSeqMsat is
	// what is left of the offer, which the caller RE-RESTS rather than retiring it.
	FilledSeqMsat    uint64
	FilledBtcMsat    uint64
	RemainderSeqMsat uint64
}

func (p *MakerPlnParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunMakerPureLN executes the pure-LN handshake as the maker: advertise terms
// (incl. its held-leg node id), receive the taker's invoice on H, register the
// incoming hold, and fulfill (wait held -> pay the taker's invoice -> settle).
func RunMakerPureLN(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln maker: NewMakerOps and Crypter are required")
	}
	if p.HoldTimeout <= 0 {
		p.HoldTimeout = 2 * time.Minute
	}
	recv := chanRecv(in)
	res := &MakerPlnResult{}

	// PARTIAL FILLS. The request carries the asset-side slice the taker wants
	// (XcMsg.SeqAmount, the field the cross and submarine rails already use). It
	// used to carry no fields at all, so this maker advertised — and settled — the
	// WHOLE offer on every lift. Pure-LN was the last rail without partials, which
	// is also why its takes could not be presented like the others.
	//
	// Zero (or the whole) keeps the classic whole-offer lift, so an older taker
	// that sends an empty request is unaffected.
	req, err := recvXcType(recv, p.Crypter, XcPlnTermsRequest, p.Timing.TermsReqWait)
	if err != nil {
		return res, err
	}
	takeSeq := req.SeqAmount
	if takeSeq == 0 {
		takeSeq = p.SeqAmtMsat
	}
	if takeSeq > p.SeqAmtMsat {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "requested slice exceeds the offer")
		return res, fmt.Errorf("pureln maker: take %d exceeds the offer's %d", takeSeq, p.SeqAmtMsat)
	}
	// Price the slice from the SIGNED offer's ratio. Round the BTC side UP when we
	// GIVE BTC is wrong — we round in OUR favour, which depends on the direction:
	// on SELL we give the asset and receive BTC, so the BTC we receive ceils; on
	// BUY we give BTC and receive the asset, so the BTC we give floors.
	var takeBtc uint64
	if p.Direction == PlnSell {
		takeBtc = ProportionalBtc(p.BtcAmtMsat, takeSeq, p.SeqAmtMsat)
	} else {
		takeBtc = ProportionalBtcFloor(p.BtcAmtMsat, takeSeq, p.SeqAmtMsat)
	}
	if takeBtc == 0 {
		sendXcFail(p.Crypter, send, "DUST", "that slice prices to zero")
		return res, fmt.Errorf("pureln maker: take %d of %d prices to 0 msat", takeSeq, p.SeqAmtMsat)
	}
	// The taker's own price of the slice, when it sends one, is taken within PlnPriceSlackMsat.
	takeBtc = PlnAgreedBtc(takeBtc, req.BtcAmount)
	res.FilledSeqMsat = takeSeq
	res.FilledBtcMsat = takeBtc
	res.RemainderSeqMsat = p.SeqAmtMsat - takeSeq
	holdAmt, payAmt := holdPayAmts(p.Direction, takeBtc, takeSeq)
	ops := p.NewMakerOps()
	holdID, err := ops.HoldNodeID()
	if err != nil {
		sendXcFail(p.Crypter, send, "maker_node", err.Error())
		return res, fmt.Errorf("maker hold node id: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnTerms, MakerLNNodeID: holdID, BtcAmount: takeBtc, SeqAmount: takeSeq}, p.Crypter, send); err != nil {
		return res, err
	}

	inv, err := recvXcType(recv, p.Crypter, XcPlnAssetInvoice, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(inv.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "bad_hash", "invoice carried a malformed hash")
		return res, fmt.Errorf("pureln maker: bad hash_h %q", inv.HashH)
	}
	res.HashH = hashH
	// The hold's lifetime is whatever the TAKER pays it with, and the outgoing
	// invoice's timelock is whatever the taker minted. Derive the hold timelock that
	// lets us pay that invoice and settle before the hold can expire, and tell the
	// taker; Fulfill then caps the outgoing route against the hold as actually paid.
	holdCltv, err := ops.HoldCltvFor(inv.Bolt11, p.HoldTimeout)
	if err != nil {
		sendXcFail(p.Crypter, send, "invoice", err.Error())
		return res, fmt.Errorf("hold cltv for the taker's invoice: %w", err)
	}
	p.logf("pureln maker: registering hold on H=%s (taker must pay with final cltv >= %d)", inv.HashH[:12], holdCltv)
	if err := ops.RegisterHold(hashH, holdAmt); err != nil {
		sendXcFail(p.Crypter, send, "hold", err.Error())
		return res, fmt.Errorf("register hold: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnHoldReady, HashH: inv.HashH, HoldCltv: holdCltv}, p.Crypter, send); err != nil {
		return res, err
	}

	// Wait for the taker to lock the incoming leg, pay its invoice (learn P), settle.
	pre, err := ops.Fulfill(hashH, inv.Bolt11, payAmt, holdAmt, p.HoldTimeout)
	if err != nil {
		sendXcFail(p.Crypter, send, "fulfill", err.Error())
		return res, fmt.Errorf("fulfill: %w", err)
	}
	res.Preimage = pre
	res.Settled = true
	_ = sendXc(&XcMsg{Type: XcPlnSettled}, p.Crypter, send)
	p.logf("pureln maker: settled, took the incoming leg")
	return res, nil
}

// RunMakerPureLNBuy / RunMakerPureLNSell are the direction-explicit entry points.
func RunMakerPureLNBuy(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Direction = PlnBuy
	return RunMakerPureLN(p, in, send)
}
func RunMakerPureLNSell(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Direction = PlnSell
	return RunMakerPureLN(p, in, send)
}

// --- taker -----------------------------------------------------------------

type TakerPlnParams struct {
	Direction PlnDirection
	Ops       PlnTakerOps
	Crypter   *Crypter
	// BtcAmtMsat / SeqAmtMsat are the WHOLE signed offer's two sides.
	BtcAmtMsat uint64
	SeqAmtMsat uint64
	// TakeSeqMsat is the asset-side slice to take. 0 (or the whole) takes the WHOLE
	// offer, reducing to the classic pure-LN lift. For a partial the BTC side is
	// derived from the signed offer's ratio and required exactly.
	TakeSeqMsat   uint64
	FinalCltv     uint32 // final-hop cltv delta for paying the hold (default 18); raised to what the maker requires
	MaxHoldCltv   uint32 // refuse a maker that requires a longer hold than this (default 1440 blocks of the hold leg's chain)
	PaymentSecret []byte // onion TLV secret; random if nil
	Timing        XcTiming
	Log           func(format string, args ...interface{})
}

type TakerPlnResult struct {
	HashH    []byte
	Preimage []byte

	// FilledSeqMsat / FilledBtcMsat are what THIS lift settled, which for a partial
	// is smaller than the offer the taker matched against.
	FilledSeqMsat uint64
	FilledBtcMsat uint64
}

// RunTakerPureLN executes the pure-LN handshake as the taker: request terms, issue
// an invoice on a fresh P for its incoming leg, hand the maker H + the invoice,
// then pay the maker's hold by bare hash (blocks until the maker settles).
func RunTakerPureLN(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	// Stage stopwatch: every latency hunt so far has been blind guessing over a
	// single opaque duration; these lines make the swap's time attributable.
	start := time.Now()
	stage := func(name string) {
		if p.Log != nil {
			p.Log("STAGE %s +%dms", name, time.Since(start).Milliseconds())
		}
	}
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln taker: Ops and Crypter are required")
	}
	if p.FinalCltv == 0 {
		p.FinalCltv = 18
	}
	if p.MaxHoldCltv == 0 {
		p.MaxHoldCltv = 1440
	}
	// The taker mints an invoice for its INCOMING leg and pays HOLD into the
	// maker on its OUTGOING leg. For BUY it receives the asset (invoice=asset) and
	// pays BTC (hold=BTC); for SELL it is the mirror.
	// PARTIAL FILLS: decide the slice, price it OURSELVES from the signed offer, and
	// name it in the request. The maker sizes both legs to it and re-rests the rest.
	takeSeq := p.TakeSeqMsat
	if takeSeq == 0 || takeSeq > p.SeqAmtMsat {
		takeSeq = p.SeqAmtMsat
	}
	// Mirror of the maker's rounding, so both derive the same number from the same
	// signed offer: the side that GIVES BTC floors it, the side that RECEIVES ceils.
	var takeBtc uint64
	if p.Direction == PlnSell {
		takeBtc = ProportionalBtc(p.BtcAmtMsat, takeSeq, p.SeqAmtMsat)
	} else {
		takeBtc = ProportionalBtcFloor(p.BtcAmtMsat, takeSeq, p.SeqAmtMsat)
	}
	if takeBtc == 0 {
		return nil, fmt.Errorf("%w: take %d of %d prices to 0 msat (dust)", ErrXcBadTerms, takeSeq, p.SeqAmtMsat)
	}
	holdAmt, invoiceAmt := holdPayAmts(p.Direction, takeBtc, takeSeq)
	secret := p.PaymentSecret
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}

	// WIRE NOTE: seq_amount rides as a JSON NUMBER — a quoted string unmarshals to 0
	// in the Go peer, which here would read as "the whole offer".
	// The price we derived goes with the request (BtcAmount), so a maker that re-rested this
	// offer's remainder between our read of the book and this request prices off our whole.
	if err := sendXc(&XcMsg{Type: XcPlnTermsRequest, SeqAmount: takeSeq, BtcAmount: takeBtc}, p.Crypter, send); err != nil {
		return nil, err
	}
	stage("terms-requested")
	terms, err := recvXcType(recv, p.Crypter, XcPlnTerms, p.Timing.TermsWait)
	if err != nil {
		return nil, err
	}
	stage("terms")
	if terms.MakerLNNodeID == "" {
		return nil, fmt.Errorf("pureln taker: maker advertised no hold-leg node id")
	}
	// Both legs must match what WE derived from the signed offer, so a maker cannot
	// re-price a slice it was asked to fill.
	if terms.BtcAmount != 0 && terms.BtcAmount != takeBtc {
		return nil, fmt.Errorf("%w: maker BTC amount %d != the %d the offer's ratio gives", ErrXcBadTerms, terms.BtcAmount, takeBtc)
	}
	if terms.SeqAmount != 0 && terms.SeqAmount != takeSeq {
		return nil, fmt.Errorf("%w: maker asset amount %d != the %d we asked for", ErrXcBadTerms, terms.SeqAmount, takeSeq)
	}
	// An older maker quotes nothing at all, which is only safe for a whole lift:
	// there is no price to disagree about when the slice IS the offer.
	if terms.SeqAmount == 0 && takeSeq != p.SeqAmtMsat {
		return nil, fmt.Errorf("%w: maker did not price the partial slice", ErrXcBadTerms)
	}

	// Our secret P, and the invoice on our incoming leg bound to it.
	pre := make([]byte, 32)
	if _, err := rand.Read(pre); err != nil {
		return nil, err
	}
	invoice, hashH, err := p.Ops.PrepareInvoice(pre, invoiceAmt)
	if err != nil {
		sendXcFail(p.Crypter, send, "invoice", err.Error())
		return nil, fmt.Errorf("prepare invoice: %w", err)
	}
	stage("invoice-prepared")
	if err := sendXc(&XcMsg{Type: XcPlnAssetInvoice, HashH: hex.EncodeToString(hashH), Bolt11: invoice}, p.Crypter, send); err != nil {
		return nil, err
	}
	ready, err := recvXcType(recv, p.Crypter, XcPlnHoldReady, p.Timing.SeqLockWait)
	if err != nil {
		return nil, err
	}
	stage("hold-ready")
	// The maker names the hold timelock it needs to pay our invoice safely. Our
	// exposure is exactly that long if the maker stalls, so it is honoured up to a
	// ceiling and refused beyond it (the swap costs nothing to abandon here).
	finalCltv := p.FinalCltv
	if ready.HoldCltv > finalCltv {
		if ready.HoldCltv > p.MaxHoldCltv {
			sendXcFail(p.Crypter, send, "hold_cltv", fmt.Sprintf("maker asks for a %d-block hold, above our %d ceiling", ready.HoldCltv, p.MaxHoldCltv))
			return nil, fmt.Errorf("%w: maker requires hold cltv %d > max %d", ErrXcBadTerms, ready.HoldCltv, p.MaxHoldCltv)
		}
		finalCltv = ready.HoldCltv
	}

	// Pay the maker's hold by bare hash; blocks until the maker settles.
	// Emit H at the moment we commit funds so a caller that is killed mid-pay (e.g. an LSP whose
	// exec timeout fires during PayHold) can reconcile: it greps this H and asks the paying node
	// `listsendpays H` before telling the user nothing was spent, instead of prompting a double-pay.
	if p.Log != nil {
		p.Log("pureln taker: paying maker hold on H=%s (funds now committed)", hex.EncodeToString(hashH))
	}
	revealed, err := p.Ops.PayHold(hashH, terms.MakerLNNodeID, holdAmt, finalCltv, secret)
	if err != nil {
		sendXcFail(p.Crypter, send, "pay_hold", err.Error())
		return nil, fmt.Errorf("pay hold: %w", err)
	}
	stage("hold-settled")
	// The maker settled with our own P; sanity-check the reveal.
	want := sha256.Sum256(pre)
	if hex.EncodeToString(revealed) != hex.EncodeToString(pre) || hex.EncodeToString(hashH) != hex.EncodeToString(want[:]) {
		return nil, fmt.Errorf("%w: settle revealed a preimage that is not ours", ErrLNPeer)
	}
	return &TakerPlnResult{HashH: hashH, Preimage: pre, FilledSeqMsat: takeSeq, FilledBtcMsat: takeBtc}, nil
}

// RunTakerPureLNBuy / RunTakerPureLNSell are the direction-explicit entry points.
func RunTakerPureLNBuy(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Direction = PlnBuy
	return RunTakerPureLN(p, send, recv)
}
func RunTakerPureLNSell(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Direction = PlnSell
	return RunTakerPureLN(p, send, recv)
}

// ErrLNPeer flags a counterparty protocol violation in a pure-LN swap.
var ErrLNPeer = fmt.Errorf("pure-LN peer protocol violation")

// PlnPriceSlackMsat is how far a taker's price of a pure-LN slice may differ from the maker's own
// for the maker to take the taker's. A maker re-rests the remainder of a partly filled offer under
// the same offer id, its counter leg rounded to the unit; a taker that read the book before the
// re-rest prices the slice off the earlier whole, the maker off the new one, and the two differ by
// at most about one unit of the counter leg (1000 msat) and a rounding msat. Two units cover that,
// and are worth nothing next to any slice.
const PlnPriceSlackMsat = 2000

// PlnAgreedBtc is the counter-leg msat a pure-LN maker quotes for a slice it prices at `own`, when
// the taker's request priced it at `asked` (0: a taker that sends no price): the taker's figure
// when it is within PlnPriceSlackMsat of the maker's, so both sides hold the same number.
func PlnAgreedBtc(own, asked uint64) uint64 {
	if asked == 0 {
		return own
	}
	d := own - asked
	if asked > own {
		d = asked - own
	}
	if d <= PlnPriceSlackMsat {
		return asked
	}
	return own
}
