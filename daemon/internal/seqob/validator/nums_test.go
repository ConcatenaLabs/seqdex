package validator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// A tapscript covenant offer is listed only under the NUMS internal key, or
// with none (NUMS is the default): any other key is a key path that takes the
// order back from under a taker.
func TestCovenantInternalKeyMustBeNUMS(t *testing.T) {
	v := New(cfg(), nil)
	check := func(name string, ik []byte, want string) {
		t.Helper()
		o := signed(t, key(t), func(o *seqobv1.Offer) {
			ct := covTerms(45, 100, 1)
			ct.Covenant.InternalKey = ik
			o.Settlement = ct
		})
		err := v.ValidateOffer(context.Background(), o, "")
		if want == "" && err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	check("NUMS", covenant.NUMS[:], "")
	check("no internal key", nil, "")
	check("a maker's key", bytes.Repeat([]byte{0xd4}, 32), "is not the NUMS point: it is a key path")
	nearly := append([]byte{}, covenant.NUMS[:]...)
	nearly[31] ^= 1
	check("NUMS with one bit changed", nearly, "is not the NUMS point")
}
