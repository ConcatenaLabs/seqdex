package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
)

// anchorOps reports an anchor tip; nothing else is called before the lock.
type anchorOps struct {
	XcOps
	tip int64
}

func (a *anchorOps) SeqAnchorTip() (int64, string, error) { return a.tip, "ok", nil }

// F1: the asset is never locked against a BTC leg that no longer verifies
// after the anchor wait (replaced in the mempool, or removed by a reorg).
// The Party is empty: a lock attempt would fail on it for another reason,
// so the error must be the re-verification's.
func TestSellerLockReverifiesTheBTCLegAfterTheWait(t *testing.T) {
	r := &RestrictedSwap{Ops: &anchorOps{tip: 100}, Party: &restricted.Party{}, Ctx: context.Background(), Timing: XcTiming{Poll: time.Millisecond}}
	r.Timing.setDefaults()
	res := &RestrictedResult{TBtc: uint32(time.Now().Add(restricted.TBtcAhead).Unix())}
	calls := 0
	_, _, err := r.sellerLock(res, restricted.SwapTerms{}, 90, func() (int64, error) {
		calls++
		return 0, errors.New("btc leg not found in the best chain")
	})
	if err == nil || !strings.Contains(err.Error(), "no longer verifies") || calls != 1 {
		t.Fatalf("the lock went ahead against a leg that no longer verifies: %v (calls %d)", err, calls)
	}
}

// A leg re-mined at another height waits for the anchor to reach the new one
// before the lock.
func TestSellerLockWaitsAgainWhenTheLegMoved(t *testing.T) {
	ops := &anchorOps{tip: 100}
	r := &RestrictedSwap{Ops: ops, Party: &restricted.Party{}, Ctx: context.Background(), Timing: XcTiming{Poll: time.Millisecond}}
	r.Timing.setDefaults()
	res := &RestrictedResult{TBtc: uint32(time.Now().Add(restricted.TBtcAhead).Unix())}
	calls := 0
	_, _, err := r.sellerLock(res, restricted.SwapTerms{}, 90, func() (int64, error) {
		calls++
		if calls == 1 {
			return 95, nil // moved, still below the tip: wait again, then verify again
		}
		return 0, errors.New("stop")
	})
	if calls != 2 || res.BtcLegHeight != 95 || err == nil || !strings.Contains(err.Error(), "no longer verifies") {
		t.Fatalf("a moved leg was not re-waited and re-verified: calls %d height %d err %v", calls, res.BtcLegHeight, err)
	}
}

// The seller's BTC leg depth never falls below MinBTCLegDepth, whatever the
// maker's -min-btc-conf.
func TestRestrictedMinBTCConfFloor(t *testing.T) {
	r := &RestrictedSwap{Ops: &anchorOps{}, Party: &restricted.Party{}, Crypter: &Crypter{}, BtcMedianTime: func() (int64, error) { return 0, nil },
		Value: 1, BtcAmount: 1, PotAsset: "x", PotAmount: 1, RfaPerVb: 1}
	if err := r.defaults(); err != nil {
		t.Fatal(err)
	}
	if r.MinBTCConf != MinBTCLegDepth {
		t.Fatalf("MinBTCConf %d, want the floor %d", r.MinBTCConf, MinBTCLegDepth)
	}
}
