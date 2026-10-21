package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// A Simplicity order's terms carry no internal key: they derive the
// NUMS-keyed output, so a coin under the same tree with any other internal key
// (a key path) pays another script, and the probe refuses the offer.
func TestASimplicityOrderIsNUMSKeyedByConstruction(t *testing.T) {
	st := simTerms("s0", 0)
	o, err := offer.SimOrderOf(st)
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := o.Derive()
	// The same tree under a maker's key (the refund key, as a key path).
	keyed := keyPath(t, st.GetRefundKey(), tr.MerkleRoot)
	other := "5120" + hex.EncodeToString(keyed)
	if other == hex.EncodeToString(tr.ScriptPubKey) {
		t.Fatal("the key path made the same script")
	}
	exp, _ := CovEntry{Sim: st}.expect()
	o2 := &seqobv1.Offer{OfferAmount: 5_000_000, Settlement: &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: st}}
	chain := newFakeChain()
	chain.states["s0:0"] = CovState{Unspent: true, Value: 5_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: other}
	err = SubmitProbe{Chain: chain}.CheckOffer(context.Background(), o2)
	if err == nil || !strings.Contains(err.Error(), "does not pay the program derived from its terms") {
		t.Fatalf("a key-path coin: %v", err)
	}
	chain.states["s0:0"] = CovState{Unspent: true, Value: 5_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex}
	if err := (SubmitProbe{Chain: chain}).CheckOffer(context.Background(), o2); err != nil {
		t.Fatalf("the NUMS-keyed coin: %v", err)
	}
}

// keyPath is the x-only output key of root under the internal key x.
func keyPath(t *testing.T, x []byte, root [32]byte) []byte {
	t.Helper()
	p, err := schnorr.ParsePubKey(x)
	if err != nil {
		t.Fatal(err)
	}
	tag := sha256.Sum256([]byte("TapTweak/elements"))
	h := sha256.New()
	h.Write(tag[:])
	h.Write(tag[:])
	h.Write(x)
	h.Write(root[:])
	var tw [32]byte
	copy(tw[:], h.Sum(nil))
	var s btcec.ModNScalar
	s.SetBytes(&tw)
	var tG, pJ, q btcec.JacobianPoint
	btcec.ScalarBaseMultNonConst(&s, &tG)
	p.AsJacobian(&pJ)
	btcec.AddNonConst(&tG, &pJ, &q)
	q.ToAffine()
	return q.X.Bytes()[:]
}
