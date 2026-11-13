package smallestsubset_selector

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/stretchr/testify/require"
)

const assetA = "aa"
const assetB = "bb"

func coins(asset string, values ...uint64) []*domain.Utxo {
	out := make([]*domain.Utxo, 0, len(values))
	for i, v := range values {
		out = append(out, &domain.Utxo{
			UtxoKey: domain.UtxoKey{TxID: fmt.Sprintf("%s%04d", asset, i), VOut: uint32(i)},
			Value:   v, Asset: asset,
		})
	}
	return out
}

func repeat(v uint64, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// oldSelect is SelectUtxos as it was, over the old enumeration.
func oldSelect(utxos []*domain.Utxo, target uint64, asset string) ([]*domain.Utxo, uint64, error) {
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].Value < utxos[j].Value })
	targetUtxos := make([]*domain.Utxo, 0)
	balance := uint64(0)
	for _, u := range utxos {
		if u.Asset == asset {
			targetUtxos = append(targetUtxos, u)
			balance += u.Value
		}
	}
	if target == balance {
		return targetUtxos, 0, nil
	}
	values := []uint64{}
	for _, u := range targetUtxos {
		values = append(values, u.Value)
	}
	indexes := findIndexes(oldBestCombination(values, target), values)
	if len(indexes) <= 0 {
		return nil, 0, ErrTargetAmountNotReached
	}
	selected := make([]*domain.Utxo, 0)
	total := uint64(0)
	for _, i := range indexes {
		total += targetUtxos[i].Value
		selected = append(selected, targetUtxos[i])
	}
	return selected, total - target, nil
}

func keysOf(us []*domain.Utxo) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.TxID)
	}
	sort.Strings(out)
	return out
}

func sameAsOld(t *testing.T, utxos []*domain.Utxo, target uint64, asset string) ([]*domain.Utxo, uint64, error) {
	t.Helper()
	a := append([]*domain.Utxo(nil), utxos...)
	b := append([]*domain.Utxo(nil), utxos...)
	wantUs, wantChange, wantErr := oldSelect(a, target, asset)
	gotUs, gotChange, gotErr := NewSmallestSubsetCoinSelector().SelectUtxos(b, target, asset)
	require.Equal(t, wantErr, gotErr)
	require.Equal(t, keysOf(wantUs), keysOf(gotUs))
	require.Equal(t, wantChange, gotChange)
	if gotErr == nil {
		var total uint64
		for _, u := range gotUs {
			total += u.Value
		}
		require.Equal(t, target+gotChange, total, "the coins cover the target plus the change")
	}
	return gotUs, gotChange, gotErr
}

func TestSelectEdges(t *testing.T) {
	t.Run("all coins equal", func(t *testing.T) {
		for _, target := range []uint64{1, 7, 8, 21, 22, 70, 140} {
			us, _, err := sameAsOld(t, coins(assetA, repeat(7, 20)...), target, assetA)
			require.NoError(t, err)
			require.NotEmpty(t, us)
		}
	})
	t.Run("target exactly a sum of k coins, and one atom above", func(t *testing.T) {
		vals := []uint64{3, 5, 9, 11, 13, 20, 30}
		for _, target := range []uint64{31, 32, 34, 35, 61, 62} { // 31 = 11+20, 61 = 11+20+30 ...
			sameAsOld(t, coins(assetA, vals...), target, assetA)
		}
	})
	t.Run("target above the account's total", func(t *testing.T) {
		_, _, err := sameAsOld(t, coins(assetA, 1, 2, 3), 7, assetA)
		require.ErrorIs(t, err, ErrTargetAmountNotReached)
	})
	t.Run("target equal to the account's total", func(t *testing.T) {
		us, change, err := sameAsOld(t, coins(assetA, 1, 2, 3), 6, assetA)
		require.NoError(t, err)
		require.Len(t, us, 3)
		require.Zero(t, change)
	})
	t.Run("one coin above the target among many crumbs", func(t *testing.T) {
		us, _, err := sameAsOld(t, coins(assetA, append(repeat(1, 200), 500)...), 120, assetA)
		require.NoError(t, err)
		require.Len(t, us, 1)
	})
	t.Run("coins above ten times the target mixed in", func(t *testing.T) {
		sameAsOld(t, coins(assetA, 2, 3, 4, 1000, 5000), 8, assetA)  // small ones reach it
		sameAsOld(t, coins(assetA, 2, 3, 1000, 5000), 8, assetA)     // they do not: the smallest big one
		sameAsOld(t, coins(assetA, 2, 3, 80, 1000), 8, assetA)       // 80 is exactly ten times
		sameAsOld(t, coins(assetA, 2, 3, 81, 1000), 8, assetA)       // 81 is above
	})
	t.Run("zero-value and dust coins", func(t *testing.T) {
		sameAsOld(t, coins(assetA, 0, 0, 1, 1, 2, 546, 600), 0, assetA)
		sameAsOld(t, coins(assetA, 0, 0, 1, 1, 2, 546, 600), 1, assetA)
		sameAsOld(t, coins(assetA, 0, 0, 1, 1, 2, 546, 600), 1000, assetA)
		sameAsOld(t, coins(assetA, 0, 0, 0), 1, assetA)
	})
	t.Run("several assets in one account", func(t *testing.T) {
		mixed := append(coins(assetA, 5, 6, 7, 100), coins(assetB, 1, 2, 3, 50)...)
		us, _, err := sameAsOld(t, mixed, 12, assetA)
		require.NoError(t, err)
		for _, u := range us {
			require.Equal(t, assetA, u.Asset)
		}
		sameAsOld(t, mixed, 6, assetB)
		_, _, err = sameAsOld(t, mixed, 1, "cc")
		require.ErrorIs(t, err, ErrTargetAmountNotReached)
	})
	t.Run("no coins", func(t *testing.T) {
		sameAsOld(t, nil, 5, assetA)
	})
}

// Selections at once share nothing (the old enumeration shared one slice between calls: a
// data race). Run with -race.
func TestSelectConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 200; r++ {
				vals := make([]uint64, 40)
				for i := range vals {
					vals[i] = uint64(1 + (i*7+g+r)%23)
				}
				target := uint64(30 + (g*r)%40)
				us, change, err := NewSmallestSubsetCoinSelector().SelectUtxos(coins(assetA, vals...), target, assetA)
				if err != nil {
					continue
				}
				var total uint64
				for _, u := range us {
					total += u.Value
				}
				if total != target+change || total < target {
					t.Errorf("selected %d for target %d, change %d", total, target, change)
				}
			}
		}()
	}
	wg.Wait()
}

// The cost of one selection: OCEAN_SELECTOR_COST=1 prints time and allocation, old and new.
func TestSelectCost(t *testing.T) {
	if os.Getenv("OCEAN_SELECTOR_COST") == "" {
		t.Skip("OCEAN_SELECTOR_COST not set")
	}
	measure := func(name string, f func()) {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		start := time.Now()
		f()
		el := time.Since(start)
		runtime.ReadMemStats(&b)
		t.Logf("%-44s %12v  %8d MB allocated", name, el.Round(time.Microsecond), (b.TotalAlloc-a.TotalAlloc)>>20)
	}
	for _, n := range []int{100, 300, 700, 30000, 300000} {
		vals := make([]uint64, n)
		for i := range vals {
			vals[i] = 1700 + uint64(i%800) // change coins of 1,700 to 2,499: three are needed for 5,000
		}
		if n <= 300 { // at 700 one old call passed 5 GB and was killed
			measure(fmt.Sprintf("old, %d coins, three needed", n), func() { oldSelect(coins(assetA, vals...), 5000, assetA) })
		}
		measure(fmt.Sprintf("new, %d coins, three needed", n), func() { NewSmallestSubsetCoinSelector().SelectUtxos(coins(assetA, vals...), 5000, assetA) })
	}
}
