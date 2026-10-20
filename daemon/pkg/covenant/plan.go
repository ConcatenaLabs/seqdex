package covenant

import "fmt"

// FillPlan is the covenant-specific recipe a taker (or an always-online settler)
// needs to assemble the permissionless FILL spend for one covenant input. The
// caller supplies the ordinary tx scaffolding (its own payment/fee inputs, its
// asset-A receipt, change, and the network fee); this plan pins the parts the
// covenant enforces: the fixed input-bound output map, the amounts, and the
// covenant input's introspection-only witness.
type FillPlan struct {
	InputIndex     uint32 // k: the covenant input's consensus index
	CreditIndex    uint32 // 2k: the maker-credit output (asset B, spk == maker_prog)
	RemainderIndex uint32 // 2k+1: the re-paid remainder of asset A (only if partial)
	Filled         uint64 // asset-A atoms taken by this fill
	Remainder      uint64 // asset-A atoms re-paid to the covenant (0 for a full fill)
	RequiredB      uint64 // asset-B atoms the maker MUST be credited (ceil price)
	Partial        bool   // whether a remainder output is required
	FillLeaf       []byte // covenant input witness item 0
	ControlBlock   []byte // covenant input witness item 1
	OrderSPK       []byte // the covenant scriptPubKey (== remainder output spk on a partial)
	Taptree        *Taptree
	// Stack, when set, is the whole witness stack of an order whose leaf is
	// not the tapscript FILL leaf: a SimOrder's [witness, program, cmr,
	// control block]. FillLeaf and Taptree are then empty.
	Stack [][]byte
}

// Witness returns the covenant input's witness stack: [leaf, control_block]
// for the tapscript FILL leaf, Stack for a SimOrder.
func (p *FillPlan) Witness() [][]byte {
	if p.Stack != nil {
		return p.Stack
	}
	return [][]byte{p.FillLeaf, p.ControlBlock}
}

// PlanFill computes the FILL recipe for taking `filled` atoms of asset A from a
// covenant UTXO currently holding `locked` atoms, spent at consensus input
// index k. It enforces the covenant's own floors (filled >= min_lot; any
// remainder >= min_lot) so a plan that the interpreter would reject is caught
// here rather than on broadcast.
func (o Order) PlanFill(locked, filled uint64, k uint32) (*FillPlan, error) {
	if filled == 0 || filled > locked {
		return nil, fmt.Errorf("filled %d out of range (locked %d)", filled, locked)
	}
	if filled < o.MinLot {
		return nil, fmt.Errorf("filled %d below min_lot %d", filled, o.MinLot)
	}
	remainder := locked - filled
	if remainder != 0 && remainder < o.MinLot {
		return nil, fmt.Errorf("remainder %d below min_lot %d (would be dust-griefing)", remainder, o.MinLot)
	}
	if err := o.CheckArithmetic(locked); err != nil {
		return nil, err
	}
	tap, err := o.Derive()
	if err != nil {
		return nil, err
	}
	return &FillPlan{
		InputIndex:     k,
		CreditIndex:    2 * k,
		RemainderIndex: 2*k + 1,
		Filled:         filled,
		Remainder:      remainder,
		RequiredB:      CeilPrice(filled, o.RateNum, o.RateDen),
		Partial:        remainder != 0,
		FillLeaf:       tap.FillLeaf,
		ControlBlock:   tap.ControlBlock(o.InternalKey),
		OrderSPK:       tap.ScriptPubKey,
		Taptree:        tap,
	}, nil
}

// JointFillPlan is the recipe for the TWO-COVENANT / both-makers-offline
// settlement: an always-online settler spends BOTH covenant UTXOs in one tx,
// each crediting the other maker, with NO taker capital. The two orders are
// counterparties — orderA sells asset A wanting B, orderB sells asset B wanting
// A — so orderA's locked A pays orderB's maker credit and vice versa.
//
// The fixed input-bound output map makes this collision-free by construction:
// the A-covenant is input 0 (credit at output 0 = B to maker A, remainder at 1),
// the B-covenant is input 1 (credit at output 2 = A to maker B, remainder at 3).
// PlanFill(k) already encodes exactly that per-input map, so the joint plan is
// just the two single-input plans at k=0 and k=1.
//
// NOTE: PlanJointSettlement (settle.go) completes this into a broadcast-ready
// tx layout (credit + reserved-slot handling, gap outputs, cross-checks) and the
// always-online settler (cmd/seqob-settler) assembles + broadcasts it. The joint
// spend is proven end-to-end on regtest by feature_seqob_joint_covenant.py:
// two covenant-funded orders match and settle against each other in ONE tx with
// BOTH makers offline.
type JointFillPlan struct {
	A *FillPlan // the A-covenant (input 0): credit B at output 0, remainder A at 1
	B *FillPlan // the B-covenant (input 1): credit A at output 2, remainder B at 3
}

// PlanJointFill builds the two-covenant settlement recipe. orderA sells asset A
// (locked lockedA, filled fillA); orderB sells asset B (locked lockedB, filled
// fillB). For a clean cross the settler picks fillA and fillB so each side's
// required credit is covered by the other's locked value.
func PlanJointFill(orderA Order, lockedA, fillA uint64, orderB Order, lockedB, fillB uint64) (*JointFillPlan, error) {
	pa, err := orderA.PlanFill(lockedA, fillA, 0)
	if err != nil {
		return nil, fmt.Errorf("A-covenant: %w", err)
	}
	pb, err := orderB.PlanFill(lockedB, fillB, 1)
	if err != nil {
		return nil, fmt.Errorf("B-covenant: %w", err)
	}
	return &JointFillPlan{A: pa, B: pb}, nil
}
