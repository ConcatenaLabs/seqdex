package watcher

import (
	"encoding/hex"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// exp is a fixed expected covenant program used across the classifier cases.
var exp = Expect{
	OrderSPKHex:   "5120" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00112233445566778899aabbccddeeff0011", // 34-byte OP_1 <32>
	AssetADisplay: "deadbeef" + "00112233445566778899aabbccddeeff00112233445566778899aabbccdd",               // 32-byte display id
	MinLot:        5_000_000,
}

func TestClassify_Live(t *testing.T) {
	st := CovState{
		Unspent:       true,
		Value:         42_000_000,
		AssetDisplay:  exp.AssetADisplay,
		SPKHex:        exp.OrderSPKHex,
		ConfirmedUTXO: true,
	}
	v := Classify(st, exp)
	if v.State != StateLive {
		t.Fatalf("want LIVE, got %s (%s)", v.State, v.Reason)
	}
	if v.LiveSize != 42_000_000 {
		t.Fatalf("live size: want 42000000, got %d", v.LiveSize)
	}
}

func TestClassify_Live_ReconcilesToChainValue(t *testing.T) {
	// A fill that never confirmed leaves the covenant unspent at its FULL value;
	// LiveSize must reflect the chain, so the book re-opens the order.
	st := CovState{Unspent: true, Value: 90_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: exp.OrderSPKHex}
	v := Classify(st, exp)
	if v.State != StateLive || v.LiveSize != 90_000_000 {
		t.Fatalf("want LIVE 90000000, got %s %d", v.State, v.LiveSize)
	}
}

func TestClassify_Live_SPKMismatchIsGhost(t *testing.T) {
	// An unspent outpoint whose spk is not the covenant program is not actually
	// this covenant: unsafe to keep resting.
	st := CovState{Unspent: true, Value: 42_000_000, AssetDisplay: exp.AssetADisplay, SPKHex: "5120" + "00"}
	v := Classify(st, exp)
	if v.State != StateGhost {
		t.Fatalf("want GHOST on spk mismatch, got %s", v.State)
	}
}

func TestClassify_Live_AssetMismatchIsGhost(t *testing.T) {
	st := CovState{Unspent: true, Value: 42_000_000, AssetDisplay: "cafe", SPKHex: exp.OrderSPKHex}
	v := Classify(st, exp)
	if v.State != StateGhost {
		t.Fatalf("want GHOST on asset mismatch, got %s", v.State)
	}
}

func TestClassify_FullFill(t *testing.T) {
	st := CovState{
		Unspent:          false,
		SpenderTxid:      "ff00ff00",
		SpenderConfirmed: true,
		Remainder:        RemainderOut{Present: false},
	}
	v := Classify(st, exp)
	if v.State != StateFilled {
		t.Fatalf("want FILLED, got %s", v.State)
	}
	if v.SpenderTxid != "ff00ff00" {
		t.Fatalf("spender txid: got %q", v.SpenderTxid)
	}
}

func TestClassify_PartialFill(t *testing.T) {
	st := CovState{
		Unspent:          false,
		SpenderTxid:      "abcabc",
		SpenderConfirmed: true,
		Remainder:        RemainderOut{Present: true, Vout: 1, Value: 60_000_000},
	}
	v := Classify(st, exp)
	if v.State != StatePartialFill {
		t.Fatalf("want PARTIAL_FILL, got %s", v.State)
	}
	if v.RemainderTxid != "abcabc" || v.RemainderVout != 1 || v.RemainderSize != 60_000_000 {
		t.Fatalf("remainder: got %s:%d size %d", v.RemainderTxid, v.RemainderVout, v.RemainderSize)
	}
}

func TestClassify_UnconfirmedSpend(t *testing.T) {
	// Spent only in the mempool: hold pending, never declare filled — a dropped
	// mempool spend must be reversible on a later pass.
	st := CovState{
		Unspent:          false,
		SpenderTxid:      "mempoolspend",
		SpenderConfirmed: false,
		Remainder:        RemainderOut{Present: true, Vout: 1, Value: 60_000_000}, // even with a remainder in mempool
	}
	v := Classify(st, exp)
	if v.State != StateUnconfirmedSpend {
		t.Fatalf("want UNCONFIRMED_SPEND, got %s", v.State)
	}
	if v.SpenderTxid != "mempoolspend" {
		t.Fatalf("spender: got %q", v.SpenderTxid)
	}
}

func TestClassify_Ghost_NoSpender(t *testing.T) {
	// Funding gone at the tip (Bitcoin-driven reorg / never confirmed): not a
	// UTXO and no spender anywhere.
	st := CovState{Unspent: false, SpenderTxid: ""}
	v := Classify(st, exp)
	if v.State != StateGhost {
		t.Fatalf("want GHOST, got %s", v.State)
	}
	if v.Reason == "" {
		t.Fatalf("ghost verdict should carry a reason")
	}
}

// --- loop routing: ReconcileOnce classifies each covenant and drives the book.

func TestReconcileOnce_RoutesAllStates(t *testing.T) {
	// Four covenant orders, one per outpoint, exercising each verdict.
	book := newFakeBook(
		mkEntry("live", "aa", 0, 90_000_000),
		mkEntry("part", "bb", 0, 90_000_000),
		mkEntry("full", "cc", 0, 90_000_000),
		mkEntry("ghost", "dd", 0, 90_000_000),
	)
	liveExp, err := expectFromTerms(sampleTerms("aa", 0))
	if err != nil {
		t.Fatalf("expect: %v", err)
	}
	chain := newFakeChain()
	chain.states["aa:0"] = CovState{Unspent: true, Value: 90_000_000, AssetDisplay: liveExp.AssetADisplay, SPKHex: liveExp.OrderSPKHex}
	chain.spend(t, []spent{{"bb", 0, 90_000_000, 30_000_000}}, "sp_bb", 50, 100)
	chain.spend(t, []spent{{"cc", 0, 90_000_000, 90_000_000}}, "sp_cc", 50, 100)
	chain.states["dd:0"] = CovState{SpenderTxid: ""}

	w := New(chain, book, nil)
	stats, err := w.ReconcileOnce()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Live != 1 || stats.PartialFill != 1 || stats.Filled != 1 || stats.Ghost != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	got := map[string]call{}
	for _, c := range book.calls {
		if c.op != "anchorconfs" && c.op != "record" {
			got[c.key.OfferID] = c
		}
	}
	if got["live"].op != "live" || got["live"].size != 90_000_000 {
		t.Fatalf("live routing: %+v", got["live"])
	}
	if got["part"].op != "repoint" || got["part"].txid != "sp_bb" || got["part"].vout != 1 || got["part"].size != 60_000_000 {
		t.Fatalf("partial routing: %+v", got["part"])
	}
	// A full fill confirmed but not final is held, not removed.
	if got["full"].op != "hold" || got["full"].txid != "sp_cc" {
		t.Fatalf("filled routing: %+v", got["full"])
	}
	if got["ghost"].op != "ghost" {
		t.Fatalf("ghost routing: %+v", got["ghost"])
	}
	if len(book.fills) != 2 {
		t.Fatalf("records: %+v", book.fills)
	}
}

// F3: a Ghost of a RE-RESTED covenant (its remainder outpoint vanished — most likely a reorg-undone
// fill) is HELD (reversible), not permanently invalidated, so it can re-open if the fill re-confirms.
// An ORIGINAL-funding ghost (never re-rested, so it never really settled) is still removed.
func TestReconcileOnce_ReRestedGhostIsHeld(t *testing.T) {
	re := mkEntry("rerested", "ee", 0, 60_000_000)
	re.ReRested = true
	book := newFakeBook(re, mkEntry("original", "ff", 0, 90_000_000))
	chain := newFakeChain()
	chain.states["ee:0"] = CovState{SpenderTxid: ""} // remainder gone, no spender -> Ghost
	chain.states["ff:0"] = CovState{SpenderTxid: ""} // original gone, no spender -> Ghost
	if _, err := New(chain, book, nil).ReconcileOnce(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := map[string]call{}
	for _, c := range book.calls {
		got[c.key.OfferID] = c
	}
	if got["rerested"].op != "holdghost" {
		t.Fatalf("re-rested ghost must be HELD (reversible), got %+v", got["rerested"])
	}
	if got["original"].op != "ghost" {
		t.Fatalf("original-funding ghost must be removed, got %+v", got["original"])
	}
}

func TestReconcileOnce_ReadsLiveTipAndDetectsReorg(t *testing.T) {
	chain := newFakeChain()
	chain.hash, chain.height = "tipA", 100
	w := New(chain, newFakeBook(), nil)
	s1, _ := w.ReconcileOnce()
	if s1.ReorgSeen {
		t.Fatalf("first pass should not flag a reorg")
	}
	// Tip hash changes at a LOWER height => rollback.
	chain.hash, chain.height = "tipB", 98
	s2, _ := w.ReconcileOnce()
	if !s2.ReorgSeen {
		t.Fatalf("rollback to a lower height must be flagged as a reorg")
	}
}

// sampleTerms builds valid CovenantTerms (NUMS internal key, 32-byte fields) so
// expectFromTerms/Derive succeed; asset ids are arbitrary-but-fixed 32-byte hex.
func sampleTerms(txid string, vout uint32) *seqobv1.CovenantTerms {
	b32 := func(fill byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = fill
		}
		return out
	}
	nums := make([]byte, 32)
	copy(nums, covenant.NUMS[:])
	return &seqobv1.CovenantTerms{
		CovenantTxid:   txid,
		CovenantVout:   vout,
		AssetA:         hex.EncodeToString(b32(0xa1)),
		AssetB:         hex.EncodeToString(b32(0xb2)),
		RateNum:        1,
		RateDen:        3,
		MakerProg:      b32(0xc3),
		MakerProgVer:   1,
		MinLot:         5_000_000,
		ExpiryLocktime: 500,
		MakerX:         b32(0xd4),
		InternalKey:    nums,
	}
}
