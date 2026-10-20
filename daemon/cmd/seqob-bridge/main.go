// Command seqob-bridge is the SILENT cross-rail settlement bridge for the SeqOB
// CLOB: it lets an order placed ON-CHAIN (a funded covenant resting order) match
// an order placed on LIGHTNING, invisibly, driven by the relay's matcher. The
// two owners each rest on the rail they chose; neither ever sees an "LP", a
// "submarine", a "take/post", or an on-chain-vs-Lightning label. When the matcher
// crosses a covenant order against a Lightning order (matcher.Match.CrossRail),
// the bridge settles the cross atomically under one preimage: it FILLS the
// on-chain covenant (paying the on-chain maker its own pinned ceil price in asset
// Y, receiving the sold asset X — all consensus-enforced) and settles the
// Lightning leg (delivering X and collecting Y over LN via the submarine/pure-LN
// orchestrator, REUSED unchanged). Net it moves X on-chain -> LN and Y LN ->
// on-chain and keeps the fee/spread.
//
// Why a distinct process (not folded into seqob-settler). The relay (seqobd),
// the settler, and the watcher are all KEYLESS and FUNDLESS. The bridge is
// fundamentally different: it holds its OWN inventory and an LN node so it can
// FRONT the cross. Folding it into the settler would give the settler a hot
// wallet and destroy the settler's fundless guarantee. So the bridge is its own
// role. Its no-loss model: it can never take USER funds (the maker is protected
// by the FILL leaf / consensus, the LN counterparty by the preimage atomicity);
// the only capital at risk is the bridge's OWN fronted inventory, only on a
// Bitcoin reorg of the on-chain FILL, bounded by the per-offer 0-conf cap and
// required to be covered by the fee.
//
// Subcommands:
//
//	plan   (reads a JSON cross on stdin)
//	    Emits the enforced settlement recipe: the on-chain covenant FILL leg
//	    (credit index/value/floor/spk, remainder or gap, the introspection-only
//	    witness) plus the priced Lightning leg (deliver/collect amounts), the
//	    fee/spread, and the fronting decision (0-conf front vs require
//	    confirmations). feature_seqob_bridge.py drives the on-chain FILL broadcast
//	    from exactly this output and mocks the LN leg at the orchestrator seam, so
//	    a bug in the planner fails the regtest proof.
//
//	run    (documented) the always-online loop: subscribe to the relay's CrossRail
//	    matches, resolve the covenant's locked amount + the LN price, bridge.Plan
//	    the cross, then (a) hold the LN counterparty's Y (hold invoice on H),
//	    (b) broadcast the on-chain FILL, (c) release the LN leg with P once the
//	    FILL is anchor-buried OR the amount is within the 0-conf cap. Steps (a) and
//	    (c) are the submarine orchestrator's RunReverse/RunNormal (pkg/xchain),
//	    which already implement the anchor-depth gate and the 0-conf fronting cap;
//	    the on-chain FILL assembly binds to an Elements node (sendrawtransaction).
//	    The enforcement core proven here is node- and LN-agnostic.
//
// All asset ids, programs, and keys are INTERNAL byte order (32-byte hex),
// matching on-chain introspection (display-hex asset ids reversed).
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/bridge"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

func die(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "seqob-bridge: "+format+"\n", a...)
	os.Exit(1)
}

// covInput is the on-chain covenant order the bridge fills.
type covInput struct {
	Num      uint64 `json:"num"`
	Den      uint64 `json:"den"`
	MinLot   uint64 `json:"minlot"`
	Prog     string `json:"prog"`     // 32-byte maker payout program hex
	MakerX   string `json:"makerx"`   // 32-byte hex
	Expiry   int64  `json:"expiry"`   // REFUND absolute lock: a height, or a median time from 500000000
	Internal string `json:"internal"` // optional 32-byte internal key hex; default NUMS
	Locked   uint64 `json:"locked"`   // atoms of X currently locked in the covenant UTXO
	Fill     uint64 `json:"fill"`     // atoms of X taken by this cross
	K        uint32 `json:"k"`        // the covenant's input index in the fill (default 0)
}

// lnInput is the Lightning counterparty's price + 0-conf cap.
type lnInput struct {
	OfferY   uint64 `json:"offer_y"`
	WantX    uint64 `json:"want_x"`
	Max0Conf uint64 `json:"max_0conf"`
}

// planInput is the cross-rail cross the bridge is asked to settle: a covenant
// selling X wanting Y, crossed against a Lightning order buying X with Y.
type planInput struct {
	AssetX        string   `json:"asset_x"`
	AssetY        string   `json:"asset_y"`
	Covenant      covInput `json:"covenant"`
	Lightning     lnInput  `json:"lightning"`
	PolicyFeeSats uint64   `json:"policy_fee_sats"`
	RateY         uint64   `json:"rate_y"`
}

func hx32(s, name string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		die("%s must be 32-byte hex (got %q)", name, s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// makerCreditSPK is the v1-taproot maker payout scriptPubKey OP_1 <32-byte prog>
// (OP_1 == 0x51). Identical to the credit spk the FILL leaf demands.
func makerCreditSPK(prog []byte) []byte {
	return append([]byte{0x51, 0x20}, prog...)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: seqob-bridge <plan> [< cross.json]")
	}
	switch os.Args[1] {
	case "plan":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			die("read stdin: %v", err)
		}
		var in planInput
		if err := json.Unmarshal(raw, &in); err != nil {
			die("parse cross json: %v", err)
		}
		x := hx32(in.AssetX, "asset_x")
		y := hx32(in.AssetY, "asset_y")
		prog, err := hex.DecodeString(in.Covenant.Prog)
		if err != nil || len(prog) != 32 {
			die("covenant prog must be 32-byte hex")
		}
		order := covenant.Order{
			AssetA:         x, // the covenant sells X
			AssetB:         y, // and wants Y
			RateNum:        in.Covenant.Num,
			RateDen:        in.Covenant.Den,
			MakerProg:      prog,
			MakerVer:       1,
			MinLot:         in.Covenant.MinLot,
			ExpiryLocktime: in.Covenant.Expiry,
			MakerX:         hx32(in.Covenant.MakerX, "makerx"),
			InternalKey:    covenant.NUMS,
		}
		if in.Covenant.Internal != "" {
			order.InternalKey = hx32(in.Covenant.Internal, "internal")
		}
		s, err := bridge.Plan(order, in.Covenant.Locked, in.Covenant.Fill, in.Covenant.K,
			bridge.LNSide{OfferY: in.Lightning.OfferY, WantX: in.Lightning.WantX, Max0Conf: in.Lightning.Max0Conf},
			in.PolicyFeeSats, in.RateY)
		if err != nil {
			die("plan: %v", err)
		}
		out, err := json.Marshal(map[string]interface{}{
			// on-chain FILL leg
			"input_index":     s.Fill.InputIndex,
			"credit_index":    s.Fill.CreditIndex,
			"credit_value":    s.PayY,
			"credit_floor":    s.Fill.RequiredB,
			"credit_asset":    hex.EncodeToString(y[:]),
			"credit_spk":      hex.EncodeToString(makerCreditSPK(prog)),
			"remainder_index": s.Fill.RemainderIndex,
			"partial":         s.Fill.Partial,
			"remainder":       s.Fill.Remainder,
			"remainder_asset": hex.EncodeToString(x[:]),
			"order_spk":       hex.EncodeToString(s.Fill.OrderSPK),
			"fill_leaf":       hex.EncodeToString(s.Fill.FillLeaf),
			"control_block":   hex.EncodeToString(s.Fill.ControlBlock),
			"recv_x":          s.RecvX,
			// Lightning leg (mocked at the orchestrator seam in the proof)
			"ln_deliver_x": s.LNDeliverX,
			"ln_recv_y":    s.LNRecvY,
			// economics + fronting decision
			"fee_y":           s.FeeY,
			"min_fee_y":       s.MinFeeY,
			"front_amount":    s.FrontAmount,
			"max_0conf":       s.Max0Conf,
			"zero_conf_front": s.ZeroConfFront,
			"require_confs":   s.RequireConfs,
			"front_reason":    s.FrontReason,
		})
		if err != nil {
			die("marshal: %v", err)
		}
		fmt.Println(string(out))
	case "run":
		die("run: the always-online bridge loop subscribes to the relay's CrossRail " +
			"matches and drives the submarine orchestrator (pkg/xchain) for the LN leg + " +
			"an Elements node for the on-chain FILL; not built as a standalone loop here " +
			"(needs the relay match feed + a funded LN node + wallet, off the regtest path)")
	default:
		die("unknown subcommand %q (want plan)", os.Args[1])
	}
}
