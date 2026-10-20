// Command seqob-covenant exposes the production Go covenant builder
// (daemon/pkg/covenant) as a small JSON CLI so an external driver (the regtest
// functional test, a wallet, or a settler) can derive a resting covenant order's
// scriptPubKey and build the permissionless FILL spend WITHOUT re-implementing
// the tapscript. Everything it emits is the exact byte artifact
// pkg/covenant/leaf_test.go pins against the proven Python.
//
// Asset ids, programs and keys are INTERNAL byte order (32-byte hex), matching
// on-chain introspection (i.e. display-hex asset ids reversed).
//
// Subcommands:
//
//	derive  -a A -b B -num N -den D -minlot M -prog P -expiry E -makerx X [-internal K]
//	    -> {fill_leaf, refund_leaf, fill_leaf_hash, refund_leaf_hash,
//	        merkle_root, merkle_branch, output_key, negflag, scriptpubkey,
//	        control_block}
//
//	fill    (derive flags) -locked L -filled F -k K
//	    -> {input_index, credit_index, remainder_index, filled, remainder,
//	        required_b, partial, fill_leaf, control_block, order_spk}
//
//	filltx  < spec.json
//	    Lays out a transaction filling several orders, order k at input k
//	    (pkg/covenant BuildFillTx; the spec format is covenant.Spec, as
//	    pkg/covenant/testdata/fill_vectors.json carries it), runs every order's
//	    leaf over it, and prints {hex, txid, plans}. The orders' inputs carry
//	    their FILL witnesses; the other inputs are left unsigned.
//
//	check   (derive flags) -tx HEX -k K -locked L
//	    Judges a transaction as the order's FILL leaf at input k would
//	    (pkg/covenant CheckFill): {ok, filled, remainder, partial, required,
//	    credited, credit_index, remainder_index} or {ok:false, error}.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

func die(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "seqob-covenant: "+format+"\n", a...)
	os.Exit(1)
}

func hex32(s, name string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		die("%s must be 32-byte hex (got %q)", name, s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func orderFlags(fs *flag.FlagSet) func() covenant.Order {
	a := fs.String("a", "", "asset A (32-byte internal-order hex)")
	b := fs.String("b", "", "asset B (32-byte internal-order hex)")
	num := fs.Uint64("num", 0, "rate numerator")
	den := fs.Uint64("den", 0, "rate denominator")
	minlot := fs.Uint64("minlot", 0, "min_lot (atoms)")
	prog := fs.String("prog", "", "maker credit witness program (32-byte hex, v1): a plain key, never an Arca leaf or board script")
	makerver := fs.Int("makerver", 1, "maker credit witness version")
	expiry := fs.Int64("expiry", 0, "REFUND absolute locktime: a height below 500000000, a median time (unix seconds) from it")
	makerx := fs.String("makerx", "", "maker x-only key for REFUND (32-byte hex)")
	internal := fs.String("internal", "", "taproot internal key (32-byte hex; default NUMS)")
	return func() covenant.Order {
		progB, err := hex.DecodeString(*prog)
		if err != nil || len(progB) != 32 {
			die("-prog must be 32-byte hex")
		}
		o := covenant.Order{
			AssetA:         hex32(*a, "-a"),
			AssetB:         hex32(*b, "-b"),
			RateNum:        *num,
			RateDen:        *den,
			MakerProg:      progB,
			MakerVer:       *makerver,
			MinLot:         *minlot,
			ExpiryLocktime: *expiry,
			MakerX:         hex32(*makerx, "-makerx"),
			InternalKey:    covenant.NUMS,
		}
		if *internal != "" {
			o.InternalKey = hex32(*internal, "-internal")
		}
		return o
	}
}

func emit(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		die("marshal: %v", err)
	}
	fmt.Println(string(b))
}

func main() {
	if len(os.Args) < 2 {
		die("usage: seqob-covenant <derive|fill|filltx|check> [flags]")
	}
	switch os.Args[1] {
	case "derive":
		fs := flag.NewFlagSet("derive", flag.ExitOnError)
		get := orderFlags(fs)
		fs.Parse(os.Args[2:])
		o := get()
		t, err := o.Derive()
		if err != nil {
			die("derive: %v", err)
		}
		emit(map[string]interface{}{
			"fill_leaf":        hex.EncodeToString(t.FillLeaf),
			"refund_leaf":      hex.EncodeToString(t.RefundLeaf),
			"fill_leaf_hash":   hex.EncodeToString(t.FillLeafHash[:]),
			"refund_leaf_hash": hex.EncodeToString(t.RefundLeafHash[:]),
			"merkle_root":      hex.EncodeToString(t.MerkleRoot[:]),
			"merkle_branch":    hex.EncodeToString(t.FillMerkleBranch),
			"output_key":       hex.EncodeToString(t.OutputKey[:]),
			"negflag":          t.Negflag,
			"scriptpubkey":     hex.EncodeToString(t.ScriptPubKey),
			"control_block":    hex.EncodeToString(t.ControlBlock(o.InternalKey)),
		})
	case "fill":
		fs := flag.NewFlagSet("fill", flag.ExitOnError)
		get := orderFlags(fs)
		locked := fs.Uint64("locked", 0, "atoms of A currently locked in the covenant UTXO")
		filled := fs.Uint64("filled", 0, "atoms of A to take")
		k := fs.Uint("k", 0, "covenant input consensus index")
		fs.Parse(os.Args[2:])
		o := get()
		p, err := o.PlanFill(*locked, *filled, uint32(*k))
		if err != nil {
			die("plan: %v", err)
		}
		emit(map[string]interface{}{
			"input_index":     p.InputIndex,
			"credit_index":    p.CreditIndex,
			"remainder_index": p.RemainderIndex,
			"filled":          p.Filled,
			"remainder":       p.Remainder,
			"required_b":      p.RequiredB,
			"partial":         p.Partial,
			"fill_leaf":       hex.EncodeToString(p.FillLeaf),
			"control_block":   hex.EncodeToString(p.ControlBlock),
			"order_spk":       hex.EncodeToString(p.OrderSPK),
		})
	case "filltx":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			die("read stdin: %v", err)
		}
		var spec covenant.Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			die("parse spec: %v", err)
		}
		f, err := spec.FillTx()
		if err != nil {
			die("spec: %v", err)
		}
		tx, plans, err := covenant.BuildFillTx(f)
		if err != nil {
			die("filltx: %v", err)
		}
		txHex, err := tx.ToHex()
		if err != nil {
			die("serialize: %v", err)
		}
		var ps []map[string]interface{}
		for _, p := range plans {
			ps = append(ps, map[string]interface{}{
				"input_index": p.InputIndex, "credit_index": p.CreditIndex, "remainder_index": p.RemainderIndex,
				"filled": p.Filled, "remainder": p.Remainder, "required_b": p.RequiredB, "partial": p.Partial,
			})
		}
		emit(map[string]interface{}{"hex": txHex, "txid": tx.TxHash().String(), "plans": ps})
	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		get := orderFlags(fs)
		txHex := fs.String("tx", "", "the transaction (hex)")
		locked := fs.Uint64("locked", 0, "atoms of A the order's output holds")
		k := fs.Uint("k", 0, "the order's input index")
		fs.Parse(os.Args[2:])
		o := get()
		raw, err := hex.DecodeString(*txHex)
		if err != nil {
			die("-tx: %v", err)
		}
		// Read with covenant.ReadOutputs, not go-elements, which cannot read a
		// Sequentia issuance (an Arca round issues its batch token).
		outs, err := covenant.ReadOutputs(raw)
		if err != nil {
			die("-tx: %v", err)
		}
		c, err := o.CheckFill(outs, uint32(*k), *locked)
		if err != nil {
			emit(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		emit(map[string]interface{}{"ok": true, "filled": c.Filled, "remainder": c.Remainder, "partial": c.Partial,
			"required": c.Required, "credited": c.Credited, "credit_index": c.CreditIndex, "remainder_index": c.RemainderIndex})
	default:
		die("unknown subcommand %q (want derive|fill|filltx|check)", os.Args[1])
	}
}
