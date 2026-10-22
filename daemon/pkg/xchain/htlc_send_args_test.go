package xchain

import (
	"reflect"
	"testing"
)

// The Sequentia leg's funding names its fee asset. Without it a node with the open
// fee market refused the maker's sendtoaddress after the taker had locked bitcoin
// (rpc error -8, "Nothing in this transaction determines the fee asset"), and the
// taker could only wait for its refund.
func TestHTLCSendArgsNameTheFeeAsset(t *testing.T) {
	eurx := "e39685e718516156679088d9400d11a1eb82bf7cc27c5b9f5a614b8c91246d13"
	got := htlcSendArgs("XP2SH", "14.46694409", eurx)
	want := map[string]interface{}{"address": "XP2SH", "amount": "14.46694409", "assetlabel": eurx, "fee_asset_label": eurx}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("an asset leg: got %v, want %v", got, want)
	}
	got = htlcSendArgs("XP2SH", "0.5", "")
	want = map[string]interface{}{"address": "XP2SH", "amount": "0.5", "fee_asset_label": "bitcoin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the pegged asset: got %v, want %v", got, want)
	}
}
