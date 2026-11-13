package smallestsubset_selector

import (
	"math"
	"math/big"
	"math/rand"
	"sort"
	"testing"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/stretchr/testify/require"
)

// exactSelect is the strategy carried out as it reads, on totals that have no limit (math/big):
// all the asset's coins when they total the target exactly; else the first combination, by size
// and then by position, whose total is from the target to ten times it; else the first whose
// total reaches the target. A selection whose total is more than a uint64 holds cannot state
// its total or its change: ErrAmountOverflow.
func exactSelect(utxos []*domain.Utxo, target uint64, asset string) ([]*domain.Utxo, uint64, error) {
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].Value < utxos[j].Value })
	var coins []*domain.Utxo
	for _, u := range utxos {
		if u.Asset == asset {
			coins = append(coins, u)
		}
	}
	big64 := func(v uint64) *big.Int { return new(big.Int).SetUint64(v) }
	totalOf := func(positions []int) *big.Int {
		t := new(big.Int)
		for _, p := range positions {
			t.Add(t, big64(coins[p].Value))
		}
		return t
	}
	all := make([]int, len(coins))
	for i := range all {
		all[i] = i
	}
	tgt := big64(target)
	if totalOf(all).Cmp(tgt) == 0 {
		return coins, 0, nil
	}

	var combos [][]int // every combination of positions: by size, then lexicographic
	var walk func(size, from int, cur []int)
	walk = func(size, from int, cur []int) {
		if size == 0 {
			combos = append(combos, append([]int(nil), cur...))
			return
		}
		for i := from; i <= len(coins)-size; i++ {
			walk(size-1, i+1, append(cur, i))
		}
	}
	for size := 1; size <= len(coins); size++ {
		walk(size, 0, nil)
	}
	ten := new(big.Int).Mul(tgt, big.NewInt(10))
	var chosen []int
	for _, c := range combos {
		if t := totalOf(c); t.Cmp(tgt) >= 0 && t.Cmp(ten) <= 0 {
			chosen = c
			break
		}
	}
	if chosen == nil {
		for _, c := range combos {
			if totalOf(c).Cmp(tgt) >= 0 {
				chosen = c
				break
			}
		}
	}
	if chosen == nil {
		return nil, 0, ErrTargetAmountNotReached
	}
	total := totalOf(chosen)
	if !total.IsUint64() {
		return nil, 0, ErrAmountOverflow
	}
	selected := make([]*domain.Utxo, 0, len(chosen))
	for _, p := range chosen {
		selected = append(selected, coins[p])
	}
	return selected, total.Uint64() - target, nil
}

// sameAsExact runs the selector and the unlimited enumeration on the same coins, and requires
// the same coins, change and error. A selection never falls short of its target.
func sameAsExact(t *testing.T, utxos []*domain.Utxo, target uint64, asset string) ([]*domain.Utxo, uint64, error) {
	t.Helper()
	a := append([]*domain.Utxo(nil), utxos...)
	b := append([]*domain.Utxo(nil), utxos...)
	wantUs, wantChange, wantErr := exactSelect(a, target, asset)
	gotUs, gotChange, gotErr := NewSmallestSubsetCoinSelector().SelectUtxos(b, target, asset)
	require.Equal(t, wantErr, gotErr, "target %d", target)
	require.Equal(t, keysOf(wantUs), keysOf(gotUs), "target %d", target)
	require.Equal(t, wantChange, gotChange, "target %d", target)
	if gotErr == nil {
		total := new(big.Int)
		for _, u := range gotUs {
			total.Add(total, new(big.Int).SetUint64(u.Value))
		}
		want := new(big.Int).Add(new(big.Int).SetUint64(target), new(big.Int).SetUint64(gotChange))
		require.Zero(t, total.Cmp(want), "the coins total the target plus the change, counted without a limit")
	}
	return gotUs, gotChange, gotErr
}

const (
	two60 = uint64(1) << 60
	two61 = uint64(1) << 61
	two62 = uint64(1) << 62
	two63 = uint64(1) << 63
	max64 = uint64(math.MaxUint64)
)

// Amounts near the largest uint64: no total wraps around, in the selection or in what it returns.
func TestSelectNearTheLargestAmount(t *testing.T) {
	t.Run("a target above a tenth of the largest amount: ten times it is more than any coin", func(t *testing.T) {
		// 10 x 2^61 wraps to 2^62: the coin of 2^63 then looked too large, and two small ones were taken
		us, change, err := sameAsExact(t, coins(assetA, two60, two60+1, two60+2, two63), two61, assetA)
		require.NoError(t, err)
		require.Len(t, us, 1)
		require.Equal(t, two63, us[0].Value)
		require.Equal(t, two63-two61, change)
	})
	t.Run("small coins that total more than the largest amount: the fewest are still found", func(t *testing.T) {
		// 2^63 + 2^63 wraps to 0: the coins then looked short of the target
		us, change, err := sameAsExact(t, coins(assetA, two62, two62, two63, two63), two63+10, assetA)
		require.NoError(t, err)
		require.Len(t, us, 2)
		require.Equal(t, two62-10, change)
	})
	t.Run("a balance that wraps onto the target is not the target", func(t *testing.T) {
		// 2^63 + 2^63 + 100 wraps to 100: all three coins were returned for 100, with no change
		us, change, err := sameAsExact(t, coins(assetA, two63, two63, 100), 100, assetA)
		require.NoError(t, err)
		require.Len(t, us, 1)
		require.Equal(t, uint64(100), us[0].Value)
		require.Zero(t, change)
	})
	t.Run("coins covering the target that total more than the largest amount: an error, no wrapped total", func(t *testing.T) {
		_, _, err := sameAsExact(t, coins(assetA, two63, two63+5), max64, assetA)
		require.ErrorIs(t, err, ErrAmountOverflow)
	})
	t.Run("the largest target and coin", func(t *testing.T) {
		us, change, err := sameAsExact(t, coins(assetA, 1, max64), max64, assetA)
		require.NoError(t, err)
		require.Len(t, us, 1)
		require.Zero(t, change)
		_, _, err = sameAsExact(t, coins(assetA, 1, max64-2), max64, assetA)
		require.ErrorIs(t, err, ErrTargetAmountNotReached)
	})
	t.Run("an account holding more of an asset than a uint64 counts still selects", func(t *testing.T) {
		// 10,000 coins of the largest amount one output carries (21e14): 2.1e19 in all
		const most = uint64(21_000_000 * 100_000_000)
		many := coins(assetA, repeat(most, 10000)...)
		us, change, err := NewSmallestSubsetCoinSelector().SelectUtxos(many, 5*most-7, assetA)
		require.NoError(t, err)
		require.Len(t, us, 5)
		require.Equal(t, uint64(7), change)
	})
}

// The selector against the unlimited enumeration on random coins and targets drawn around the
// places a uint64 total wraps: 2^61 to 2^64, a tenth of the largest amount, and small amounts.
func TestSelectIsExactAtAnyAmount(t *testing.T) {
	rng := rand.New(rand.NewSource(182))
	amount := func() uint64 {
		near := []uint64{0, 50, max64 / 10, two60, two61, two62, two63, max64 - 60}[rng.Intn(8)]
		return near + uint64(rng.Intn(60))
	}
	for round := 0; round < 30000; round++ {
		vals := make([]uint64, rng.Intn(9))
		for i := range vals {
			vals[i] = amount()
		}
		target := amount()
		switch rng.Intn(4) {
		case 0: // the total of some of the coins, when it fits, or one atom either side
			sum, fits := uint64(0), true
			for _, v := range vals {
				if rng.Intn(2) == 0 && fits {
					sum, fits = addChecked(sum, v)
				}
			}
			if fits {
				target = sum + uint64(rng.Intn(3)) - 1
			}
		case 1:
			target = max64 - uint64(rng.Intn(3))
		}
		us := coins(assetA, vals...)
		if rng.Intn(3) == 0 {
			us = append(us, coins(assetB, amount(), amount())...)
		}
		sameAsExact(t, us, target, assetA)
	}
}
