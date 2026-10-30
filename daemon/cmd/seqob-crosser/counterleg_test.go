package main

import "testing"

// A pure-LN take's counter leg settles real BTC on the bitcoin node and an asset quote on
// the asset node, never an asset on the bitcoin node.
func TestCounterLegSocket(t *testing.T) {
	c := &Caps{AssetLNSocket: "/asset/lightning-rpc", BtcLNSocket: "/btc/lightning-rpc"}
	if s := counterLegSocket(c, ""); s != c.BtcLNSocket {
		t.Fatalf("a BTC quote on %s", s)
	}
	if s := counterLegSocket(c, "BTC"); s != c.BtcLNSocket {
		t.Fatalf("the BTC sentinel on %s", s)
	}
	gold := "4dfe69c334a9c8d5f05c8c1f0dd8f1e4c9a8a4e6d0c3b1a2f9e8d7c6b5a49382"
	if s := counterLegSocket(c, gold); s != c.AssetLNSocket {
		t.Fatalf("an asset quote on %s, not the asset node", s)
	}
}

// A partly filled offer is priced from its signed whole, as the maker prices it, not from what is
// left of it.
func TestPlnWholeIsTheSignedWhole(t *testing.T) {
	n := &NormOrder{BaseSize: 400, BaseDen: 1000, QuoteNum: 333}
	if a, b := plnWhole(n); a != 1000 || b != 333 {
		t.Fatalf("priced from %d/%d, not the signed 1000/333", a, b)
	}
}
