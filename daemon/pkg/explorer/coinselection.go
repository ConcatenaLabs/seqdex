package explorer

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
)

var (
	// ErrTargetNotReached is returned when the coins of the asset together do not cover the
	// target amount.
	ErrTargetNotReached = errors.New(
		"error on target amount: total utxo amount does not cover target amount",
	)
	// ErrAmountOverflow is returned when the coins selected to cover the target total more
	// than a uint64 holds: their total, and so the change, cannot be given.
	ErrAmountOverflow = errors.New(
		"error on utxos: the utxos covering the target amount total more than can be counted",
	)
)

// SelectUnspents performs a coin selection over the given list of Utxos and
// returns a subset of them of type targetAsset to cover the targetAmount.
// In case any utxo is confidential, it's required that's already unblinded.
//
// The selection takes as few coins as possible, preferring a total from the
// target to ten times it (see bestCombination). It depends only on the coins
// given, not on their order or on any earlier call: the same coins always give
// the same selection, in the same order, which callers that select twice to
// build matching inputs rely on. It never changes the slice it is given, and
// its time and memory grow with the number of coins.
func SelectUnspents(
	utxos []Utxo,
	targetAmount uint64,
	targetAsset string,
) (coins []Utxo, change uint64, err error) {
	unblindedUtxos := make([]Utxo, 0)

	for i := range utxos {
		utxo := utxos[i]
		if utxo.IsConfidential() && !utxo.IsRevealed() {
			err = fmt.Errorf(
				"error on utxos: all confidential utxos must be already unblinded",
			)
			return
		}
		if utxo.Asset() == targetAsset {
			unblindedUtxos = append(unblindedUtxos, utxo)
		}
	}

	// Largest first; coins of equal value in the order of their outpoints, so
	// that the order the coins were given in does not matter.
	sort.SliceStable(unblindedUtxos, func(i, j int) bool {
		a, b := unblindedUtxos[i], unblindedUtxos[j]
		if a.Value() != b.Value() {
			return a.Value() > b.Value()
		}
		if a.Hash() != b.Hash() {
			return a.Hash() < b.Hash()
		}
		return a.Index() < b.Index()
	})

	values := make([]uint64, 0, len(unblindedUtxos))
	for _, u := range unblindedUtxos {
		values = append(values, u.Value())
	}

	indexes := bestCombination(values, targetAmount)
	if len(indexes) <= 0 {
		return nil, 0, ErrTargetNotReached
	}

	selectedUtxos := make([]Utxo, 0, len(indexes))
	totalAmount := uint64(0)
	for _, v := range indexes {
		var fits bool
		if totalAmount, fits = addChecked(totalAmount, values[v]); !fits {
			return nil, 0, ErrAmountOverflow
		}
		selectedUtxos = append(selectedUtxos, unblindedUtxos[v])
	}
	if totalAmount < targetAmount {
		// never met: what bestCombination selects reaches the target
		return nil, 0, ErrTargetNotReached
	}

	return selectedUtxos, totalAmount - targetAmount, nil
}

// addChecked is a+b, and whether it fits in a uint64.
func addChecked(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	return sum, carry == 0
}

// addCapped is a+b, or the largest uint64 when a+b is more than that. Every
// total it is used for is only ever compared with a target, which is a uint64:
// a capped total is at least any target, as the true one is.
func addCapped(a, b uint64) uint64 {
	if sum, ok := addChecked(a, b); ok {
		return sum
	}
	return math.MaxUint64
}

// bestCombination returns the positions of the values to select, `values`
// being sorted largest first. It is the first, in the order below, of the
// combinations of `values` whose total is at least `target` and at most ten
// times it:
//
//	the combinations of 1 value, then of 2, then of 3, and so on, each size in
//	lexicographic order of the values' positions;
//
// or, when there is none, the largest value if it is above the target; or
// nothing.
//
// This strategy used to be carried out as written: every combination of each
// size was made and kept until one fitted. With a coin above ten times the
// target beside coins that together fall short of it, none fits, and the
// enumeration went through all 2^n combinations before its fallback: about a
// billion slices for thirty coins, which took a taker past 30 GB (seqdex #198).
// The same answer is found directly:
//
//   - A value from the target to ten times it is a combination of 1 that fits:
//     the first is the largest such value.
//   - Otherwise every value is either below the target ("small") or above ten
//     times it, and a combination holding one of the latter totals more than
//     ten times the target. So the combinations that fit hold small values
//     only, which are the values after the large ones. The fewest that can
//     reach the target is the smallest k whose k largest small values reach
//     it, and those k values, in a row, are the first combination of k small
//     values. Its first k-1 values total less than the target (k is the
//     fewest), and its last is small, so its total is below twice the target:
//     it fits.
//   - Otherwise the small values together fall short and no combination fits.
//
// No total here wraps around. Ten times a target above a tenth of the largest
// uint64 is more than any value, so every value from the target up fits it.
// The totals of several values are capped at the largest uint64 (addCapped)
// and only compared with the target.
//
// coinselection_oracle_test.go checks it against the old enumeration.
func bestCombination(values []uint64, target uint64) []int {
	n := len(values)
	tenTimes := uint64(math.MaxUint64)
	if target <= math.MaxUint64/10 {
		tenTimes = target * 10
	}
	// the first value of at most ten times the target: every one before it is
	// above that
	firstFitting := sort.Search(n, func(i int) bool { return values[i] <= tenTimes })
	if firstFitting < n && values[firstFitting] >= target {
		return []int{firstFitting}
	}

	// every value from firstFitting on is below the target
	total := uint64(0)
	for i := firstFitting; i < n; i++ {
		total = addCapped(total, values[i])
		if total >= target {
			picked := make([]int, 0, i-firstFitting+1)
			for j := firstFitting; j <= i; j++ {
				picked = append(picked, j)
			}
			return picked
		}
	}

	if n > 0 && values[0] > target {
		return []int{0}
	}
	return nil
}
