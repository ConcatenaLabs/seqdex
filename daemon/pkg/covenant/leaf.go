// Package covenant is the Go port of the proven SeqOB FILL/REFUND covenant
// primitive (test/functional/seqob_covenant.py in the Sequentia tree,
// proven byte-for-byte by feature_seqob_covenant_fill.py's 11 scenarios).
//
// A maker locks N units of explicit asset A in ONE taproot UTXO ("the order is
// the coin"): internal key = NUMS (no key-path spend) and a two-leaf tree
// {FILL, REFUND}.
//
//   - FILL is permissionless (NO maker signature, zero witness data beyond
//     [leaf, control_block]). It enforces, entirely from transaction
//     introspection, that the covenant input at consensus index k credits the
//     maker at output 2k (asset == B, spk == maker_prog, value >=
//     ceil(filled*num/den)) and, for a partial fill, re-pays the remainder of
//     asset A to the SAME covenant scriptPubKey at output 2k+1 (>= min_lot).
//   - REFUND is <expiry> CLTV DROP <P_maker> CHECKSIG: the maker reclaims after
//     the absolute-locktime expiry, a block height or (from 500,000,000) a
//     median time past.
//
// Every order parameter (asset ids, rate, maker payout program, min_lot) is a
// compile-time constant baked into the FILL leaf, so it is committed inside the
// taproot output key: a taker can only satisfy the terms, never alter them.
//
// This file reproduces the exact tapscript byte sequence, tapleaf/tapbranch/
// taptweak tagged hashes ("*/elements" tags, LEAF_VERSION_TAPSCRIPT 0xc4), and
// the BIP341 output-key tweak of the Python builders. leaf_test.go pins the
// output against a golden vector generated from that proven Python so the
// production artifact is provably identical to the audited one.
package covenant

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// NUMS is the BIP341 nothing-up-my-sleeve internal key (no known discrete log),
// so a NUMS-internal-key output has no key-path spend. Same constant the Python
// builder pins.
var NUMS = mustHex32("50929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0")

// leafVersion is Elements/Sequentia LEAF_VERSION_TAPSCRIPT (Bitcoin uses 0xc0).
const leafVersion = 0xc4

// Elements/Sequentia tapscript opcodes used by the covenant leaves. Values are
// the on-wire bytes from the Sequentia test_framework script.py.
const (
	op0                         = 0x00
	op1                         = 0x51
	opIf                        = 0x63
	opElse                      = 0x67
	opEndif                     = 0x68
	opVerify                    = 0x69
	opDrop                      = 0x75
	opDup                       = 0x76
	opNip                       = 0x77
	opRot                       = 0x7b
	opSwap                      = 0x7c
	opEqual                     = 0x87
	opEqualVerify               = 0x88
	op1Add                      = 0x8b
	opAdd                       = 0x93
	opLessThan                  = 0x9f
	opChecksig                  = 0xac
	opCheckLockTimeVerify       = 0xb1
	opInspectInputValue         = 0xc9
	opInspectInputScriptPubkey  = 0xca
	opPushCurrentInputIndex     = 0xcd
	opInspectOutputAsset        = 0xce
	opInspectOutputValue        = 0xcf
	opInspectOutputScriptPubkey = 0xd1
	opInspectNumOutputs         = 0xd5
	opAdd64                     = 0xd7
	opSub64                     = 0xd8
	opMul64                     = 0xd9
	opDiv64                     = 0xda
	opGreaterThanOrEqual64      = 0xdf
)

// creditIdx / remIdx recompute the input-bound output indices from
// OP_PUSHCURRENTINPUTINDEX each time (2k for the maker credit, 2k+1 for the
// remainder), exactly as the Python _CREDIT_IDX / _REM_IDX helpers.
var (
	creditIdx = []byte{opPushCurrentInputIndex, opDup, opAdd}
	remIdx    = []byte{opPushCurrentInputIndex, opDup, opAdd, op1Add}
)

// scriptBuilder appends opcodes and minimally-encoded pushes, reproducing the
// Python CScript serialization for exactly the forms the covenant leaves use.
type scriptBuilder struct{ b []byte }

func (s *scriptBuilder) op(o byte)     { s.b = append(s.b, o) }
func (s *scriptBuilder) ops(o ...byte) { s.b = append(s.b, o...) }
func (s *scriptBuilder) raw(p []byte)  { s.b = append(s.b, p...) }

// push encodes a data push exactly as CScriptOp.encode_op_pushdata: a direct
// length-prefixed push for < 0x4c bytes (the only case the leaves need; 8-byte
// LE operands and 32-byte asset/program pushes).
func (s *scriptBuilder) push(d []byte) {
	switch {
	case len(d) < 0x4c:
		s.b = append(s.b, byte(len(d)))
	case len(d) <= 0xff:
		s.b = append(s.b, 0x4c, byte(len(d)))
	case len(d) <= 0xffff:
		s.b = append(s.b, 0x4d, byte(len(d)), byte(len(d)>>8))
	default:
		s.b = append(s.b, 0x4e, byte(len(d)), byte(len(d)>>8), byte(len(d)>>16), byte(len(d)>>24))
	}
	s.b = append(s.b, d...)
}

// pushNum pushes a signed integer as CScript does: OP_0/OP_1..OP_16 for small
// values, else a minimally-encoded (little-endian, sign-in-top-bit) push. Used
// for the REFUND leaf's absolute expiry (a height, or a median time).
func (s *scriptBuilder) pushNum(n int64) {
	if n == 0 {
		s.op(op0)
		return
	}
	if n >= 1 && n <= 16 {
		s.op(byte(op1 + n - 1))
		return
	}
	if n == -1 {
		s.op(0x4f) // OP_1NEGATE
		return
	}
	s.push(bn2vch(n))
}

// bn2vch is the Bitcoin-specific little-endian sign-magnitude encoding.
func bn2vch(v int64) []byte {
	if v == 0 {
		return nil
	}
	neg := v < 0
	av := uint64(v)
	if neg {
		av = uint64(-v)
	}
	var out []byte
	for av > 0 {
		out = append(out, byte(av&0xff))
		av >>= 8
	}
	if out[len(out)-1]&0x80 != 0 {
		if neg {
			out = append(out, 0x80)
		} else {
			out = append(out, 0x00)
		}
	} else if neg {
		out[len(out)-1] |= 0x80
	}
	return out
}

// le8 is a 64-bit little-endian operand for the OP_*64 arithmetic opcodes.
func le8(n uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	return b[:]
}

// BuildFillLeaf returns the permissionless FILL tapscript for one resting order.
//
// assetA/assetB are 32-byte INTERNAL-order asset ids (the resting asset and the
// payment asset), matching what OP_INSPECTOUTPUTASSET returns on-chain.
// makerProg is the maker credit output's witness program (32 bytes for the v1
// taproot payout this builder pins). required_B = ceil(filled*rateNum/rateDen);
// minLot floors both `filled` and any remainder.
func BuildFillLeaf(assetA, assetB [32]byte, rateNum, rateDen, minLot uint64, makerProg []byte, makerVer int) ([]byte, error) {
	if rateNum < 1 || rateDen < 1 || minLot < 1 {
		return nil, fmt.Errorf("rateNum, rateDen, minLot must be >= 1")
	}
	if err := (Order{RateNum: rateNum, RateDen: rateDen, MinLot: minLot}).CheckTerms(); err != nil {
		return nil, err
	}
	if makerVer != 1 {
		return nil, fmt.Errorf("this builder pins a v1 taproot maker payout (got version %d)", makerVer)
	}
	if len(makerProg) != 32 {
		return nil, fmt.Errorf("makerProg must be 32 bytes for a v1 taproot payout (got %d)", len(makerProg))
	}
	s := &scriptBuilder{}

	// locked = this covenant input's own value (must be explicit).
	s.ops(opPushCurrentInputIndex, opInspectInputValue) // [locked8, prefix]
	s.ops(op1, opEqualVerify)                           // [locked8]

	// remainder = asset A re-paid to output 2k+1 (0 for a full fill).
	s.raw(remIdx)
	s.ops(opInspectNumOutputs, opLessThan) // [locked8, (2k+1 < numouts)]
	s.op(opIf)
	{
		s.raw(remIdx)
		s.op(opInspectOutputAsset) // [locked8, asset32, prefix]
		s.ops(op1, opEqualVerify)  // explicit
		s.push(assetA[:])
		s.op(opEqual) // [locked8, isA]
		s.op(opIf)
		{
			// asset A at 2k+1: self-replicate spk, floor, read value.
			s.raw(remIdx)
			s.op(opInspectOutputScriptPubkey)
			s.ops(opPushCurrentInputIndex, opInspectInputScriptPubkey)
			s.ops(opRot, opEqualVerify, opEqualVerify) // outver==inver, outprog==inprog
			s.raw(remIdx)
			s.ops(opInspectOutputValue, op1, opEqualVerify) // [locked8, remainder8]
			s.op(opDup)
			s.push(le8(minLot))
			s.ops(opGreaterThanOrEqual64, opVerify)
		}
		s.op(opElse)
		{
			s.push(le8(0)) // remainder = 0
		}
		s.op(opEndif)
	}
	s.op(opElse)
	{
		s.push(le8(0)) // full fill, remainder = 0
	}
	s.op(opEndif)

	// filled = locked - remainder, floored by min_lot.
	s.ops(opSub64, opVerify)
	s.op(opDup)
	s.push(le8(minLot))
	s.ops(opGreaterThanOrEqual64, opVerify)

	// required_B = ceil(filled*num/den) = floor((filled*num + den-1)/den).
	s.push(le8(rateNum))
	s.ops(opMul64, opVerify)
	s.push(le8(rateDen - 1))
	s.ops(opAdd64, opVerify)
	s.push(le8(rateDen))
	s.ops(opDiv64, opVerify, opNip)

	// credit output at 2k: asset == B, spk == maker, value >= required.
	s.raw(creditIdx)
	s.ops(opInspectOutputAsset, op1, opEqualVerify)
	s.push(assetB[:])
	s.op(opEqualVerify)
	s.raw(creditIdx)
	s.ops(opInspectOutputScriptPubkey, op1, opEqualVerify)
	s.push(makerProg)
	s.op(opEqualVerify)
	s.raw(creditIdx)
	s.ops(opInspectOutputValue, op1, opEqualVerify)
	s.ops(opSwap, opGreaterThanOrEqual64)
	return s.b, nil
}

// BuildRefundLeaf returns the REFUND tapscript: absolute-CLTV reclaim by the
// maker after expiry, a block height below LocktimeThreshold or a median time
// past from it (CLTV compares the spend's nLockTime, which the maker sets to
// expiryLocktime, against the same kind). makerX is the maker's 32-byte x-only
// pubkey.
func BuildRefundLeaf(expiryLocktime int64, makerX [32]byte) ([]byte, error) {
	if expiryLocktime < 0 {
		return nil, fmt.Errorf("expiryLocktime must be >= 0")
	}
	s := &scriptBuilder{}
	s.pushNum(expiryLocktime)
	s.ops(opCheckLockTimeVerify, opDrop)
	s.push(makerX[:])
	s.op(opChecksig)
	return s.b, nil
}

// --- tagged hashes ---

func taggedHash(tag string, data ...[]byte) [32]byte {
	th := sha256.Sum256([]byte(tag))
	h := sha256.New()
	h.Write(th[:])
	h.Write(th[:])
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// serString is compact-size(len) || bytes, for the tapleaf preimage.
func serString(b []byte) []byte {
	return append(compactSize(uint64(len(b))), b...)
}

func compactSize(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	case n <= 0xffffffff:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	default:
		b := make([]byte, 9)
		b[0] = 0xff
		binary.LittleEndian.PutUint64(b[1:], n)
		return b
	}
}

// LeafHash is TapLeaf/elements over (leaf_version || ser_string(script)).
func LeafHash(script []byte) [32]byte {
	return taggedHash("TapLeaf/elements", []byte{leafVersion}, serString(script))
}

func mustHex32(s string) [32]byte {
	var out [32]byte
	if len(s) != 64 {
		panic("mustHex32: wrong length")
	}
	for i := 0; i < 32; i++ {
		var v byte
		fmt.Sscanf(s[i*2:i*2+2], "%02x", &v)
		out[i] = v
	}
	return out
}

// tweakOutputKey performs the BIP341 tweak with the Elements TapTweak tag:
// lift internalX to its even-Y point P, Q = P + t*G where
// t = TapTweak/elements(internalX || merkleRoot), returning Q's x-only bytes
// and the parity (1 iff Q has odd Y — the control-block sign bit).
func tweakOutputKey(internalX, merkleRoot [32]byte) (out [32]byte, negated byte, err error) {
	ip, err := schnorr.ParsePubKey(internalX[:]) // BIP340 even-Y lift
	if err != nil {
		return out, 0, fmt.Errorf("internal key not a valid x-only point: %w", err)
	}
	t := taggedHash("TapTweak/elements", internalX[:], merkleRoot[:])
	var ts btcec.ModNScalar
	if ts.SetBytes(&t) != 0 {
		return out, 0, fmt.Errorf("tweak >= curve order")
	}
	var tG, ipJ, q btcec.JacobianPoint
	btcec.ScalarBaseMultNonConst(&ts, &tG)
	ip.AsJacobian(&ipJ)
	btcec.AddNonConst(&tG, &ipJ, &q)
	q.ToAffine()
	out = *q.X.Bytes()
	if q.Y.IsOdd() {
		negated = 1
	}
	return out, negated, nil
}
