package smallestsubset_selector

import (
	"math/rand"
	"runtime"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetBestPairs(t *testing.T) {
	type args struct {
		items  []uint64
		target uint64
	}
	tests := []struct {
		name     string
		args     args
		expected []uint64
	}{
		{
			name: "1",
			args: args{
				items:  []uint64{61, 61, 61, 38, 61, 61, 61, 1, 1, 1, 3},
				target: 6,
			},
			expected: []uint64{38},
		},
		{
			name: "2",
			args: args{
				items:  []uint64{61, 61, 61, 61, 61, 61, 1, 1, 1, 3},
				target: 6,
			},
			expected: []uint64{3, 1, 1, 1},
		},
		{
			name: "3",
			args: args{
				items:  []uint64{61, 61},
				target: 6,
			},
			expected: []uint64{61},
		},
		{
			name: "4",
			args: args{
				items:  []uint64{2, 2},
				target: 6,
			},
			expected: []uint64{},
		},
		{
			name: "5",
			args: args{
				items:  []uint64{61, 1, 1, 1, 3, 56},
				target: 6,
			},
			expected: []uint64{56},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// the selector sorts the coins by value, smallest first
			sort.Slice(tt.args.items, func(i, j int) bool {
				return tt.args.items[i] < tt.args.items[j]
			})
			got := []uint64{}
			for _, i := range bestCombination(tt.args.items, tt.args.target) {
				got = append(got, tt.args.items[i])
			}
			sort.Slice(got, func(i, j int) bool { return got[i] > got[j] })
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestFindIndexes(t *testing.T) {
	type args struct {
		list                 []uint64
		unblindedUtxosValues []uint64
	}
	tests := []struct {
		name     string
		args     args
		expected []int
	}{
		{
			name: "1",
			args: args{
				list:                 []uint64{1000},
				unblindedUtxosValues: []uint64{1000, 1000, 1000},
			},
			expected: []int{0},
		},
		{
			name: "2",
			args: args{
				list:                 []uint64{1000, 1000},
				unblindedUtxosValues: []uint64{1000, 2000, 1000},
			},
			expected: []int{0, 2},
		},
		{
			name: "3",
			args: args{
				list: []uint64{2000, 2000},
				unblindedUtxosValues: []uint64{1000, 2000, 1000, 2000, 2000,
					2000},
			},
			expected: []int{1, 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexes := findIndexes(tt.args.list, tt.args.unblindedUtxosValues)
			require.Equal(t, tt.expected, indexes)
		})
	}
}

// oldBestCombination is the selector as it was: every combination of every size, made and kept
// (it was a package variable shared by every call, a data race; here it is local). It is the
// oracle bestCombination is checked against.
func oldBestCombination(items []uint64, target uint64) []uint64 {
	var combination []uint64
	var getCombination func(src []uint64, size int, offset int) [][]uint64
	getCombination = func(src []uint64, size int, offset int) [][]uint64 {
		result := [][]uint64{}
		if size == 0 {
			temp := make([]uint64, len(combination))
			copy(temp, combination)
			return append(result, temp)
		}
		for i := offset; i <= len(src)-size; i++ {
			combination = append(combination, src[i])
			temp := getCombination(src, size-1, i+1)
			result = append(result, temp...)
			combination = combination[:len(combination)-1]
		}
		return result
	}
	sum := func(items []uint64) uint64 {
		var total uint64
		for _, v := range items {
			total += v
		}
		return total
	}
	combinations := [][]uint64{}
	for i := 1; i < len(items)+1; i++ {
		combinations = append(combinations, getCombination(items, i, 0)...)
		for j := 0; j < len(combinations); j++ {
			total := sum(combinations[j])
			if total < target {
				continue
			}
			if total == target {
				return combinations[j]
			}
			if total <= target*10 {
				return combinations[j]
			}
		}
	}
	for _, combo := range combinations {
		if totalAmount := sum(combo); totalAmount >= target {
			return combo
		}
	}
	return []uint64{}
}

// The selector picks the very coins the old enumeration picked, on random sets of coins: small
// and large ones, repeated values, targets met by one coin, by several, by none.
func TestBestCombinationIsTheOldOne(t *testing.T) {
	rng := rand.New(rand.NewSource(181))
	for round := 0; round < 20000; round++ {
		n := rng.Intn(13)
		values := make([]uint64, n)
		spread := []uint64{3, 10, 100, 1000}[rng.Intn(4)]
		for i := range values {
			values[i] = uint64(rng.Int63n(int64(spread))) + 1
			if rng.Intn(6) == 0 {
				values[i] *= 50
			}
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		target := uint64(rng.Int63n(int64(spread)*4)) + 1
		if rng.Intn(10) == 0 {
			target = 0
		}
		want := findIndexes(oldBestCombination(values, target), values)
		got := bestCombination(values, target)
		require.Equal(t, len(want), len(got), "values %v target %d: old %v new %v", values, target, want, got)
		if len(want) > 0 {
			require.Equal(t, want, got, "values %v target %d", values, target)
		}
	}
}

// A selection among many small coins costs their number, not their combinations: the old
// enumeration needed C(5000, 3), about 2·10^10 slices, for this one.
func TestBestCombinationManyCoins(t *testing.T) {
	values := make([]uint64, 5000)
	for i := range values {
		values[i] = 1000 + uint64(i%1400)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := bestCombination(values, 5000)
	runtime.ReadMemStats(&after)
	var total uint64
	for _, i := range got {
		total += values[i]
	}
	require.Len(t, got, 3)
	require.GreaterOrEqual(t, total, uint64(5000))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20))
}

// findIndexes and isIndexOccupied map the old selector's values back to positions, as it did.
func findIndexes(list []uint64, utxosValues []uint64) []int {
	var indexes []int
loop:
	for _, v := range list {
		for i, v1 := range utxosValues {
			if v == v1 {
				if isIndexOccupied(i, indexes) {
					continue
				} else {
					indexes = append(indexes, i)
					continue loop
				}
			}
		}
	}
	return indexes
}
func isIndexOccupied(i int, list []int) bool {
	for _, v := range list {
		if v == i {
			return true
		}
	}
	return false
}
