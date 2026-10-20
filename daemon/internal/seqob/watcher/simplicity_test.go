package watcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

var simMaker = append([]byte{0x00, 0x14}, bytes.Repeat([]byte{0xc3}, 20)...)

func simTerms(txid string, vout uint32) *seqobv1.SimplicityCovenantTerms {
	k := sha256.Sum256([]byte("refund"))
	_, pub := btcec.PrivKeyFromBytes(k[:])
	return &seqobv1.SimplicityCovenantTerms{
		CovenantTxid: txid, CovenantVout: vout,
		AssetA: hex.EncodeToString(bytes.Repeat([]byte{0xa1}, 32)), AssetB: hex.EncodeToString(bytes.Repeat([]byte{0xb2}, 32)),
		RateNum: 9_000_000_000_007, RateDen: 10_000_000_000_000, MinLot: 1_000_000, MakerScript: simMaker,
		ExpiryLocktime: 1_792_611_631, RefundKey: schnorr.SerializePubKey(pub), TemplateHash: covenant.SimOrderTemplateHash,
	}
}

func mkSimEntry(id, txid string, vout uint32, active uint64) CovEntry {
	st := simTerms(txid, vout)
	return CovEntry{
		Key: offerstore.Key{MakerPubkey: "mk", OfferID: id},
		Offer: &seqobv1.Offer{OfferId: id, MakerPubkey: "mk", BaseAmount: active,
			Pair: &seqobv1.AssetPair{BaseAsset: "a1", QuoteAsset: "b2"}, Settlement: &seqobv1.Offer_SimplicityCovenant{SimplicityCovenant: st}},
		Sim:    st,
		Active: active,
	}
}

// simSpend makes txid a confirmed spend of the Simplicity order at txid0:vout0
// (held locked) at input 0: a fill of filled (partial or whole, by its
// witness), or with refund set, the refund key's spend.
func simSpend(t *testing.T, f *fakeChain, txid0 string, vout0 uint32, txid string, locked, filled uint64, refund bool, h int64) {
	t.Helper()
	o, err := offer.SimOrderOf(simTerms(txid0, 0))
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := o.Derive()
	partial := filled < locked
	var w [][]byte
	var outs []covenant.Output
	if refund {
		w, _ = o.RefundWitness([64]byte{1})
		outs = []covenant.Output{covenant.Explicit(o.AssetA, locked, []byte{0x51})}
	} else {
		w, _ = o.FillWitness(partial)
		outs = append(outs, covenant.Explicit(o.AssetB, covenant.CeilPrice(filled, o.RateNum, o.RateDen), simMaker))
		if partial {
			outs = append(outs, covenant.Explicit(o.AssetA, locked-filled, tr.ScriptPubKey))
		} else {
			outs = append(outs, covenant.Explicit(o.AssetA, filled, []byte{0x51})) // the taker's own receipt at 2k+1
		}
	}
	st := CovState{SpenderTxid: txid, SpenderConfirmed: true, Spender: &SpenderTx{
		Txid: txid, Input: 0, Witness: w, Outputs: outs, PrevValue: locked,
	}}
	if partial && !refund {
		st.Remainder = RemainderOut{Present: true, Vout: 1, Value: locked - filled}
	}
	f.states[fmt.Sprintf("%s:%d", txid0, vout0)] = st
	f.txs[txid] = TxStatus{InChain: true, Block: "blk_" + txid, Height: h, Confirmations: 1, BlockAnchor: 100, CertPresent: true, Certified: true}
}

// A Simplicity order is reconciled as a tapscript one is: a partial fill read
// from its witness and outputs re-points it at (fill txid, 1); a whole fill is
// held until final, then removed; a refund records no fill and no trade. The
// order's fill product is far past the tapscript leaf's 2^63 bound.
func TestASimplicityOrdersFillsAreRecorded(t *testing.T) {
	const locked = 20_000_000_000_000
	chain := newFakeChain()
	book := newFakeBook(mkSimEntry("S", "s0", 0, locked), mkSimEntry("R", "r0", 0, 5_000_000))
	simSpend(t, chain, "s0", 0, "fill1", locked, 6_000_000_000_001, false, 99)
	simSpend(t, chain, "r0", 0, "refund1", 5_000_000, 0, true, 99)
	w := New(chain, book, nil)
	pass(t, w)
	fs := book.fillsOf("S")
	if len(fs) != 1 || fs[0].Filled != 6_000_000_000_001 || fs[0].Rest == nil || fs[0].Rest.Vout != 1 || fs[0].Credit.Vout != 0 {
		t.Fatalf("the partial fill's record: %+v", fs)
	}
	if fs[0].Taker.Via != offerstore.TakerChain {
		t.Fatalf("taker %+v", fs[0].Taker)
	}
	e, _ := book.entry("S")
	if txid, vout := e.outpoint(); txid != "fill1" || vout != 1 || e.Active != locked-6_000_000_000_001 || !e.ReRested {
		t.Fatalf("order S rests at %s:%d with %d", txid, vout, e.Active)
	}
	if e.Sim == nil || e.Offer.GetSimplicityCovenant() == nil {
		t.Fatal("the re-pointed order lost its terms")
	}
	if len(book.fillsOf("R")) != 0 {
		t.Fatalf("a refund recorded a fill: %+v", book.fillsOf("R"))
	}
	// The rest, filled whole: held until final, then removed.
	simSpend(t, chain, "fill1", 1, "fill2", locked-6_000_000_000_001, locked-6_000_000_000_001, false, 100)
	pass(t, w)
	if fs := book.fillsOf("S"); len(fs) != 2 || fs[1].Rest != nil || fs[1].Gap == nil || fs[1].Gap.Vout != 1 {
		t.Fatalf("the whole fill's record: %+v", fs)
	}
	chain.height, chain.hash = 103, "tip2"
	chain.anchor.Height = 103
	pass(t, w)
	if _, ok := book.entry("S"); ok {
		t.Fatal("order S was not removed once its whole fill was final")
	}
	if _, ok := book.entry("R"); ok {
		t.Fatal("order R was not removed once its refund was final")
	}
}

// ReadSimFill names the program's refusal when the chain took a spend the
// reading would refuse, and reads a spend that is not a fill as no fill.
func TestReadSimFill(t *testing.T) {
	o, err := offer.SimOrderOf(simTerms("x", 0))
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := o.Derive()
	w, _ := o.FillWitness(true)
	outs := []covenant.Output{
		covenant.Explicit(o.AssetB, covenant.CeilPrice(4_000_000, o.RateNum, o.RateDen)-1, simMaker),
		covenant.Explicit(o.AssetA, 6_000_000, tr.ScriptPubKey),
	}
	r, err := ReadSimFill(o, &SpenderTx{Txid: "t", Witness: w, Outputs: outs}, 10_000_000)
	if err != nil || !r.Partial || r.Filled != 4_000_000 || r.Disagrees == "" {
		t.Fatalf("%v %+v", err, r)
	}
	// A tapscript FILL witness on this coin is no fill of this order.
	r, _ = ReadSimFill(o, &SpenderTx{Txid: "t", Witness: [][]byte{{1}, {2}}, Outputs: outs}, 10_000_000)
	if !r.Refund {
		t.Fatalf("%+v", r)
	}
}
