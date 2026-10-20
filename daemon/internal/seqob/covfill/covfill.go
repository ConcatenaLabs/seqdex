// Package covfill is the taker driver for the SeqOB passive-CLOB covenant: it
// turns one or more resting covenant orders (tapscript-locked on-chain UTXOs
// ANYONE may fill) into one broadcast FILL transaction funded and signed by an
// elementsd wallet.
//
// Where it sits among the existing covenant consumers:
//
//   - pkg/covenant owns the byte-exact primitive (leaf, taptree, PlanFill), the
//     multi-order layout (BuildFillTx) and the leaf's judgement of a
//     transaction (CheckFill), and stays pure — no RPC, no wallet. This
//     package only CONSUMES it.
//
//   - internal/seqob/settler assembles the JOINT covenant-vs-covenant fill (two
//     covenant inputs, settler fee input); its recipe (fixed 2k/2k+1 output map,
//     explicit-only outputs, sign-then-inject-witness order against elementsd)
//     is the one used here, with the taker's own wallet-funded inputs.
//
//   - the web wallet fills one covenant live through the SWK wasm assembler
//     (SWK/lwk_wollet/src/seqob_covenant.rs build_covenant_fill_tx); for a single
//     order this driver is that assembler's Go port, byte-compatible in layout.
//
// The layout, for orders 0..n-1:
//
//	input  k   : order k's covenant UTXO — witness [FILL leaf, control block], NO sig
//	input  n+  : the taker wallet's credit-asset (and fee-asset) funding UTXOs
//	output 2k  : order k's maker credit — its asset B, >= ceil price, spk OP_1 <maker_prog>
//	output 2k+1: PARTIAL -> order k's self-replicating remainder covenant (its asset A)
//	             FULL    -> an explicit output in any asset but order k's A (a "gap"):
//	                        the leaf inspects slot 2k+1 and reads an asset-A output
//	                        there as an underpaid remainder. The gap is the change
//	                        of the asset the order is paid in, else another change,
//	                        else the fee output, else another order's receipt, else
//	                        a gap coin of DustFoldAtoms of the credit asset back
//	                        to the taker.
//	then       : the taker's receipts (one per sold asset), the changes, the fee.
//
// For one order that is [credit, remainder or gap, receipt, changes, fee].
//
// A take may be a Simplicity order (Take.Sim, the template seqdex/order)
// instead: its input carries [witness, program, cmr, control block], built
// from the template's pinned programs, and a whole fill's 2k+1 may hold
// anything, the taker's own receipt of asset A included, since that program
// does not read it. Every rule above runs through covenant.Leaf, so the two
// kinds of order fill side by side in one transaction.
//
// Trust model: identical to the settler's. Every maker-facing output is fixed by
// the maker's own FILL leaf and re-checked by the script interpreter, so a wrong
// assembly can only produce a tx the chain REJECTS — never a theft. The driver
// still fails closed before broadcast (trustless spk verification, on-chain UTXO
// state, min-lot floors, each order's leaf run over the transaction by
// covenant.CheckFill, and a mandatory testmempoolaccept) so it never ships a
// spend the mempool would bounce.
//
// A covenant fill is ONE atomic transaction: there is no counterparty leg, no
// refund branch and no session state to resume, so the driver keeps no state
// file — on any failure before broadcast nothing moved, and after broadcast the
// tx either confirms or is dropped whole.
package covfill

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// DefaultDustFold is the atom threshold below which a leftover change would be
// dust: credit-asset dust is folded into the maker credit (the leaf enforces
// only a MINIMUM, so overpaying is always valid) and fee-asset dust into the
// fee, mirroring the settler's dustFold so no dust output is ever emitted.
const DefaultDustFold = 1000

// Node is the wallet-scoped elementsd JSON-RPC surface the driver needs
// (satisfied by xchain.(*RPC) with a wallet path; scripted by tests).
type Node interface {
	Call(out interface{}, method string, params ...interface{}) error
}

// Take is one covenant order a fill transaction takes.
type Take struct {
	// Order is the covenant's terms (from the relay's CovenantTerms via
	// bridge.OrderFromTerms). Asset ids/keys are INTERNAL byte order.
	Order covenant.Order
	// Sim, when set, is a Simplicity order (from the relay's
	// SimplicityCovenantTerms via offer.SimOrderOf) and Order is ignored.
	Sim *covenant.SimOrder
	// Txid/Vout locate the funded covenant UTXO being filled.
	Txid string
	Vout uint32
	// TakeAtoms is the asset-A atoms to take; 0 = the whole covenant. A take
	// leaving a remainder in (0, min_lot) is SHRUNK to locked-min_lot (the
	// largest fill leaving a valid remainder), exactly as the crosser's
	// ClampOverlap and the web's planFill dust rule size it.
	TakeAtoms uint64
	// MaxPayB, when > 0, caps the asset-B atoms the fill may credit the maker. The
	// price a taker decided on comes from the OFFER it read (offer_amount /
	// want_amount); the price the fill actually pays is baked into the covenant's
	// rate_num/rate_den, and both are under the maker's signature. A maker whose
	// terms demand more than its offer advertises would otherwise be paid whatever
	// the leaf requires. Dust folded into the credit (the taker's own change below
	// the fold threshold) is not counted against the cap.
	MaxPayB uint64
}

// Params configures one covenant fill transaction.
type Params struct {
	// Takes are the orders filled, take k at input k: its maker's credit at
	// output 2k, its remainder (or a non-A gap) at 2k+1. When empty, the single
	// order below is taken at input 0.
	Takes []Take
	// Order, Txid, Vout, TakeAtoms and MaxPayB describe a single order, as one
	// Take, when Takes is empty.
	Order     covenant.Order
	Sim       *covenant.SimOrder
	Txid      string
	Vout      uint32
	TakeAtoms uint64
	MaxPayB   uint64
	// FeeAtoms/FeeAssetDisplay are the explicit network fee (open fee market:
	// any accepted asset, denominated in that asset's own atoms). The fee asset
	// must NOT be any taken covenant's sold asset A: the taker never funds asset
	// A (it comes from the covenant input), and an asset-A fee output could be
	// misread by the leaf at the gap slot.
	FeeAtoms        uint64
	FeeAssetDisplay string
	// Node is the wallet-scoped RPC used for funding, signing and broadcast.
	Node Node
	// DustFoldAtoms overrides DefaultDustFold when > 0. It is also the size of a
	// gap coin: a full fill whose 2k+1 no change, fee or receipt can stand at
	// gets an output of this many atoms of the asset its order is paid in, back
	// to the taker.
	DustFoldAtoms uint64
	// AllowMakerCancellable opts in to filling a non-NUMS-internal-key order (a
	// maker-cancellable/key-path-rug covenant). Default false: fail closed.
	AllowMakerCancellable bool
}

func (p Params) takes() []Take {
	if len(p.Takes) > 0 {
		return p.Takes
	}
	return []Take{{Order: p.Order, Sim: p.Sim, Txid: p.Txid, Vout: p.Vout, TakeAtoms: p.TakeAtoms, MaxPayB: p.MaxPayB}}
}

// leaf is the take's order, whichever covenant it rests under.
func (t Take) leaf() covenant.Leaf {
	if t.Sim != nil {
		return *t.Sim
	}
	return t.Order
}

// Filled is what one take did.
type Filled struct {
	Txid           string // the covenant outpoint it spent
	Vout           uint32
	InputIndex     uint32 // k
	CreditIndex    uint32 // 2k
	RemainderIndex uint32 // 2k+1
	Locked         uint64
	Filled         uint64
	Remainder      uint64
	PaidB          uint64
	Partial        bool
}

// Result is a settled fill. Locked..Partial describe the take at input 0;
// Fills describes every take.
type Result struct {
	Txid      string
	RawHex    string
	Locked    uint64 // asset-A atoms the covenant held before this fill
	Filled    uint64 // asset-A atoms taken (after any dust-shrink)
	Remainder uint64 // asset-A atoms re-rested to the covenant (0 = full fill)
	PaidB     uint64 // asset-B atoms actually credited to the maker (>= ceil price)
	Partial   bool
	Fills     []Filled
}

// ShrinkTake applies the covenant dust-remainder rule to a requested take:
// take==0 means the whole covenant; a take leaving 0 < remainder < minLot is
// shrunk to locked-minLot (the largest fill with a valid remainder). It does NOT
// enforce the other floors (take >= minLot etc.) — covenant.Order.PlanFill
// re-validates everything and fails closed.
func ShrinkTake(locked, take, minLot uint64) uint64 {
	if take == 0 || take > locked {
		return locked
	}
	if rem := locked - take; rem > 0 && rem < minLot {
		return locked - minLot
	}
	return take
}

// planned is one take checked against the chain and planned at its input.
type planned struct {
	take   Take
	locked uint64
	plan   *covenant.FillPlan
	a, b   string // display asset ids
	credit uint64
}

// FillCovenant verifies each resting covenant on-chain, assembles one FILL
// spend of all of them (wallet-funded), signs the wallet inputs via elementsd,
// injects each covenant's introspection-only witness, gates on
// testmempoolaccept and broadcasts. It fails closed on ANY constraint mismatch
// — a spend the chain or mempool would reject is never broadcast.
func FillCovenant(p Params) (*Result, error) {
	if p.Node == nil {
		return nil, fmt.Errorf("no node RPC")
	}
	dust := p.DustFoldAtoms
	if dust == 0 {
		dust = DefaultDustFold
	}
	takes := p.takes()

	// --- static constraint checks (fail closed before any I/O) --------------
	feeDisplay := strings.ToLower(p.FeeAssetDisplay)
	if b, err := hex.DecodeString(feeDisplay); err != nil || len(b) != 32 {
		return nil, fmt.Errorf("fee asset must be 32-byte display hex (got %q)", p.FeeAssetDisplay)
	}
	if p.FeeAtoms == 0 {
		return nil, fmt.Errorf("fee must be > 0 atoms")
	}
	sold := map[string]bool{}
	for _, t := range takes {
		if t.leaf().Sells() == t.leaf().PaysIn() {
			return nil, fmt.Errorf("degenerate covenant: sold asset A equals payment asset B")
		}
		sold[displayHex(t.leaf().Sells())] = true
	}
	if sold[feeDisplay] {
		return nil, fmt.Errorf("fee asset must not be the sold asset A of a covenant taken (%s)", feeDisplay)
	}
	for _, t := range takes {
		if b := displayHex(t.leaf().PaysIn()); sold[b] {
			return nil, fmt.Errorf("a take pays in %s, which another covenant taken here sells: the taker never funds a sold asset", b)
		}
	}

	// --- read + trustlessly verify each covenant UTXO at the live tip -------
	var ps []planned
	covs := map[string]bool{}
	for k, t := range takes {
		leaf := t.leaf()
		a := displayHex(leaf.Sells())
		locked, onchainSPK, err := verifyCovenantUTXO(p.Node, t.Txid, t.Vout, a)
		if err != nil {
			return nil, err
		}
		if err := leaf.VerifyScript(onchainSPK, p.AllowMakerCancellable); err != nil {
			return nil, fmt.Errorf("covenant %s:%d fails trustless verify: %w", t.Txid, t.Vout, err)
		}
		// --- size the fill (dust-shrink, then the covenant's own floors) --------
		take := ShrinkTake(locked, t.TakeAtoms, leaf.Lot())
		plan, err := leaf.PlanFill(locked, take, uint32(k))
		if err != nil {
			return nil, fmt.Errorf("fill plan of %s:%d: %w", t.Txid, t.Vout, err)
		}
		if t.MaxPayB > 0 && plan.RequiredB > t.MaxPayB {
			num, den := leaf.Rate()
			return nil, fmt.Errorf("covenant rate %d/%d demands %d asset-B atoms for %d asset-A atoms, above the %d ceiling the taker priced from the offer", num, den, plan.RequiredB, plan.Filled, t.MaxPayB)
		}
		covs[fmt.Sprintf("%s:%d", t.Txid, t.Vout)] = true
		ps = append(ps, planned{take: t, locked: locked, plan: plan, a: a, b: displayHex(leaf.PaysIn()), credit: plan.RequiredB})
	}

	// --- select the taker's funding (credit assets + the fee asset) ----------
	// A full fill whose 2k+1 nothing else can take gets a gap coin of `dust`
	// atoms of its credit asset; gapCoins counts them per take, found by laying
	// the transaction out and asking again with that much more.
	gapCoins := make([]bool, len(ps))
	for attempt := 0; ; attempt++ {
		need := map[string]uint64{}
		for k, q := range ps {
			need[q.b] += q.plan.RequiredB
			if gapCoins[k] {
				need[q.b] += dust
			}
		}
		need[feeDisplay] += p.FeeAtoms
		sel, err := selectFunding(p.Node, need, covs, sold)
		if err != nil {
			return nil, err
		}
		res, missing, err := fillWith(p, ps, sel, gapCoins, dust, feeDisplay)
		if missing >= 0 && attempt < len(ps) {
			gapCoins[missing] = true
			continue
		}
		return res, err
	}
}

// fillWith lays out, signs and broadcasts the fill with the coins selected.
// It returns missing >= 0 when take `missing` is a full fill with nothing to
// stand at its 2k+1: the coins are released and the caller funds a gap coin.
// Every path that does not broadcast releases the coins it was given.
func fillWith(p Params, ps []planned, sel *funding, gapCoins []bool, dust uint64, feeDisplay string) (*Result, int, error) {
	broadcast := false
	defer func() {
		if !broadcast {
			_ = lockCoins(p.Node, sel.inputs, true)
		}
	}()

	// Per-asset change, with dust folded where it cannot hurt: credit-asset dust
	// overpays the maker (the leaf enforces value >= required, never ==), fee-
	// asset dust raises the fee. Both stay the taker's own money to give away.
	feeValue := p.FeeAtoms
	paid := map[string]uint64{}
	for k, q := range ps {
		paid[q.b] += q.plan.RequiredB
		if gapCoins[k] {
			paid[q.b] += dust
		}
	}
	paid[feeDisplay] += p.FeeAtoms
	var changeAssets []string // credit assets in take order, then the fee asset
	for _, q := range ps {
		if !contains(changeAssets, q.b) {
			changeAssets = append(changeAssets, q.b)
		}
	}
	if !contains(changeAssets, feeDisplay) {
		changeAssets = append(changeAssets, feeDisplay)
	}
	change := map[string]uint64{}
	for _, a := range changeAssets {
		c := sel.total[a] - paid[a]
		if c == 0 {
			continue
		}
		if c < dust {
			if a == feeDisplay {
				feeValue += c
			} else {
				for k := range ps {
					if ps[k].b == a {
						ps[k].credit += c
						break
					}
				}
			}
			continue
		}
		change[a] = c
	}

	// --- outputs: receipts, changes, gap coins and the fee --------------------
	var receiptAssets []string
	receipt := map[string]uint64{}
	for _, q := range ps {
		if !contains(receiptAssets, q.a) {
			receiptAssets = append(receiptAssets, q.a)
		}
		receipt[q.a] += q.plan.Filled
	}
	receiptSPK, err := freshUnconfSPK(p.Node)
	if err != nil {
		return nil, -1, fmt.Errorf("receipt spk: %w", err)
	}
	needChangeSPK := len(change) > 0
	for _, g := range gapCoins {
		needChangeSPK = needChangeSPK || g
	}
	var changeSPK []byte
	if needChangeSPK {
		if changeSPK, err = freshUnconfSPK(p.Node); err != nil {
			return nil, -1, fmt.Errorf("change spk: %w", err)
		}
	}
	out := func(display string, atoms uint64, spk []byte) (covenant.Output, error) {
		b, err := hex.DecodeString(display)
		if err != nil || len(b) != 32 {
			return covenant.Output{}, fmt.Errorf("asset %q", display)
		}
		var a [32]byte
		copy(a[:], elementsutil.ReverseBytes(b))
		return covenant.Explicit(a, atoms, spk), nil
	}
	// The tail in its order before any gap moves out of it: receipts, the
	// credit-asset changes, the fee-asset change, the fee.
	type cand struct {
		o     covenant.Output
		asset string
		kind  int // 0 receipt, 1 credit-asset change, 2 fee change, 3 fee
	}
	var tail []cand
	for _, a := range receiptAssets {
		o, err := out(a, receipt[a], receiptSPK)
		if err != nil {
			return nil, -1, err
		}
		tail = append(tail, cand{o, a, 0})
	}
	for _, a := range changeAssets {
		c, ok := change[a]
		if !ok {
			continue
		}
		o, err := out(a, c, changeSPK)
		if err != nil {
			return nil, -1, err
		}
		kind := 1
		if a == feeDisplay {
			kind = 2
			for _, q := range ps {
				if q.b == a {
					kind = 1 // the fee asset is also a credit asset: one change
				}
			}
		}
		tail = append(tail, cand{o, a, kind})
	}
	feeOut, err := out(feeDisplay, feeValue, []byte{})
	if err != nil {
		return nil, -1, err
	}
	tail = append(tail, cand{feeOut, feeDisplay, 3})

	// --- gaps: what stands at each full fill's 2k+1 --------------------------
	// Preference, as the wasm assembler's for one order: the change of the
	// asset the order is paid in, any other credit-asset change, the fee-asset
	// change, the fee output itself, then another order's receipt; never an
	// output of the asset that order sells.
	gaps := make([]*covenant.Output, len(ps))
	for k, q := range ps {
		if q.plan.Partial {
			continue
		}
		if gapCoins[k] {
			o, err := out(q.b, dust, changeSPK)
			if err != nil {
				return nil, -1, err
			}
			gaps[k] = &o
			continue
		}
		pick := -1
		readsRest := q.take.leaf().WholeFillReadsRest()
		rank := func(c cand) int {
			switch {
			case c.asset == q.a && readsRest:
				return -1
			case c.kind == 1 && c.asset == q.b:
				return 0
			case c.kind == 1:
				return 1
			case c.kind == 2:
				return 2
			case c.kind == 3:
				return 3
			default:
				return 4
			}
		}
		best := 99
		for i, c := range tail {
			if r := rank(c); r >= 0 && r < best {
				best, pick = r, i
			}
		}
		if pick < 0 {
			return nil, k, fmt.Errorf("take %d needs a gap coin", k)
		}
		o := tail[pick].o
		gaps[k] = &o
		tail = append(tail[:pick], tail[pick+1:]...)
	}

	// --- assemble, judge every order's leaf over it, sign, inject ------------
	spec := covenant.FillTx{Gaps: gaps}
	for _, q := range ps {
		spec.Takes = append(spec.Takes, covenant.Take{Order: q.take.Order, Sim: q.take.Sim, Txid: q.take.Txid, Vout: q.take.Vout, Locked: q.locked, Filled: q.plan.Filled})
		spec.Credits = append(spec.Credits, q.credit)
	}
	for _, u := range sel.inputs {
		spec.Inputs = append(spec.Inputs, covenant.Outpoint{Txid: u.txid, Vout: u.vout})
	}
	for _, c := range tail {
		spec.Tail = append(spec.Tail, c.o)
	}
	tx, plans, err := covenant.BuildFillTx(spec)
	if err != nil {
		return nil, -1, err
	}
	// Sign the wallet inputs over the transaction WITHOUT the covenant witnesses,
	// then inject them (the settler's proven sign-then-inject order): the covenant
	// inputs are not the wallet's, so complete=false is expected.
	for k := range ps {
		tx.Inputs[k].Witness = nil
	}
	rawHex, err := tx.ToHex()
	if err != nil {
		return nil, -1, err
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := p.Node.Call(&signed, "signrawtransactionwithwallet", rawHex); err != nil {
		return nil, -1, fmt.Errorf("signrawtransactionwithwallet: %w", err)
	}
	if signed.Hex == "" {
		return nil, -1, fmt.Errorf("signrawtransactionwithwallet returned empty hex")
	}
	ftx, err := transaction.NewTxFromHex(signed.Hex)
	if err != nil {
		return nil, -1, fmt.Errorf("parse signed tx: %w", err)
	}
	if len(ftx.Inputs) < len(ps) {
		return nil, -1, fmt.Errorf("signed tx has %d inputs, fewer than its %d covenants", len(ftx.Inputs), len(ps))
	}
	for k := range ps {
		ftx.Inputs[k].Witness = plans[k].Witness()
	}
	for k, q := range ps {
		if _, err := q.take.leaf().Judge(covenant.OutputsOf(ftx), uint32(k), q.locked, plans[k].Partial); err != nil {
			return nil, -1, fmt.Errorf("the signed transaction no longer satisfies covenant %d: %w", k, err)
		}
	}
	finalHex, err := ftx.ToHex()
	if err != nil {
		return nil, -1, fmt.Errorf("serialize final tx: %w", err)
	}

	// --- mempool gate, then broadcast ----------------------------------------
	var accept []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := p.Node.Call(&accept, "testmempoolaccept", []string{finalHex}); err != nil {
		return nil, -1, fmt.Errorf("testmempoolaccept: %w", err)
	}
	if len(accept) == 0 || !accept[0].Allowed {
		reason := "unknown"
		if len(accept) > 0 && accept[0].RejectReason != "" {
			reason = accept[0].RejectReason
		}
		return nil, -1, fmt.Errorf("refusing to broadcast: node would reject the FILL spend (%s)", reason)
	}
	result := func(txid string) *Result {
		r := &Result{Txid: txid, RawHex: finalHex}
		for k, q := range ps {
			r.Fills = append(r.Fills, Filled{Txid: q.take.Txid, Vout: q.take.Vout, InputIndex: uint32(k),
				CreditIndex: plans[k].CreditIndex, RemainderIndex: plans[k].RemainderIndex, Locked: q.locked,
				Filled: plans[k].Filled, Remainder: plans[k].Remainder, PaidB: q.credit, Partial: plans[k].Partial})
		}
		f := r.Fills[0]
		r.Locked, r.Filled, r.Remainder, r.PaidB, r.Partial = f.Locked, f.Filled, f.Remainder, f.PaidB, f.Partial
		return r
	}
	var txid string
	if err := p.Node.Call(&txid, "sendrawtransaction", finalHex); err != nil {
		// A reply lost after the node accepted the spend is not a failed fill: if the
		// covenant outpoint is now spent and the node knows our transaction, the
		// fill landed. Reporting "nothing executed" here made the crosser re-plan the
		// same crossing, or book drift that never happened.
		if landedTxid, ok := alreadyLanded(p.Node, ps[0].take.Txid, ps[0].take.Vout, finalHex); ok {
			broadcast = true
			return result(landedTxid), -1, nil
		}
		return nil, -1, fmt.Errorf("sendrawtransaction: %w", err)
	}
	broadcast = true
	return result(txid), -1, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// alreadyLanded reports whether the covenant outpoint is spent and the node knows
// our assembled transaction (decoded to its txid), i.e. the fill is in its mempool
// or chain despite the broadcast reply being lost.
func alreadyLanded(node Node, covTxid string, covVout uint32, rawHex string) (string, bool) {
	var utxo *struct {
		Confirmations int `json:"confirmations"`
	}
	if err := node.Call(&utxo, "gettxout", covTxid, covVout, true); err != nil || utxo != nil {
		return "", false
	}
	var dec struct {
		Txid string `json:"txid"`
	}
	if err := node.Call(&dec, "decoderawtransaction", rawHex); err != nil || dec.Txid == "" {
		return "", false
	}
	var known struct {
		Txid string `json:"txid"`
	}
	if err := node.Call(&known, "getrawtransaction", dec.Txid, true); err != nil {
		return "", false
	}
	return dec.Txid, true
}

// verifyCovenantUTXO reads the covenant outpoint at the live tip (gettxout is
// mempool-aware and returns null once spent) and checks it is unspent, explicit,
// and funded with the advertised sold asset. Returns the locked atoms and the
// on-chain scriptPubKey for the trustless spk equality check.
func verifyCovenantUTXO(node Node, txid string, vout uint32, assetADisplay string) (uint64, []byte, error) {
	var utxo *struct {
		Confirmations int64   `json:"confirmations"`
		Value         float64 `json:"value"`
		Asset         string  `json:"asset"`
		ScriptPubKey  struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := node.Call(&utxo, "gettxout", txid, vout, true); err != nil {
		return 0, nil, fmt.Errorf("gettxout %s:%d: %w", txid, vout, err)
	}
	if utxo == nil {
		return 0, nil, fmt.Errorf("covenant %s:%d is not an unspent utxo (spent, reorged away, or never funded)", txid, vout)
	}
	if utxo.Asset == "" {
		return 0, nil, fmt.Errorf("covenant %s:%d has no explicit asset (blinded output cannot be a covenant)", txid, vout)
	}
	if !strings.EqualFold(utxo.Asset, assetADisplay) {
		return 0, nil, fmt.Errorf("covenant %s:%d holds asset %s, want sold-asset %s", txid, vout, utxo.Asset, assetADisplay)
	}
	spk, err := hex.DecodeString(utxo.ScriptPubKey.Hex)
	if err != nil {
		return 0, nil, fmt.Errorf("bad on-chain spk hex: %w", err)
	}
	locked := coinsToAtoms(utxo.Value)
	if locked == 0 {
		return 0, nil, fmt.Errorf("covenant %s:%d has zero locked value", txid, vout)
	}
	return locked, spk, nil
}

// funding is a per-asset selection of the taker wallet's own UTXOs.
type funding struct {
	inputs []fundUTXO
	total  map[string]uint64
}

type fundUTXO struct {
	txid  string
	vout  uint32
	atoms uint64
	asset string
}

// selectFunding picks spendable wallet UTXOs covering each needed asset amount:
// per asset, the smallest single UTXO that covers the target, else smallest-
// first accumulation (the settler's proven selection). The covenant outpoints
// and any coin of a sold asset are never candidates — the taker must not fund
// the sold asset (asset A comes entirely from the covenant input; the wasm
// assembler enforces the same).
func selectFunding(node Node, need map[string]uint64, covs map[string]bool, sold map[string]bool) (*funding, error) {
	var raw []struct {
		Txid          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		Asset         string  `json:"asset"`
		Spendable     bool    `json:"spendable"`
		Confirmations int     `json:"confirmations"`
	}
	if err := node.Call(&raw, "listunspent", 1); err != nil {
		return nil, fmt.Errorf("listunspent: %w", err)
	}
	byAsset := map[string][]fundUTXO{}
	for _, u := range raw {
		a := strings.ToLower(u.Asset)
		if !u.Spendable || sold[a] {
			continue
		}
		if covs[fmt.Sprintf("%s:%d", u.Txid, u.Vout)] {
			continue // never re-select a covenant itself
		}
		if _, wanted := need[a]; !wanted {
			continue
		}
		byAsset[a] = append(byAsset[a], fundUTXO{txid: u.Txid, vout: u.Vout, atoms: coinsToAtoms(u.Amount), asset: a})
	}

	sel := &funding{total: map[string]uint64{}}
	// Deterministic asset order for a stable input layout (tests + logs).
	assets := make([]string, 0, len(need))
	for a := range need {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, asset := range assets {
		target := need[asset]
		if target == 0 {
			continue
		}
		cands := byAsset[asset]
		sort.Slice(cands, func(i, j int) bool { return cands[i].atoms < cands[j].atoms })
		picked, sum := pickCoins(cands, target)
		if sum < target {
			return nil, fmt.Errorf("wallet has only %d atoms of %s across %d spendable utxo(s), need %d",
				sum, asset, len(cands), target)
		}
		sel.inputs = append(sel.inputs, picked...)
		sel.total[asset] += sum
	}
	// Lock the picked coins in the wallet until this fill is broadcast or abandoned:
	// two fills funded from the same wallet in parallel (the crosser runs pairs
	// concurrently) otherwise pick the same smallest coins and the loser fails the
	// mempool gate with a conflict, booked as drift if it was the second leg.
	if err := lockCoins(node, sel.inputs, false); err != nil {
		return nil, fmt.Errorf("lockunspent: %w", err)
	}
	return sel, nil
}

// lockCoins locks (unlock=false) or releases (unlock=true) coins in the node wallet.
func lockCoins(node Node, coins []fundUTXO, unlock bool) error {
	if len(coins) == 0 {
		return nil
	}
	refs := make([]map[string]interface{}, 0, len(coins))
	for _, c := range coins {
		refs = append(refs, map[string]interface{}{"txid": c.txid, "vout": c.vout})
	}
	var ok bool
	return node.Call(&ok, "lockunspent", unlock, refs)
}

func pickCoins(cands []fundUTXO, target uint64) ([]fundUTXO, uint64) {
	for _, u := range cands { // smallest single cover (minimal change)
		if u.atoms >= target {
			return []fundUTXO{u}, u.atoms
		}
	}
	var picked []fundUTXO
	var sum uint64
	for _, u := range cands {
		picked = append(picked, u)
		sum += u.atoms
		if sum >= target {
			break
		}
	}
	return picked, sum
}

// freshUnconfSPK returns a fresh UNBLINDED wallet scriptPubKey (the settler's
// getnewaddress + unconfidential-form pattern): every output the driver emits
// must be explicit or the covenant leaf cannot introspect the tx.
func freshUnconfSPK(node Node) ([]byte, error) {
	var addr string
	if err := node.Call(&addr, "getnewaddress"); err != nil {
		return nil, fmt.Errorf("getnewaddress: %w", err)
	}
	var info struct {
		Unconfidential string `json:"unconfidential"`
		ScriptPubKey   string `json:"scriptPubKey"`
	}
	if err := node.Call(&info, "getaddressinfo", addr); err != nil {
		return nil, fmt.Errorf("getaddressinfo: %w", err)
	}
	spkHex := info.ScriptPubKey
	if info.Unconfidential != "" && info.Unconfidential != addr {
		var uinfo struct {
			ScriptPubKey string `json:"scriptPubKey"`
		}
		if err := node.Call(&uinfo, "getaddressinfo", info.Unconfidential); err == nil && uinfo.ScriptPubKey != "" {
			spkHex = uinfo.ScriptPubKey
		}
	}
	if spkHex == "" {
		return nil, fmt.Errorf("no scriptPubKey for address %s", addr)
	}
	return hex.DecodeString(spkHex)
}

// displayHex converts a 32-byte internal-order asset id to its display hex.
func displayHex(internal [32]byte) string {
	return hex.EncodeToString(elementsutil.ReverseBytes(internal[:]))
}

// coinsToAtoms converts decimal coins to integer atoms (1e8 per coin).
func coinsToAtoms(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(math.Round(v * 1e8))
}
