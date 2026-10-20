package settler

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// terms builds CovenantTerms for a covenant that sells assetA wanting assetB.
func terms(assetA, assetB [32]byte, num, den, minlot uint64, prog, makerx []byte, txid string, vout uint32) *seqobv1.CovenantTerms {
	return &seqobv1.CovenantTerms{
		CovenantTxid: txid, CovenantVout: vout,
		AssetA: hex.EncodeToString(assetA[:]), AssetB: hex.EncodeToString(assetB[:]),
		RateNum: num, RateDen: den, MinLot: minlot,
		MakerProg: prog, MakerProgVer: 1,
		ExpiryLocktime: 400, MakerX: makerx, InternalKey: covenant.NUMS[:],
	}
}

func TestPlanFromBothCovenantMatch(t *testing.T) {
	var x, y [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
	}
	prog0 := bytes.Repeat([]byte{0x11}, 32)
	prog1 := bytes.Repeat([]byte{0x44}, 32)
	mx0 := bytes.Repeat([]byte{0x22}, 32)
	mx1 := bytes.Repeat([]byte{0x33}, 32)

	m := matcher.Match{
		RestingCovenant:  terms(x, y, 3, 1, 5, prog0, mx0, repeat("aa", 32), 0),
		IncomingCovenant: terms(y, x, 1, 3, 5, prog1, mx1, repeat("bb", 32), 1),
	}
	if !m.BothCovenant() {
		t.Fatal("expected both-covenant match")
	}
	s, err := Plan(m, 30, 30, 90, 90)
	if err != nil {
		t.Fatal(err)
	}
	if s.In0.Txid != repeat("aa", 32) || s.In1.Vout != 1 {
		t.Fatalf("outpoints not carried through: %+v %+v", s.In0, s.In1)
	}
	if s.Leg0.CreditValue != 90 || s.Leg1.CreditValue != 30 {
		t.Fatalf("credits %d/%d, want 90/30", s.Leg0.CreditValue, s.Leg1.CreditValue)
	}
	// maker0 (resting) receives Y; maker1 (incoming) receives X.
	if !bytes.Equal(s.Leg0.CreditAsset[:], y[:]) || !bytes.Equal(s.Leg1.CreditAsset[:], x[:]) {
		t.Fatal("credit assets wrong")
	}
}

func TestPlanRejectsSingleCovenant(t *testing.T) {
	var x, y [32]byte
	m := matcher.Match{RestingCovenant: terms(x, y, 1, 1, 1, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), repeat("aa", 32), 0)}
	if _, err := Plan(m, 1, 1, 1, 1); err == nil {
		t.Fatal("expected rejection of non-both-covenant match")
	}
}

func repeat(h string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += h
	}
	return out
}

// expiryView answers Tip and TipMedianTime; Inspect is never reached when a
// covenant is past its refund lock.
type expiryView struct{ height, mtp int64 }

func (v expiryView) Tip() (string, int64, error)   { return "tip", v.height, nil }
func (v expiryView) TipMedianTime() (int64, error) { return v.mtp, nil }
func (v expiryView) Inspect(string, uint32, string, string) (watcher.CovState, error) {
	return watcher.CovState{}, errInspected
}

var errInspected = errors.New("inspected")

// A covenant past its REFUND lock is not settled, whether the lock is a height
// or a median time (judged by the tip's median time).
func TestResolveSkipsACovenantPastItsRefundLock(t *testing.T) {
	var x, y [32]byte
	y[0] = 1
	mk := func(expiry uint32) matcher.Match {
		a := terms(x, y, 1, 1, 1, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), repeat("aa", 32), 0)
		b := terms(y, x, 1, 1, 1, bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32), repeat("bb", 32), 0)
		a.ExpiryLocktime = expiry
		b.ExpiryLocktime = 499_999_999 // a height far ahead: only a's lock decides
		return matcher.Match{RestingCovenant: a, IncomingCovenant: b}
	}
	const T = 1792611631
	for _, c := range []struct {
		name   string
		expiry uint32
		view   expiryView
		past   bool
	}{
		{"height lock reached", 400, expiryView{height: 400}, true},
		{"height lock ahead", 400, expiryView{height: 399}, false},
		{"time lock, median time equal", T, expiryView{height: 9_000_000, mtp: T}, false},
		{"time lock, median time past", T, expiryView{height: 1, mtp: T + 1}, true},
	} {
		_, _, err := Resolve(mk(c.expiry), c.view)
		if c.past != (err != nil && strings.Contains(err.Error(), "past its refund expiry")) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.past && !errors.Is(err, errInspected) {
			t.Fatalf("%s: went no further than the lock: %v", c.name, err)
		}
	}
}
