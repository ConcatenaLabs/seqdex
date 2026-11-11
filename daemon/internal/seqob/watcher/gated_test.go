package watcher

import (
	"bytes"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

func gatedTerms() *seqobv1.GatedOrderTerms {
	return &seqobv1.GatedOrderTerms{
		CovenantTxid: strings.Repeat("ab", 32), AssetA: strings.Repeat("0a", 32), AssetB: strings.Repeat("0b", 32),
		RateNum: 1000, RateDen: 1, MinLot: 10, MakerProg: bytes.Repeat([]byte{0xc3}, 32), MakerProgVer: 1,
		Expiry: 1_800_000_000, PolicyX: bytes.Repeat([]byte{0x02}, 32), MakerEnclaveProg: bytes.Repeat([]byte{0xe5}, 32),
		PolicyServer: "https://policy.example", MakerAid: strings.Repeat("aa", 20),
	}
}

// A spend through the policy gate is a fill: the credit at 2k, the rest at
// 2k+1 when it is the order's own script. The keyless refund is no fill.
func TestReadGatedFill(t *testing.T) {
	o, err := offer.GatedOrderOf(gatedTerms())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := o.Tree()
	if err != nil {
		t.Fatal(err)
	}
	outs := []covenant.Output{
		covenant.Explicit(o.AssetB, 100_000, append([]byte{0x51, 0x20}, o.MakerProg...)),
		covenant.Explicit(o.AssetA, 400, tr.ScriptPubKey),
		covenant.Explicit(o.AssetA, 100, []byte{0x51, 0x20, 9}),
	}
	fill := [][]byte{make([]byte, 64), tr.FillLeaf, tr.FillControl}
	r, err := ReadGatedFill(o, &SpenderTx{Txid: "t", Witness: fill, Outputs: outs}, 500)
	if err != nil || !r.Partial || r.Filled != 100 || r.Remainder != 400 || r.Refund {
		t.Fatalf("a partial fill: %v %+v", err, r)
	}
	// The rest at another script is no rest: the whole order was filled.
	outs[1] = covenant.Explicit(o.AssetA, 400, []byte{0x51, 0x20, 8})
	if r, _ := ReadGatedFill(o, &SpenderTx{Txid: "t", Witness: fill, Outputs: outs}, 500); r.Partial || r.Filled != 500 {
		t.Fatalf("a rest elsewhere: %+v", r)
	}
	if r, _ := ReadGatedFill(o, &SpenderTx{Txid: "t", Witness: [][]byte{tr.RefundLeaf, tr.FillControl}, Outputs: outs}, 500); !r.Refund {
		t.Fatalf("the refund read as a fill: %+v", r)
	}
	// The watcher derives the script it expects at the outpoint from the terms.
	exp, err := CovEntry{Gated: gatedTerms()}.expect()
	if err != nil || exp.MinLot != 10 || !strings.EqualFold(exp.OrderSPKHex, hexOf(tr.ScriptPubKey)) {
		t.Fatalf("expect: %v %+v", err, exp)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// mtpChain is a fake chain whose tip carries a median time past.
type mtpChain struct {
	*fakeChain
	mtp int64
}

func (c mtpChain) TipMedianTime() (int64, error) { return c.mtp, nil }

// gatedBook records the gated orders the watcher delists.
type gatedBook struct {
	*fakeBook
	delisted []offerstore.Key
}

func (b *gatedBook) RemoveGatedExpired(k offerstore.Key) error {
	b.delisted = append(b.delisted, k)
	return nil
}

// PV5 Q1: a gated order stops resting at its expiry less the margin, by the
// later of the clock and the chain's median time, even while its coin is
// unspent: its policy server refuses every fill from then.
func TestGatedOrderDelistedAtItsExpiry(t *testing.T) {
	terms := gatedTerms()
	o := &seqobv1.Offer{OfferId: "G", Settlement: &seqobv1.Offer_GatedOrder{GatedOrder: terms}}
	k := offerstore.Key{MakerPubkey: "m", OfferID: "G"}
	for _, c := range []struct {
		name   string
		mtp    int64
		delist bool
	}{
		{"a day before", int64(terms.GetExpiry()) - 86400, false},
		{"the median time a minute short of the margin", offer.GatedDelistAt(terms) - 60, false},
		{"the median time at the margin, the clock long before", offer.GatedDelistAt(terms), true},
		{"after the expiry", int64(terms.GetExpiry()) + 6, true},
	} {
		book := &gatedBook{fakeBook: newFakeBook(CovEntry{Key: k, Offer: o, Gated: terms, Active: 500})}
		w := New(mtpChain{newFakeChain(), c.mtp}, book, nil)
		if _, err := w.ReconcileOnce(); err != nil {
			t.Fatal(err)
		}
		if got := len(book.delisted) == 1; got != c.delist {
			t.Fatalf("%s: delisted %v, want %v", c.name, got, c.delist)
		}
	}
}
