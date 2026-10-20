package watcher

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// --- a stateful fake book ------------------------------------------------------

type call struct {
	op   string
	key  offerstore.Key
	txid string
	vout uint32
	size uint64
	re   bool
}

type fakeBook struct {
	covs  []CovEntry
	calls []call
	fills []offerstore.CovenantFill // every record, as RecordFill/UpdateFill leave it
}

func newFakeBook(es ...CovEntry) *fakeBook { return &fakeBook{covs: es} }

func (b *fakeBook) find(k offerstore.Key) int {
	for i := range b.covs {
		if b.covs[i].Key == k {
			return i
		}
	}
	return -1
}

func (b *fakeBook) SnapshotCovenants() []CovEntry { return append([]CovEntry(nil), b.covs...) }
func (b *fakeBook) ReconcileLive(k offerstore.Key, active uint64) error {
	b.calls = append(b.calls, call{op: "live", key: k, size: active})
	if i := b.find(k); i >= 0 {
		b.covs[i].Active = active
	}
	return nil
}
func (b *fakeBook) Repoint(k offerstore.Key, txid string, vout uint32, size uint64, reRested bool) error {
	b.calls = append(b.calls, call{op: "repoint", key: k, txid: txid, vout: vout, size: size, re: reRested})
	if i := b.find(k); i >= 0 {
		t := proto.Clone(b.covs[i].Terms).(*seqobv1.CovenantTerms)
		t.CovenantTxid, t.CovenantVout = txid, vout
		b.covs[i].Terms, b.covs[i].Active, b.covs[i].ReRested = t, size, reRested
		o := proto.Clone(b.covs[i].Offer).(*seqobv1.Offer)
		o.Settlement = &seqobv1.Offer_Covenant{Covenant: t}
		b.covs[i].Offer = o
	}
	return nil
}
func (b *fakeBook) remove(k offerstore.Key) {
	if i := b.find(k); i >= 0 {
		b.covs = append(b.covs[:i], b.covs[i+1:]...)
	}
}
func (b *fakeBook) RemoveFilled(k offerstore.Key, spender string) error {
	b.calls = append(b.calls, call{op: "filled", key: k, txid: spender})
	b.remove(k)
	return nil
}
func (b *fakeBook) RemoveRefunded(k offerstore.Key, spender string) error {
	b.calls = append(b.calls, call{op: "refunded", key: k, txid: spender})
	b.remove(k)
	return nil
}
func (b *fakeBook) HoldForSpend(k offerstore.Key, spender string) error {
	b.calls = append(b.calls, call{op: "hold", key: k, txid: spender})
	if i := b.find(k); i >= 0 {
		b.covs[i].Active = 0
	}
	return nil
}
func (b *fakeBook) RemoveGhost(k offerstore.Key, reason string) error {
	b.calls = append(b.calls, call{op: "ghost", key: k})
	b.remove(k)
	return nil
}
func (b *fakeBook) HoldGhost(k offerstore.Key, reason string) error {
	b.calls = append(b.calls, call{op: "holdghost", key: k})
	return nil
}
func (b *fakeBook) Reopen(o *seqobv1.Offer, active uint64) error {
	k := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
	b.calls = append(b.calls, call{op: "reopen", key: k, txid: o.GetCovenant().GetCovenantTxid(), vout: o.GetCovenant().GetCovenantVout(), size: active})
	if b.find(k) < 0 {
		b.covs = append(b.covs, CovEntry{Key: k, Offer: o, Terms: o.GetCovenant(), Active: active})
	}
	return nil
}
func (b *fakeBook) RecordFill(f offerstore.CovenantFill, resting *seqobv1.Offer) {
	b.calls = append(b.calls, call{op: "record", key: f.Order, txid: f.Txid, vout: f.Input, size: f.Filled})
	b.fills = append(b.fills, f)
}
func (b *fakeBook) UpdateFill(f offerstore.CovenantFill) {
	b.calls = append(b.calls, call{op: "update:" + f.State, key: f.Order, txid: f.Txid, vout: f.Input})
	for i := range b.fills {
		if b.fills[i].Order == f.Order && b.fills[i].Txid == f.Txid && b.fills[i].Input == f.Input && b.fills[i].State != offerstore.FillUndone {
			b.fills[i] = f
			return
		}
	}
}
func (b *fakeBook) SetAnchorConfs(k offerstore.Key, depth uint32) {
	b.calls = append(b.calls, call{op: "anchorconfs", key: k, size: uint64(depth)})
}

func (b *fakeBook) ops(k string) []string {
	var out []string
	for _, c := range b.calls {
		if c.key.OfferID == k && c.op != "anchorconfs" {
			out = append(out, c.op)
		}
	}
	return out
}

func (b *fakeBook) fillsOf(id string) []offerstore.CovenantFill {
	var out []offerstore.CovenantFill
	for _, f := range b.fills {
		if f.Order.OfferID == id {
			out = append(out, f)
		}
	}
	return out
}

func (b *fakeBook) entry(id string) (CovEntry, bool) {
	for _, c := range b.covs {
		if c.Key.OfferID == id {
			return c, true
		}
	}
	return CovEntry{}, false
}

// --- a fake chain ---------------------------------------------------------------

// fakeChain returns a canned CovState per outpoint and a canned status per
// transaction; Anchor answers anchor.
type fakeChain struct {
	hash   string
	height int64
	states map[string]CovState
	txs    map[string]TxStatus
	anchor AnchorTip
}

func newFakeChain() *fakeChain {
	return &fakeChain{hash: "tip1", height: 100, states: map[string]CovState{}, txs: map[string]TxStatus{},
		anchor: AnchorTip{Anchored: true, Height: 100, Status: "ok"}}
}

func (f *fakeChain) Tip() (string, int64, error) { return f.hash, f.height, nil }
func (f *fakeChain) Inspect(txid string, vout uint32, _, _ string) (CovState, error) {
	return f.states[fmt.Sprintf("%s:%d", txid, vout)], nil
}
func (f *fakeChain) TxStatus(txid string) (TxStatus, error) { return f.txs[txid], nil }
func (f *fakeChain) Anchor() (AnchorTip, error)             { return f.anchor, nil }

// spent is an order's outpoint a spender spends, what it held and what is taken.
type spent struct {
	txid   string
	vout   uint32
	locked uint64
	filled uint64
}

// spend makes txid a confirmed fill of each order in ins, order k at input k,
// in a certified block at height h whose anchor is blockAnchor: the order's
// credit at 2k, its rest (or an asset-B gap) at 2k+1, a receipt after. Every
// order has sampleTerms' program. It returns the spender's outputs.
func (f *fakeChain) spend(t *testing.T, ins []spent, txid string, h int64, blockAnchor int64) []covenant.Output {
	t.Helper()
	o, err := orderFromTerms(sampleTerms("x", 0))
	if err != nil {
		t.Fatal(err)
	}
	tap, err := o.Derive()
	if err != nil {
		t.Fatal(err)
	}
	maker := append([]byte{0x51, 0x20}, o.MakerProg...)
	var outs []covenant.Output
	for _, in := range ins {
		outs = append(outs, covenant.Explicit(o.AssetB, covenant.CeilPrice(in.filled, o.RateNum, o.RateDen), maker))
		if in.filled < in.locked {
			outs = append(outs, covenant.Explicit(o.AssetA, in.locked-in.filled, tap.ScriptPubKey))
		} else {
			outs = append(outs, covenant.Explicit(o.AssetB, 1000, []byte{0x00, 0x14, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}))
		}
	}
	outs = append(outs, covenant.Explicit(o.AssetA, 1, []byte{0x51}))
	for k, in := range ins {
		st := CovState{SpenderTxid: txid, SpenderConfirmed: true, Spender: &SpenderTx{
			Txid: txid, Input: uint32(k), Witness: tap.FillWitness(o.InternalKey), Outputs: outs, PrevValue: in.locked,
		}}
		if in.filled < in.locked {
			st.Remainder = RemainderOut{Present: true, Vout: uint32(2*k + 1), Value: in.locked - in.filled}
		}
		f.states[fmt.Sprintf("%s:%d", in.txid, in.vout)] = st
	}
	f.txs[txid] = TxStatus{InChain: true, Block: "blk_" + txid, Height: h, Confirmations: 1, BlockAnchor: blockAnchor, CertPresent: true, Certified: true}
	return outs
}

// mempool puts the spends of txid back in the mempool: its block left the chain.
func (f *fakeChain) mempool(txid string) {
	f.txs[txid] = TxStatus{InMempool: true}
	for k, st := range f.states {
		if st.SpenderTxid == txid {
			st.SpenderConfirmed = false
			f.states[k] = st
		}
	}
}

// unspent makes the outpoint unspent again with value atoms of the order.
func (f *fakeChain) unspent(t *testing.T, txid string, vout uint32, value uint64) {
	exp, err := expectFromTerms(sampleTerms(txid, vout))
	if err != nil {
		t.Fatal(err)
	}
	f.states[fmt.Sprintf("%s:%d", txid, vout)] = CovState{Unspent: true, Value: value, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex, ConfirmedUTXO: true}
}

func mkEntry(id, txid string, vout uint32, active uint64) CovEntry {
	t := sampleTerms(txid, vout)
	return CovEntry{
		Key: offerstore.Key{MakerPubkey: "mk", OfferID: id},
		Offer: &seqobv1.Offer{OfferId: id, MakerPubkey: "mk", BaseAmount: active,
			Pair: &seqobv1.AssetPair{BaseAsset: "a1", QuoteAsset: "b2"}, Settlement: &seqobv1.Offer_Covenant{Covenant: t}},
		Terms:  t,
		Active: active,
	}
}
