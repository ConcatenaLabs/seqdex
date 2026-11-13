package explorer

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// oldSelectUnspents is SelectUnspents as it was before seqdex #198, kept as the oracle the new
// selection is checked against: every combination of every size, made and kept. Its work slice
// was a package variable shared by every call; here it is local, so that the oracle itself can
// be run from several tests.
func oldSelectUnspents(
	utxos []Utxo, targetAmount uint64, targetAsset string,
) (coins []Utxo, change uint64, err error) {
	unblindedUtxos := make([]Utxo, 0)
	totalAmount := uint64(0)
	for i := range utxos {
		utxo := utxos[i]
		if utxo.IsConfidential() && !utxo.IsRevealed() {
			return nil, 0, fmt.Errorf("error on utxos: all confidential utxos must be already unblinded")
		}
		if utxo.Asset() == targetAsset {
			unblindedUtxos = append(unblindedUtxos, utxo)
		}
	}
	sort.Slice(unblindedUtxos, func(i, j int) bool {
		return unblindedUtxos[i].Value() > unblindedUtxos[j].Value()
	})
	values := []uint64{}
	for _, v := range unblindedUtxos {
		values = append(values, v.Value())
	}
	indexes := oldFindIndexes(oldBestCombination(values, targetAmount), values)
	if len(indexes) <= 0 {
		return nil, 0, ErrTargetNotReached
	}
	for _, v := range indexes {
		totalAmount += unblindedUtxos[v].Value()
		coins = append(coins, unblindedUtxos[v])
	}
	return coins, totalAmount - targetAmount, nil
}

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
	result := [][]uint64{}
	for i := 1; i < len(items)+1; i++ {
		result = append(result, getCombination(items, i, 0)...)
		for j := 0; j < len(result); j++ {
			total := sum(result[j])
			if total < target {
				continue
			}
			if total == target {
				return result[j]
			}
			if total <= target*10 {
				return result[j]
			}
		}
	}
	for _, v := range items {
		if v > target {
			return []uint64{v}
		}
	}
	return []uint64{}
}

func oldFindIndexes(list []uint64, values []uint64) []int {
	var indexes []int
loop:
	for _, v := range list {
		for i, v1 := range values {
			if v == v1 {
				for _, o := range indexes {
					if o == i {
						goto next
					}
				}
				indexes = append(indexes, i)
				continue loop
			next:
			}
		}
	}
	return indexes
}

func coinsOf(values []uint64, asset string) []Utxo {
	utxos := make([]Utxo, 0, len(values))
	for i, v := range values {
		utxos = append(utxos, testUtxo{hash: fmt.Sprintf("%064x", i), index: uint32(i % 3), value: v, asset: asset})
	}
	return utxos
}

func valuesOf(coins []Utxo) []uint64 {
	out := []uint64{}
	for _, c := range coins {
		out = append(out, c.Value())
	}
	return out
}

// checkAgainstOld runs both selections on the same coins and requires the same values, in the
// same order, the same change and the same error.
func checkAgainstOld(t *testing.T, values []uint64, target uint64) {
	t.Helper()
	utxos := coinsOf(values, "a")
	oldCoins, oldChange, oldErr := oldSelectUnspents(utxos, target, "a")
	coins, change, err := SelectUnspents(utxos, target, "a")
	if (oldErr == nil) != (err == nil) || change != oldChange ||
		!reflect.DeepEqual(valuesOf(oldCoins), valuesOf(coins)) {
		t.Fatalf("values %v target %d: old %v change %d err %v; new %v change %d err %v",
			values, target, valuesOf(oldCoins), oldChange, oldErr, valuesOf(coins), change, err)
	}
}

// Every multiset of up to 9 values over a set holding values below, within and far above the
// band, against every target from 0 to its total + 2: the new selection is the old one.
func TestSelectUnspentsIsTheOldOneExhaustive(t *testing.T) {
	alphabets := [][]uint64{
		{0, 1, 2, 3, 5, 40},
		{1, 2, 7, 30, 31, 300},
	}
	cases := 0
	for _, alphabet := range alphabets {
		var walk func(prefix []uint64, from int)
		walk = func(prefix []uint64, from int) {
			var total uint64
			for _, v := range prefix {
				total += v
			}
			for target := uint64(0); target <= total+2; target++ {
				checkAgainstOld(t, prefix, target)
				cases++
			}
			if len(prefix) == 9 {
				return
			}
			for i := from; i < len(alphabet); i++ {
				walk(append(append([]uint64{}, prefix...), alphabet[i]), i)
			}
		}
		walk(nil, 0)
	}
	t.Logf("%d cases, 0 differences", cases)
}

// Random sets of up to 14 coins, small and large, repeated values, targets met by one coin,
// by several, by none, and zero.
func TestSelectUnspentsIsTheOldOneRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(198))
	for round := 0; round < 20000; round++ {
		n := rng.Intn(15)
		values := make([]uint64, n)
		spread := []uint64{3, 10, 100, 1000}[rng.Intn(4)]
		for i := range values {
			values[i] = uint64(rng.Int63n(int64(spread))) + 1
			if rng.Intn(6) == 0 {
				values[i] *= 50
			}
		}
		target := uint64(rng.Int63n(int64(spread)*4)) + 1
		if rng.Intn(10) == 0 {
			target = 0
		}
		checkAgainstOld(t, values, target)
	}
}

// The selection covers the target whenever the coins do, holds no coin twice and only coins of
// the asset, gives the right change, and takes no more coins than any covering set needs.
// Totals here are counted without a limit, near 2^64 too.
func TestSelectUnspentsCoversWheneverTheCoinsDo(t *testing.T) {
	rng := rand.New(rand.NewSource(1982))
	for round := 0; round < 30000; round++ {
		n := rng.Intn(40)
		values := make([]uint64, n)
		for i := range values {
			switch rng.Intn(4) {
			case 0:
				values[i] = rng.Uint64() // near 2^64
			case 1:
				values[i] = uint64(rng.Int63n(1_000_000))
			default:
				values[i] = uint64(rng.Int63n(1000))
			}
		}
		target := uint64(rng.Int63n(5000))
		if rng.Intn(4) == 0 {
			target = rng.Uint64()
		}
		utxos := coinsOf(values, "a")
		utxos = append(utxos, testUtxo{hash: "other", value: math.MaxUint64, asset: "b"})
		all := new(big.Int)
		for _, v := range values {
			all.Add(all, new(big.Int).SetUint64(v))
		}
		coins, change, err := SelectUnspents(utxos, target, "a")
		covers := all.Cmp(new(big.Int).SetUint64(target)) >= 0
		if !covers {
			if err != ErrTargetNotReached {
				t.Fatalf("values %v target %d: the coins fall short and err is %v", values, target, err)
			}
			continue
		}
		if err == ErrAmountOverflow {
			continue // a covering set exists, but the one selected totals more than a uint64
		}
		if err != nil {
			t.Fatalf("values %v target %d: %v", values, target, err)
		}
		seen := map[string]bool{}
		got := new(big.Int)
		for _, c := range coins {
			key := fmt.Sprintf("%s:%d", c.Hash(), c.Index())
			if seen[key] || c.Asset() != "a" {
				t.Fatalf("values %v target %d: coin %s twice or of another asset", values, target, key)
			}
			seen[key] = true
			got.Add(got, new(big.Int).SetUint64(c.Value()))
		}
		wantChange := new(big.Int).Sub(got, new(big.Int).SetUint64(target))
		if wantChange.Sign() < 0 || !wantChange.IsUint64() || wantChange.Uint64() != change {
			t.Fatalf("values %v target %d: total %s, change %d", values, target, got, change)
		}
		// as few coins as possible: no fewer of the largest coins reach the target
		sorted := append([]uint64{}, values...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })
		fewest, run := 0, new(big.Int)
		for i, v := range sorted {
			run.Add(run, new(big.Int).SetUint64(v))
			if run.Cmp(new(big.Int).SetUint64(target)) >= 0 {
				fewest = i + 1
				break
			}
		}
		inBand := got.Cmp(new(big.Int).Mul(new(big.Int).SetUint64(target), big.NewInt(10))) <= 0
		if !inBand && len(coins) != fewest {
			t.Fatalf("values %v target %d: %d coins outside the band, %d would do", values, target, len(coins), fewest)
		}
	}
}

// Named edges: the band, the fallback, zero targets, equal values, totals near 2^64.
func TestSelectUnspentsEdges(t *testing.T) {
	max := uint64(math.MaxUint64)
	for _, c := range []struct {
		name   string
		values []uint64
		target uint64
		want   []uint64
		err    error
	}{
		{"one coin in the band, the largest of them", []uint64{5, 61, 38, 600, 59}, 6, []uint64{59}, nil},
		{"the band's upper edge", []uint64{61, 60}, 6, []uint64{60}, nil},
		{"exactly the target", []uint64{6, 1000}, 6, []uint64{6}, nil},
		{"several small coins, the largest ones", []uint64{61, 1, 1, 1, 3, 2}, 6, []uint64{3, 2, 1}, nil},
		{"fallback: the largest coin above the band", []uint64{100, 1000, 1, 1}, 6, []uint64{1000}, nil},
		{"short", []uint64{2, 2}, 6, nil, ErrTargetNotReached},
		{"no coins", nil, 6, nil, ErrTargetNotReached},
		{"zero target, a zero coin", []uint64{5, 0, 3}, 0, []uint64{0}, nil},
		{"zero target, no zero coin", []uint64{5, 3}, 0, []uint64{5}, nil},
		{"zero target, no coins", nil, 0, nil, ErrTargetNotReached},
		{"equal values", []uint64{4, 4, 4, 4}, 10, []uint64{4, 4, 4}, nil},
		{"target above a tenth of 2^64", []uint64{max, max / 2}, max / 3, []uint64{max}, nil},
		{"the whole of 2^64", []uint64{max}, max, []uint64{max}, nil},
		{"selected coins total more than a uint64", []uint64{max - 1, max - 2}, max, nil, ErrAmountOverflow},
		{"the coins total more than a uint64, one is taken", []uint64{max - 5, 10, 7}, 100, []uint64{max - 5}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			coins, _, err := SelectUnspents(coinsOf(c.values, "a"), c.target, "a")
			if err != c.err {
				t.Fatalf("err %v, want %v", err, c.err)
			}
			if c.err == nil && !reflect.DeepEqual(valuesOf(coins), c.want) {
				t.Fatalf("selected %v, want %v", valuesOf(coins), c.want)
			}
		})
	}
}

func shuffled(rng *rand.Rand, utxos []Utxo) []Utxo {
	out := append([]Utxo{}, utxos...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// The same coins give the same selection, in the same order, whatever order they come in and
// however often they are selected; the slice given is left as it was.
func TestSelectUnspentsIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 2000; round++ {
		values := make([]uint64, rng.Intn(60))
		for i := range values {
			values[i] = uint64(rng.Intn(8)) * 100 // many equal values
		}
		utxos := coinsOf(values, "a")
		target := uint64(rng.Intn(2000))
		first, firstChange, firstErr := SelectUnspents(utxos, target, "a")
		for k := 0; k < 4; k++ {
			given := shuffled(rng, utxos)
			before := append([]Utxo{}, given...)
			coins, change, err := SelectUnspents(given, target, "a")
			if !reflect.DeepEqual(coins, first) || change != firstChange || err != firstErr {
				t.Fatalf("values %v target %d: %v then %v", values, target, first, coins)
			}
			if !reflect.DeepEqual(given, before) {
				t.Fatal("the slice given was changed")
			}
		}
	}
}

// Selections running at once in one process do not disturb each other (the old selector shared
// its work slice between calls). Run with -race.
func TestSelectUnspentsConcurrent(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	type job struct {
		utxos  []Utxo
		target uint64
		want   []Utxo
		change uint64
	}
	jobs := make([]job, 64)
	for i := range jobs {
		values := make([]uint64, 5+rng.Intn(20))
		for j := range values {
			values[j] = uint64(rng.Intn(1000))
		}
		jobs[i].utxos = coinsOf(values, "a")
		jobs[i].target = uint64(rng.Intn(3000))
		jobs[i].want, jobs[i].change, _ = SelectUnspents(jobs[i].utxos, jobs[i].target, "a")
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < 500; r++ {
				j := jobs[(g*31+r)%len(jobs)]
				coins, change, _ := SelectUnspents(j.utxos, j.target, "a")
				if !reflect.DeepEqual(coins, j.want) || change != j.change {
					errs <- fmt.Sprintf("goroutine %d: %v change %d, want %v change %d", g, coins, change, j.want, j.change)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// Ten thousand coins: in the shape of #198 (one coin above ten times the target, the rest
// falling short together), and with a target that needs many of them. Each selection takes well
// under a second and allocates less than 4 MiB.
func TestSelectUnspentsTenThousandCoins(t *testing.T) {
	const n = 10000
	shapes := map[string]struct {
		utxos  []Utxo
		target uint64
		want   int
	}{}
	short := []Utxo{testUtxo{hash: "big", value: 1_000_000_000_000, asset: "a"}}
	for i := 0; i < n-1; i++ {
		short = append(short, testUtxo{hash: fmt.Sprintf("%064x", i), value: 1, asset: "a"})
	}
	shapes["#198"] = struct {
		utxos  []Utxo
		target uint64
		want   int
	}{short, 100_000, 1}
	many := make([]uint64, n)
	for i := range many {
		many[i] = 1000 + uint64(i%1400)
	}
	shapes["many needed"] = struct {
		utxos  []Utxo
		target uint64
		want   int
	}{coinsOf(many, "a"), 2_000_000, 0}
	for name, s := range shapes {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		coins, _, err := SelectUnspents(s.utxos, s.target, "a")
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.want > 0 && len(coins) != s.want {
			t.Fatalf("%s: %d coins", name, len(coins))
		}
		alloc := after.TotalAlloc - before.TotalAlloc
		t.Logf("%s: %d coins in, %d selected, %s, %d bytes allocated", name, n, len(coins), elapsed, alloc)
		if elapsed > 250*time.Millisecond {
			t.Fatalf("%s: %s", name, elapsed)
		}
		if alloc > 4<<20 {
			t.Fatalf("%s: %d bytes allocated", name, alloc)
		}
	}
}
