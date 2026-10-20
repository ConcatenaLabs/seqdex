package validator

import (
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A tapscript covenant offer whose rate_num, rate_den or min_lot is 2^63 or
// more is refused for that reason, named, before any other check: the FILL
// leaf would read the term as negative and fill the order for any credit. The
// refusal points at the Simplicity order, which takes such sizes and prices.
func TestCovenantTermsAt2to63AreRefused(t *testing.T) {
	v := New(cfg(), nil)
	for _, c := range []struct {
		name          string
		num, den, lot uint64
		want          string
	}{
		// 1:1 at 2^63+1: the offer's 100 for 100 matches the rate exactly.
		{"rate_num and rate_den at 2^63+1", 1<<63 + 1, 1<<63 + 1, 1, "rate_num 9223372036854775809 is 2^63 or more"},
		{"rate_den at 2^64-1", 45, 18446744073709551615, 1, "rate_den 18446744073709551615 is 2^63 or more"},
		{"min_lot at 2^63", 45, 100, 1 << 63, "min_lot 9223372036854775808 is 2^63 or more"},
	} {
		o := signed(t, key(t), func(o *seqobv1.Offer) {
			if c.num == c.den {
				o.WantAmount = o.OfferAmount
			}
			o.Settlement = covTerms(c.num, c.den, c.lot)
		})
		err := v.ValidateOffer(context.Background(), o, "")
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Simplicity order (seqdex/order") {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	// At 2^63-1 the term is read as itself; the order is then judged as any other.
	o := signed(t, key(t), func(o *seqobv1.Offer) { o.Settlement = covTerms(45, 100, 1) })
	if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
		t.Fatal(err)
	}
}
