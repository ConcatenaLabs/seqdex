package restricted

import (
	"errors"
	"testing"
	"time"
)

func TestPickTSeqKeepsADayBeforeTBtc(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tBtc := uint32(now.Add(TBtcAhead).Unix())
	ts, err := PickTSeq(now, tBtc)
	if err != nil || int64(ts) != now.Add(TSeqAhead).Unix() {
		t.Fatalf("a lock at once: T_seq %d, %v", ts, err)
	}
	// Two and a half hours later T_seq is pulled in to T_btc - 24 h.
	later := now.Add(150 * time.Minute)
	if ts, err = PickTSeq(later, tBtc); err != nil || int64(tBtc)-int64(ts) != int64(BtcAfterSeq/time.Second) {
		t.Fatalf("a late lock: T_seq %d, %v", ts, err)
	}
	// Past T_btc - 24 h - MinTSeqAhead nothing can be locked.
	if _, err = PickTSeq(now.Add(4*time.Hour), tBtc); err == nil {
		t.Fatal("a lock with no room for T_seq was allowed")
	}
}

func TestCheckLocks(t *testing.T) {
	if err := CheckLocks(1_800_000_000, 1_800_000_000+86_400); err != nil {
		t.Fatal(err)
	}
	if CheckLocks(1_800_000_000, 1_800_000_000+86_399) == nil {
		t.Fatal("T_btc less than a day after T_seq was accepted")
	}
	if CheckLocks(400_000, 1_800_000_000) == nil {
		t.Fatal("a height T_seq was accepted")
	}
}

func TestNoClaimAfterTheCutoffOrNearTBtc(t *testing.T) {
	tSeq := uint32(1_800_000_000)
	tBtc := tSeq + 86_400
	btcNow := int64(tSeq) - 3600
	if err := ClaimAllowed(time.Unix(int64(tSeq)-3601, 0), btcNow, tSeq, tBtc); err != nil {
		t.Fatalf("a claim a second before the cut-off: %v", err)
	}
	if err := ClaimAllowed(time.Unix(int64(tSeq)-3600, 0), btcNow, tSeq, tBtc); !errors.Is(err, ErrNoClaim) {
		t.Fatalf("a claim at the cut-off: %v", err)
	}
	// Bitcoin's clock running ahead: within 12 h of T_btc, no claim.
	if err := ClaimAllowed(time.Unix(int64(tSeq)-7200, 0), int64(tBtc)-12*3600, tSeq, tBtc); !errors.Is(err, ErrNoClaim) {
		t.Fatalf("a claim 12 h before T_btc by Bitcoin's clock: %v", err)
	}
}
