package smallestsubset_selector

import (
	"fmt"
	"math"
	"math/bits"
	"sort"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/core/ports"
)

var (
	ErrBlindedUtxos           = fmt.Errorf("error on utxos: all confidential utxos must be already revealed")
	ErrTargetAmountNotReached = fmt.Errorf("not found enough utxos to cover target amount")
	// ErrAmountOverflow is returned when the utxos that cover the target total more than a
	// uint64 holds: their total, and so the change, cannot be given.
	ErrAmountOverflow = fmt.Errorf("the utxos covering the target amount total more than can be counted")
)

// addChecked is a+b, and whether it fits in a uint64.
func addChecked(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	return sum, carry == 0
}

// addCapped is a+b, or the largest uint64 when a+b is more than that. Every total it is used
// for is only ever compared with a target, which is a uint64: a capped total is at least any
// target, as the true one is.
func addCapped(a, b uint64) uint64 {
	if sum, ok := addChecked(a, b); ok {
		return sum
	}
	return math.MaxUint64
}

type selector struct{}

func NewSmallestSubsetCoinSelector() ports.CoinSelector {
	return &selector{}
}

func (s *selector) SelectUtxos(
	utxos []*domain.Utxo, targetAmount uint64, targetAsset string,
) ([]*domain.Utxo, uint64, error) {
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].Value < utxos[j].Value
	})

	targetUtxos := make([]*domain.Utxo, 0)
	// balance is the total of the asset's utxos while it fits in a uint64 (balanceFits). One
	// that does not fit is more than any target, and never equal to one.
	balance, balanceFits := uint64(0), true
	for i := range utxos {
		utxo := utxos[i]
		if utxo.IsConfidential() && !utxo.IsRevealed() {
			return nil, 0, ErrBlindedUtxos
		}
		if utxo.Asset == targetAsset {
			targetUtxos = append(targetUtxos, utxo)
			if balanceFits {
				balance, balanceFits = addChecked(balance, utxo.Value)
			}
		}
	}

	if balanceFits && targetAmount == balance {
		return targetUtxos, 0, nil
	}

	indexes := selectUtxos(targetAmount, targetUtxos)
	if len(indexes) <= 0 {
		return nil, 0, ErrTargetAmountNotReached
	}

	selectedUtxos := make([]*domain.Utxo, 0)
	totalAmount := uint64(0)
	for _, v := range indexes {
		var fits bool
		if totalAmount, fits = addChecked(totalAmount, targetUtxos[v].Value); !fits {
			return nil, 0, ErrAmountOverflow
		}
		selectedUtxos = append(selectedUtxos, targetUtxos[v])
	}
	if totalAmount < targetAmount {
		// never met: what bestCombination selects reaches the target
		return nil, 0, ErrTargetAmountNotReached
	}

	change := totalAmount - targetAmount
	return selectedUtxos, change, nil
}

// selectUtxos returns the indexes of the utxos to select, `utxos` being sorted by value,
// smallest first. The goal of this strategy is to select as few utxos as possible covering
// the target amount.
func selectUtxos(targetAmount uint64, utxos []*domain.Utxo) []int {
	values := make([]uint64, 0, len(utxos))
	for _, u := range utxos {
		values = append(values, u.Value)
	}
	return bestCombination(values, targetAmount)
}

// bestCombination is the first, in the order below, of the combinations of `values` (sorted,
// smallest first) whose total is at least `target` and at most ten times it:
//
//	the combinations of 1 value, then of 2, then of 3, and so on, each size in lexicographic
//	order of the values' positions;
//
// or, when there is none, the first whose total is at least `target`; or nothing, when all of
// them together fall short. It returns the positions of the values it selects.
//
// This strategy used to be carried out as written: every combination of each size was made and
// kept until one fitted, C(n, k) of them, which for an account of a thousand small coins and a
// target needing three is 166 million slices, several gigabytes for one request, and it took the
// wallet daemon past its memory limit (seqdex #181). The same answer is found directly:
//
//   - A value from target to ten times it is a combination of 1 that fits: the first is the
//     smallest such value.
//   - Otherwise every value is either below the target ("small") or above ten times it, and a
//     combination holding one of the latter totals more than ten times the target. So the
//     combinations that fit hold small values only. The fewest that can reach the target is the
//     smallest k whose k largest small values reach it, and the first combination of k small
//     values to reach it is built position by position: each takes the smallest value that
//     still lets the remaining positions, filled with the largest values after it, reach the
//     target. Its first k-1 values total less than the target (k is the fewest), and its last
//     is small, so its total is below twice the target: it fits.
//   - Otherwise the small values together fall short, no combination fits, and the first that
//     reaches the target is the smallest value above it, if there is one.
//
// No total here wraps around. Ten times a target above a tenth of the largest uint64 is more
// than any value, so every value from the target up fits it. The totals of several values are
// capped at the largest uint64 (addCapped) and only compared with the target.
//
// selector_internal_test.go checks it against the old enumeration, and selector_overflow_test.go
// against an enumeration that adds without a limit.
func bestCombination(values []uint64, target uint64) []int {
	n := len(values)
	tenTimes := uint64(math.MaxUint64)
	if target <= math.MaxUint64/10 {
		tenTimes = target * 10
	}
	// the first value of at least `target`
	firstAtLeast := sort.Search(n, func(i int) bool { return values[i] >= target })
	if firstAtLeast < n && values[firstAtLeast] <= tenTimes {
		return []int{firstAtLeast}
	}

	small := values[:firstAtLeast] // every value below the target
	m := len(small)
	// tail[j] is the total of the j largest small values
	tail := make([]uint64, m+1)
	for j := 1; j <= m; j++ {
		tail[j] = addCapped(tail[j-1], small[m-j])
	}
	k := 0
	for j := 1; j <= m; j++ {
		if tail[j] >= target {
			k = j
			break
		}
	}
	if k == 0 {
		if firstAtLeast < n {
			return []int{firstAtLeast}
		}
		return nil
	}

	picked := make([]int, 0, k)
	var total uint64
	from := 0
	for p := 1; p <= k; p++ {
		rest := k - p // positions left after this one, filled with the largest values
		last := m - rest
		// the smallest value at a position in [from, last) that still lets the target be reached
		need := uint64(0)
		if reached := addCapped(total, tail[rest]); target > reached {
			need = target - reached
		}
		i := from + sort.Search(last-from, func(j int) bool { return small[from+j] >= need })
		picked = append(picked, i)
		total = addCapped(total, small[i])
		from = i + 1
	}
	return picked
}
