package main

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// After a partial fill the maker re-rests the remainder, its amounts rounded, and a taker prices
// the next slice off that signed remainder. The maker must price it off the same whole: priced
// off the size it was started with, the two differed by a few msat and every such take failed
// ("maker BTC amount N != the N the offer's ratio gives").
func TestPureLNSlicePricedOffTheSignedRemainder(t *testing.T) {
	for _, dir := range []uint32{offer.LnBTCLNForAssetLN, offer.LnAssetLNForBTCLN} {
		const assetAmt, btcAmt = uint64(309895875), uint64(731171029) // started with
		o := &seqobv1.Offer{BaseAmount: assetAmt, Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{LnDirection: dir}}}
		if dir == offer.LnBTCLNForAssetLN {
			o.OfferAmount, o.WantAmount = assetAmt, btcAmt
		} else {
			o.OfferAmount, o.WantAmount = btcAmt, assetAmt
		}
		if r := plnRestRemainder(o, 123456789); r != assetAmt-123456789 || o.GetBaseAmount() != r {
			t.Fatalf("dir %d: remainder %d, base %d", dir, r, o.GetBaseAmount())
		}
		// The taker's price of a slice: the signed whole of the offer it reads (crosser plnWhole).
		var signedAsset, signedBtc uint64
		if dir == offer.LnBTCLNForAssetLN {
			signedAsset, signedBtc = o.GetOfferAmount(), o.GetWantAmount()
		} else {
			signedAsset, signedBtc = o.GetWantAmount(), o.GetOfferAmount()
		}
		take := uint64(77777777) * 1000
		taker := client.ProportionalBtc(signedBtc*1000, take, signedAsset*1000)
		btcMsat, seqMsat := plnWholeMsat(o)
		maker := client.ProportionalBtc(btcMsat, take, seqMsat)
		if maker != taker {
			t.Fatalf("dir %d: the maker prices the slice at %d msat, the taker at %d", dir, maker, taker)
		}
		if old := client.ProportionalBtc(btcAmt*1000, take, assetAmt*1000); old == taker {
			t.Logf("dir %d: the started-with size happens to give the same price here", dir)
		} else {
			t.Logf("dir %d: priced off the started-with size it was %d msat, %d apart", dir, old, int64(old)-int64(taker))
		}
	}
}
