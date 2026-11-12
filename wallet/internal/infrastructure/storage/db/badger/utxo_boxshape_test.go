package dbbadger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

// The testnet's wallet store, in shape: 81 accounts and millions of coins, most of them spent.
// These run only when OCEAN_BOXSHAPE_DIR names a directory for the store; it is written once
// (by whichever build runs first) and read by later runs, so a store written by one build can
// be opened by another. OCEAN_BOXSHAPE_UTXOS sets the number of coins (default 3,000,000) and
// OCEAN_BOXSHAPE_SECONDS how long the readers run (default 120).

const boxAccounts = 81

type boxBalance struct {
	Unspent   int               `json:"unspent"`
	Locked    int               `json:"locked"`
	Confirmed map[string]uint64 `json:"confirmed"`
	LockedValue   map[string]uint64 `json:"locked_value"`
}

func boxEnv(t *testing.T) (string, int) {
	dir := os.Getenv("OCEAN_BOXSHAPE_DIR")
	if dir == "" {
		t.Skip("OCEAN_BOXSHAPE_DIR not set")
	}
	n := 3000000
	if v := os.Getenv("OCEAN_BOXSHAPE_UTXOS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		require.NoError(t, err)
	}
	return dir, n
}

// boxCoin is coin i of the store: account i%81; one in twenty unspent, one in two hundred of
// those locked.
func boxCoin(i int) *domain.Utxo {
	account := fmt.Sprintf("bip84-account%d", 1+i%boxAccounts)
	u := coinOf(account, i, i%20 != 0)
	if i%20 == 0 && i%4000 == 0 {
		u.LockTimestamp = 1700000000
		u.LockExpiryTimestamp = 4000000000
	}
	return u
}

func TestBoxShapeWriteStore(t *testing.T) {
	dir, n := boxEnv(t)
	if _, err := os.Stat(filepath.Join(dir, "expected.json")); err == nil {
		t.Skip("the store is already written")
	}
	rm, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	repo := rm.UtxoRepository()
	want := map[string]*boxBalance{}
	start := time.Now()
	batch := make([]*domain.Utxo, 0, 5000)
	for i := 0; i < n; i++ {
		u := boxCoin(i)
		b := want[u.AccountName]
		if b == nil {
			b = &boxBalance{Confirmed: map[string]uint64{}, LockedValue: map[string]uint64{}}
			want[u.AccountName] = b
		}
		if !u.IsSpent() {
			if u.IsLocked() {
				b.Locked++
				b.LockedValue[u.Asset] += u.Value
			} else {
				b.Unspent++
				b.Confirmed[u.Asset] += u.Value
			}
		}
		batch = append(batch, u)
		if len(batch) == cap(batch) || i == n-1 {
			addCoins(t, repo, batch)
			batch = batch[:0]
		}
		if (i+1)%500000 == 0 {
			t.Logf("%d coins written in %s", i+1, time.Since(start))
		}
	}
	rm.Close()
	f, err := json.Marshal(want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "expected.json"), f, 0o600))
	t.Logf("store of %d coins over %d accounts written in %s", n, boxAccounts, time.Since(start))
}

// The reads the makers make, at the store's shape: one of each alone, then 40 clients reading
// balances and spendable coins and giving up after 20 s, each relaunching at once, while the
// heap and the reads still running are sampled every second.
func TestBoxShapeMakersRead(t *testing.T) {
	dir, _ := boxEnv(t)
	raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	require.NoError(t, err, "write the store first")
	want := map[string]*boxBalance{}
	require.NoError(t, json.Unmarshal(raw, &want))
	seconds := 120
	if v := os.Getenv("OCEAN_BOXSHAPE_SECONDS"); v != "" {
		seconds, err = strconv.Atoi(v)
		require.NoError(t, err)
	}
	// The laptop is shared: stop the pile-up well before it takes the memory the others need.
	debug.SetMemoryLimit(3 << 30)
	const heapStop = 2500 << 20

	start := time.Now()
	rm, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	defer rm.Close()
	t.Logf("store opened in %s", time.Since(start))
	repo := rm.UtxoRepository()
	ctx := context.Background()

	for _, account := range []string{"bip84-account17"} {
		s := time.Now()
		b, err := repo.GetBalanceForAccount(ctx, account)
		require.NoError(t, err)
		tb := time.Since(s)
		s = time.Now()
		u, err := repo.GetSpendableUtxosForAccount(ctx, account, nil)
		require.NoError(t, err)
		tu := time.Since(s)
		s = time.Now()
		l, err := repo.GetLockedUtxosForAccount(ctx, account, nil)
		require.NoError(t, err)
		tl := time.Since(s)
		t.Logf("%s alone: balance %s, %d spendable coins %s, %d locked coins %s",
			account, tb, len(u), tu, len(l), tl)
		require.Len(t, u, want[account].Unspent)
		require.Len(t, l, want[account].Locked)
		for asset, v := range want[account].Confirmed {
			require.Equal(t, v, b[asset].Confirmed, "%s %s", account, asset)
		}
	}

	var running, started, finished, overran int64
	stop := make(chan struct{})
	var clients sync.WaitGroup
	for c := 0; c < 40; c++ {
		clients.Add(1)
		go func(c int) {
			defer clients.Done()
			account := fmt.Sprintf("bip84-account%d", 1+c%boxAccounts)
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				default:
				}
				rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				done := make(chan struct{})
				atomic.AddInt64(&running, 1)
				atomic.AddInt64(&started, 1)
				go func(k int) {
					defer close(done)
					defer atomic.AddInt64(&running, -1)
					if k%2 == 0 {
						_, _ = repo.GetBalanceForAccount(rctx, account)
					} else {
						_, _ = repo.GetSpendableUtxosForAccount(rctx, account, nil)
					}
					if rctx.Err() != nil {
						atomic.AddInt64(&overran, 1)
					} else {
						atomic.AddInt64(&finished, 1)
					}
				}(k)
				select {
				case <-done:
				case <-rctx.Done():
				case <-stop:
				}
				cancel()
			}
		}(c)
	}

	var ms runtime.MemStats
	var peak uint64
	tick := time.NewTicker(time.Second)
	readers := time.Now()
	for s := 1; s <= seconds; s++ {
		<-tick.C
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > peak {
			peak = ms.HeapInuse
		}
		if s%10 == 0 || ms.HeapInuse > heapStop {
			t.Logf("%3ds heap in use %5d MB; reads running %d, started %d, done in time %d, past their deadline %d",
				s, ms.HeapInuse>>20, atomic.LoadInt64(&running), atomic.LoadInt64(&started),
				atomic.LoadInt64(&finished), atomic.LoadInt64(&overran))
		}
		if ms.HeapInuse > heapStop {
			t.Logf("heap passed %d MB after %ds: stopping the readers", heapStop>>20, s)
			break
		}
	}
	tick.Stop()
	stopped := time.Now()
	close(stop)
	clients.Wait()
	t.Logf("readers ran %s; heap peak %d MB; reads started %d, done in time %d, past their deadline %d, still running %d",
		time.Since(readers).Round(time.Second), peak>>20, atomic.LoadInt64(&started),
		atomic.LoadInt64(&finished), atomic.LoadInt64(&overran), atomic.LoadInt64(&running))
	// Wait for the reads still running before the store closes, for two minutes at most.
	for atomic.LoadInt64(&running) > 0 && time.Since(stopped) < 2*time.Minute {
		time.Sleep(100 * time.Millisecond)
	}
	if n := atomic.LoadInt64(&running); n > 0 {
		t.Logf("%d reads still running two minutes after the readers stopped: ending here", n)
		os.Exit(1)
	}
	t.Logf("the last read ended %s after the readers stopped", time.Since(stopped).Round(100*time.Millisecond))
}
