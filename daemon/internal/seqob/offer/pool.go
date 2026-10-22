package offer

import (
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A pool (PoolTerms) is a maker's two coins under seqdex/pool, one of base and
// one of quote. It posts two offers, its ask (offering base for quote at the
// ask) and its bid (offering quote for base at the bid), both TRADE_DIR_SELL
// and both carrying the whole pool's terms, so that each side is a resting
// order of the book in the direction it sells. Both rest at the same two
// coins; each lists what its own coin holds above that coin's reserve. The
// coins are outside the signed bytes (CanonicalOfferBytes): every fill moves
// the pool to new coins while its maker is away.
//
// A pool is not IsCovenant: the covenant helpers assume one coin and a
// remainder at 2k+1, and a pool's fill re-creates it at 2j and 2j+1 from two
// coins.

// IsPool reports whether o is one side of a pool.
func IsPool(o *seqobv1.Offer) bool { return o.GetPool() != nil }

// PoolOf is the pool a PoolTerms describes, its terms checked
// (covenant.SimPool.Validate) and its template the one this build pins.
func PoolOf(t *seqobv1.PoolTerms) (covenant.SimPool, error) {
	var p covenant.SimPool
	if t == nil {
		return p, fmt.Errorf("no pool terms")
	}
	if !strings.EqualFold(t.GetTemplateHash(), covenant.PoolTemplateHash) {
		return p, fmt.Errorf("template %q is not seqdex/pool %s, the one this build reads", t.GetTemplateHash(), covenant.PoolTemplateHash)
	}
	b, err := hex32(t.GetBase())
	if err != nil {
		return p, fmt.Errorf("base: %w", err)
	}
	q, err := hex32(t.GetQuote())
	if err != nil {
		return p, fmt.Errorf("quote: %w", err)
	}
	if len(t.GetMakerKey()) != 32 {
		return p, fmt.Errorf("maker_key must be 32 bytes")
	}
	p = covenant.SimPool{Base: b, Quote: q, BidNum: t.GetBidNum(), BidDen: t.GetBidDen(), AskNum: t.GetAskNum(),
		AskDen: t.GetAskDen(), MinLot: t.GetMinLot(), ReserveBase: t.GetReserveBase(), ReserveQuote: t.GetReserveQuote()}
	copy(p.MakerKey[:], t.GetMakerKey())
	if err := p.Validate(); err != nil {
		return covenant.SimPool{}, err
	}
	return p, nil
}

// PoolIsAsk reports whether a pool offer is the pool's ask (it offers base);
// otherwise it is the pool's bid (it offers quote).
func PoolIsAsk(o *seqobv1.Offer) bool {
	t := o.GetPool()
	if t == nil {
		return false
	}
	return displayOf(t.GetBase()) == strings.ToLower(o.GetOfferAsset())
}

// PoolCoins is where a pool rests: its base coin and its quote coin (display txids).
func PoolCoins(o *seqobv1.Offer) (baseTxid string, baseVout uint32, quoteTxid string, quoteVout uint32, ok bool) {
	t := o.GetPool()
	if t == nil {
		return "", 0, "", 0, false
	}
	return t.GetBaseTxid(), t.GetBaseVout(), t.GetQuoteTxid(), t.GetQuoteVout(), true
}

// SetPoolCoins points a pool offer at other coins, its terms unchanged.
func SetPoolCoins(o *seqobv1.Offer, baseTxid string, baseVout uint32, quoteTxid string, quoteVout uint32) bool {
	t := o.GetPool()
	if t == nil {
		return false
	}
	t.BaseTxid, t.BaseVout, t.QuoteTxid, t.QuoteVout = baseTxid, baseVout, quoteTxid, quoteVout
	return true
}

// PoolOwnCoin is the coin a pool offer lists from: the base coin for the ask,
// the quote coin for the bid. The relay lists one offer per such coin.
func PoolOwnCoin(o *seqobv1.Offer) (string, uint32, bool) {
	bt, bv, qt, qv, ok := PoolCoins(o)
	if !ok {
		return "", 0, false
	}
	if PoolIsAsk(o) {
		return bt, bv, true
	}
	return qt, qv, true
}

// PoolKey names the pool an offer belongs to: its terms. Two offers of one
// pool share it.
func PoolKey(o *seqobv1.Offer) (string, bool) {
	p, err := PoolOf(o.GetPool())
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("pool:%x", p.Terms()), true
}

// displayOf reverses an internal-order asset id hex into its display form.
func displayOf(internal string) string {
	b := []byte(strings.ToLower(internal))
	if len(b) != 64 {
		return string(b)
	}
	out := make([]byte, 64)
	for i := 0; i < 32; i++ {
		out[2*i], out[2*i+1] = b[62-2*i], b[63-2*i]
	}
	return string(out)
}
