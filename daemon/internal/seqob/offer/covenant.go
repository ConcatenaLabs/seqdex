package offer

import (
	"encoding/hex"
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A covenant offer is a funded order resting on the chain, which anyone fills
// with its maker offline. It carries one of two terms members: CovenantTerms,
// the tapscript FILL/REFUND order, or SimplicityCovenantTerms, the Simplicity
// order of the template seqdex/order. The relay treats both alike wherever it
// asks only whether an offer is funded on the chain and where it rests; these
// helpers answer that for either member.

// IsCovenant reports whether o is a funded resting order, under either leaf.
func IsCovenant(o *seqobv1.Offer) bool {
	return o.GetCovenant() != nil || o.GetSimplicityCovenant() != nil
}

// CovenantOutpoint is where a covenant offer rests (display txid).
func CovenantOutpoint(o *seqobv1.Offer) (txid string, vout uint32, ok bool) {
	if t := o.GetCovenant(); t != nil {
		return t.GetCovenantTxid(), t.GetCovenantVout(), true
	}
	if t := o.GetSimplicityCovenant(); t != nil {
		return t.GetCovenantTxid(), t.GetCovenantVout(), true
	}
	return "", 0, false
}

// SetCovenantOutpoint points a covenant offer at another outpoint, its terms
// unchanged; false for any other offer. The outpoint is outside the signed
// bytes, so the maker's signature still verifies.
func SetCovenantOutpoint(o *seqobv1.Offer, txid string, vout uint32) bool {
	if t := o.GetCovenant(); t != nil {
		t.CovenantTxid, t.CovenantVout = txid, vout
		return true
	}
	if t := o.GetSimplicityCovenant(); t != nil {
		t.CovenantTxid, t.CovenantVout = txid, vout
		return true
	}
	return false
}

// CovenantMinLot is a covenant offer's min_lot, 0 for any other offer.
func CovenantMinLot(o *seqobv1.Offer) uint64 {
	if t := o.GetCovenant(); t != nil {
		return t.GetMinLot()
	}
	return o.GetSimplicityCovenant().GetMinLot()
}

// SimOrderOf is the Simplicity order a SimplicityCovenantTerms describes, its
// terms checked (covenant.SimOrder.Validate) and its template the one this
// build pins. Asset ids are internal-order hex.
func SimOrderOf(t *seqobv1.SimplicityCovenantTerms) (covenant.SimOrder, error) {
	var o covenant.SimOrder
	if t == nil {
		return o, fmt.Errorf("no simplicity terms")
	}
	if !strings.EqualFold(t.GetTemplateHash(), covenant.SimOrderTemplateHash) {
		return o, fmt.Errorf("template %q is not seqdex/order %s, the one this build fills", t.GetTemplateHash(), covenant.SimOrderTemplateHash)
	}
	a, err := hex32(t.GetAssetA())
	if err != nil {
		return o, fmt.Errorf("asset_a: %w", err)
	}
	b, err := hex32(t.GetAssetB())
	if err != nil {
		return o, fmt.Errorf("asset_b: %w", err)
	}
	if len(t.GetRefundKey()) != 32 {
		return o, fmt.Errorf("refund_key must be 32 bytes")
	}
	o = covenant.SimOrder{
		AssetA: a, AssetB: b, RateNum: t.GetRateNum(), RateDen: t.GetRateDen(), MinLot: t.GetMinLot(),
		MakerScript: append([]byte{}, t.GetMakerScript()...), Expiry: t.GetExpiryLocktime(),
	}
	copy(o.RefundKey[:], t.GetRefundKey())
	if err := o.Validate(); err != nil {
		return covenant.SimOrder{}, err
	}
	return o, nil
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("must be 32-byte hex")
	}
	copy(out[:], b)
	return out, nil
}
