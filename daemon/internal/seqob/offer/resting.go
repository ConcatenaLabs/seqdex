package offer

import (
	"encoding/hex"
	"fmt"
	"math/bits"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A resting leaf order (RestingLeafTerms) is an order-3 (or order-2) leaf in an Arca
// operator's tree: its maker rests it and leaves, and takers fill it out of
// round through the operator, each fill co-signed by the operator and held to
// the order's terms by the leaf's program. It is not a coin on the chain, so
// none of the covenant helpers (IsCovenant, CovenantOutpoint) apply to it; it
// shares with a covenant order only that it is filled with its maker away.
//
// Its locator, the coin it rests at now and what that coin holds, is outside
// the signed bytes (CanonicalOfferBytes): every fill moves a part-filled order
// to a new coin under a new salt, and the relay follows it there from the
// operator's state view. Its order id, the leaf id of its first coin, is
// signed and never moves.

// IsResting reports whether o is an order resting as a leaf in an operator's
// tree.
func IsResting(o *seqobv1.Offer) bool { return o.GetRestingLeaf() != nil }

// FilledWithoutMaker reports whether o is filled with its maker away: a
// covenant order on the chain, a side of a pool, or an order resting in an
// operator's tree. Such an order needs no live maker connection and holds no
// lift slot.
func FilledWithoutMaker(o *seqobv1.Offer) bool { return IsCovenant(o) || IsPool(o) || IsResting(o) }

// RestingLocator is where a resting leaf order rests now: the coin's leaf id
// and the atoms of asset_a it holds.
func RestingLocator(o *seqobv1.Offer) (coin string, value uint64, ok bool) {
	t := o.GetRestingLeaf()
	if t == nil {
		return "", 0, false
	}
	return t.GetCoin(), t.GetValue(), true
}

// SetRestingLocator points a resting leaf order at another coin, its signed
// terms unchanged; false for any other offer.
func SetRestingLocator(o *seqobv1.Offer, coin string, value uint64) bool {
	t := o.GetRestingLeaf()
	if t == nil {
		return false
	}
	t.Coin, t.Value = strings.ToLower(coin), value
	return true
}

// RestingOrderKey is the one listing a resting leaf order may have in a book:
// its operator's key and its order id. Two offers for one order are one order
// counted twice.
func RestingOrderKey(o *seqobv1.Offer) (string, bool) {
	t := o.GetRestingLeaf()
	if t == nil || t.GetOrderId() == "" {
		return "", false
	}
	return "leaf:" + strings.ToLower(t.GetOperatorKey()) + ":" + strings.ToLower(t.GetOrderId()), true
}

// CheckRestingTerms checks a resting leaf order's terms on their own, and
// against the offer that carries them: the terms are the operator's leaf
// record's, and the offer's price, assets and direction are read from them.
// Hex fields are lowercase, ids 32 bytes; the order is a SELL of asset_a for
// asset_b at exactly rate_num/rate_den; its locator names a coin holding at
// least min_lot and no more than the offer_amount its maker signed.
func CheckRestingTerms(o *seqobv1.Offer) error {
	t := o.GetRestingLeaf()
	if t == nil {
		return fmt.Errorf("no resting leaf order terms")
	}
	if u := t.GetOperatorUrl(); !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("resting_leaf operator_url %q must be an http(s) base URL", u)
	}
	for _, f := range []struct{ name, v string }{
		{"operator_key", t.GetOperatorKey()}, {"order_id", t.GetOrderId()}, {"asset_a", t.GetAssetA()},
		{"asset_b", t.GetAssetB()}, {"coin", t.GetCoin()},
	} {
		if !isHex32(f.v) {
			return fmt.Errorf("resting_leaf %s must be 32-byte lowercase hex", f.name)
		}
	}
	if len(t.GetLandedMaker()) != 32 || len(t.GetRefundKey()) != 32 {
		return fmt.Errorf("resting_leaf landed_maker and refund_key must be 32 bytes")
	}
	if t.GetLandedExpiry() < 500_000_000 {
		return fmt.Errorf("resting_leaf landed_expiry %d is a height: a resting order's REFUND is a median time", t.GetLandedExpiry())
	}
	if t.GetAssetA() == t.GetAssetB() {
		return fmt.Errorf("resting_leaf sells and is paid in the same asset")
	}
	if t.GetRateNum() == 0 || t.GetRateDen() == 0 || t.GetMinLot() == 0 {
		return fmt.Errorf("resting_leaf rate_num, rate_den and min_lot must be >= 1")
	}
	if o.GetTradeDir() != seqobv1.TradeDir_TRADE_DIR_SELL {
		return fmt.Errorf("a resting leaf order is TRADE_DIR_SELL (asset_a is the offered base)")
	}
	if t.GetAssetA() != strings.ToLower(o.GetOfferAsset()) || t.GetAssetB() != strings.ToLower(o.GetWantAsset()) {
		return fmt.Errorf("resting_leaf sells %s for %s, but the offer gives %s for %s", t.GetAssetA(), t.GetAssetB(), o.GetOfferAsset(), o.GetWantAsset())
	}
	if !sameRatio(t.GetRateNum(), t.GetRateDen(), o.GetWantAmount(), o.GetOfferAmount()) {
		return fmt.Errorf("resting_leaf rate %d/%d does not equal the offer's want/offer %d/%d", t.GetRateNum(), t.GetRateDen(), o.GetWantAmount(), o.GetOfferAmount())
	}
	if t.GetMinLot() > o.GetOfferAmount() {
		return fmt.Errorf("resting_leaf min_lot %d exceeds the offer_amount %d", t.GetMinLot(), o.GetOfferAmount())
	}
	if t.GetValue() < t.GetMinLot() {
		return fmt.Errorf("resting_leaf locator holds %d atoms, below the order's min_lot %d: no fill can take it", t.GetValue(), t.GetMinLot())
	}
	if t.GetValue() > o.GetOfferAmount() {
		return fmt.Errorf("resting_leaf locator holds %d atoms, more than the offer_amount %d its maker signed", t.GetValue(), o.GetOfferAmount())
	}
	return nil
}

// sameRatio reports a/b == c/d, exactly, on 128-bit products.
func sameRatio(a, b, c, d uint64) bool {
	h1, l1 := bits.Mul64(a, d)
	h2, l2 := bits.Mul64(b, c)
	return h1 == h2 && l1 == l2
}

func isHex32(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
