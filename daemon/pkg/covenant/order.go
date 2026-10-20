package covenant

import (
	"bytes"
	"fmt"
	"math"
	"math/bits"
)

// Order captures everything a taker needs to (a) verify a resting covenant
// UTXO trustlessly and (b) build the permissionless FILL spend. All asset ids
// and keys are INTERNAL byte order (32 bytes), matching on-chain introspection.
type Order struct {
	AssetA    [32]byte
	AssetB    [32]byte
	RateNum   uint64
	RateDen   uint64
	MakerProg []byte // 32-byte v1 witness program credited on a fill: a plain key, never an Arca leaf or board script
	MakerVer  int
	MinLot    uint64

	ExpiryLocktime int64    // REFUND lock: a height below LocktimeThreshold, a median time from it
	MakerX         [32]byte // maker x-only key authorizing the REFUND leaf
	InternalKey    [32]byte // taproot internal key (NUMS default => no key-path spend)
}

// LocktimeThreshold splits an absolute lock as CLTV reads it: below it a block
// height, from it a median time past in unix seconds. An order's
// ExpiryLocktime may be either; the same REFUND leaf takes both.
const LocktimeThreshold = 500_000_000

// RefundIsTime reports whether the order's REFUND lock is a median time.
func (o Order) RefundIsTime() bool { return o.ExpiryLocktime >= LocktimeThreshold }

// RefundOpen reports whether the order's maker can refund it now, at a tip of
// height tipHeight whose median time past is tipMTP: a height lock once the tip
// has reached it (the refund goes in a later block), a median-time lock once
// the tip's median time has passed it (BIP 113). known is false for a
// median-time lock when tipMTP is 0 (not read).
func (o Order) RefundOpen(tipHeight, tipMTP int64) (open, known bool) {
	if o.RefundIsTime() {
		if tipMTP <= 0 {
			return false, false
		}
		return tipMTP > o.ExpiryLocktime, true
	}
	return tipHeight >= o.ExpiryLocktime, true
}

// Taptree is the derived taproot commitment for an Order.
type Taptree struct {
	FillLeaf       []byte
	RefundLeaf     []byte
	FillLeafHash   [32]byte
	RefundLeafHash [32]byte
	MerkleRoot     [32]byte
	// FillMerkleBranch is the sibling hash proving the FILL leaf under the root
	// (here: the REFUND leaf hash). It is the control-block merkle path.
	FillMerkleBranch []byte
	OutputKey        [32]byte // x-only taproot output key
	Negflag          byte     // control-block parity bit (0/1)
	ScriptPubKey     []byte   // OP_1 <32-byte output key>
}

// DefaultOrder fills in the conventional defaults (NUMS internal key, v1 maker
// payout) and returns an Order ready for Derive.
func DefaultOrder(assetA, assetB [32]byte, rateNum, rateDen, minLot uint64, makerProg []byte, expiry int64, makerX [32]byte) Order {
	return Order{
		AssetA: assetA, AssetB: assetB,
		RateNum: rateNum, RateDen: rateDen,
		MakerProg: makerProg, MakerVer: 1, MinLot: minLot,
		ExpiryLocktime: expiry, MakerX: makerX, InternalKey: NUMS,
	}
}

// Derive builds the {FILL, REFUND} taptree, its Merkle root, the tweaked output
// key, and the scriptPubKey. It mirrors the Python order_taptree exactly:
// leaf order [fill, refund], TapBranch over the two leaf hashes sorted
// ascending, TapTweak/elements over (internal_key || root).
func (o Order) Derive() (*Taptree, error) {
	if o.InternalKey == ([32]byte{}) {
		o.InternalKey = NUMS
	}
	fill, err := BuildFillLeaf(o.AssetA, o.AssetB, o.RateNum, o.RateDen, o.MinLot, o.MakerProg, o.MakerVer)
	if err != nil {
		return nil, err
	}
	refund, err := BuildRefundLeaf(o.ExpiryLocktime, o.MakerX)
	if err != nil {
		return nil, err
	}
	fillH := LeafHash(fill)
	refundH := LeafHash(refund)

	// TapBranch inputs are sorted ascending (BIP341); the FILL leaf's control
	// merkle branch is simply the REFUND leaf hash (its only sibling).
	lo, hi := fillH, refundH
	if bytes.Compare(hi[:], lo[:]) < 0 {
		lo, hi = hi, lo
	}
	root := taggedHash("TapBranch/elements", lo[:], hi[:])

	outKey, neg, err := tweakOutputKey(o.InternalKey, root)
	if err != nil {
		return nil, err
	}
	spk := append([]byte{op1, 0x20}, outKey[:]...)
	return &Taptree{
		FillLeaf: fill, RefundLeaf: refund,
		FillLeafHash: fillH, RefundLeafHash: refundH,
		MerkleRoot:       root,
		FillMerkleBranch: refundH[:],
		OutputKey:        outKey,
		Negflag:          neg,
		ScriptPubKey:     spk,
	}, nil
}

// ControlBlock returns the taproot control block for a FILL script-path spend:
// (leaf_version | parity) || internal_key || merkle_branch. 65 bytes for the
// two-leaf tree.
func (t *Taptree) ControlBlock(internalKey [32]byte) []byte {
	cb := make([]byte, 0, 1+32+len(t.FillMerkleBranch))
	cb = append(cb, byte(leafVersion)+t.Negflag)
	cb = append(cb, internalKey[:]...)
	cb = append(cb, t.FillMerkleBranch...)
	return cb
}

// FillWitness is the covenant input's witness stack for a FILL spend. FILL is
// fully introspection-driven, so the stack is just [leaf, control_block].
func (t *Taptree) FillWitness(internalKey [32]byte) [][]byte {
	return [][]byte{t.FillLeaf, t.ControlBlock(internalKey)}
}

// VerifyAgainstSPK is the trustless-verify a taker MUST run before filling: it
// reconstructs the output key from the advertised constants and confirms it
// equals the on-chain scriptPubKey (OP_1 <32>). It also rejects a non-NUMS
// internal key unless makerCancellableOK is set (a non-NUMS key is a hidden
// maker key-path cancel/rug — see the design red-team, finding 1).
func (o Order) VerifyAgainstSPK(onchainSPK []byte, makerCancellableOK bool) (*Taptree, error) {
	if !makerCancellableOK && o.InternalKey != NUMS {
		return nil, fmt.Errorf("non-NUMS internal key: order is maker-cancellable (key-path rug risk); reject unless explicitly allowed")
	}
	t, err := o.Derive()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(t.ScriptPubKey, onchainSPK) {
		return nil, fmt.Errorf("reconstructed spk %x != on-chain spk %x", t.ScriptPubKey, onchainSPK)
	}
	return t, nil
}

// CeilPrice is required_B = ceil(filled * rateNum / rateDen), rounding in the
// maker's favour, matching the FILL leaf's on-chain arithmetic. The product is
// formed in 128 bits: a wrapped uint64 product would silently size a fill the
// chain rejects (or, past 2^63, one the leaf cannot even compute — see
// CheckArithmetic). Callers gate on CheckArithmetic; this never returns a
// wrapped value.
func CeilPrice(filled, rateNum, rateDen uint64) uint64 {
	hi, lo := bits.Mul64(filled, rateNum)
	lo, carry := bits.Add64(lo, rateDen-1, 0)
	hi += carry
	if hi >= rateDen {
		return math.MaxUint64 // quotient does not fit; CheckArithmetic refuses such orders
	}
	q, _ := bits.Div64(hi, lo, rateDen)
	return q
}

// MaxLeafProduct is the largest filled*rateNum + rateDen - 1 the FILL leaf can
// evaluate: OP_MUL64/OP_ADD64 are signed 64-bit and push false on overflow, after
// which the leaf's OP_VERIFY fails for every fill, full or partial, and the locked
// asset is reachable only through REFUND at expiry.
const MaxLeafProduct = math.MaxInt64

// CheckArithmetic reports whether a fill of up to `locked` atoms stays within the
// leaf's arithmetic. A maker that locks 1e10 atoms against a coprime 4e11-atom
// price bakes rateNum ≈ 4e11 into the leaf; the product is 4e21, and the order is
// dead on arrival. Every planner and the relay validator gate on this.
func (o Order) CheckArithmetic(locked uint64) error {
	if o.RateNum < 1 || o.RateDen < 1 {
		return fmt.Errorf("rate %d/%d: numerator and denominator must be >= 1", o.RateNum, o.RateDen)
	}
	hi, lo := bits.Mul64(locked, o.RateNum)
	lo, carry := bits.Add64(lo, o.RateDen-1, 0)
	hi += carry
	if hi != 0 || lo > MaxLeafProduct {
		return fmt.Errorf("locked %d * rate_num %d + rate_den-1 overflows the FILL leaf's signed 64-bit arithmetic (max %d); reduce the rate by its gcd or the order size", locked, o.RateNum, uint64(MaxLeafProduct))
	}
	return nil
}
