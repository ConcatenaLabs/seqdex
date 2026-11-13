package explorer

import (
	"testing"
	"time"
)

// testUtxo is a Utxo carrying only what the selection reads.
type testUtxo struct {
	Utxo
	hash  string
	index uint32
	value uint64
	asset string
}

func (u testUtxo) Hash() string         { return u.hash }
func (u testUtxo) Index() uint32        { return u.index }
func (u testUtxo) Value() uint64        { return u.value }
func (u testUtxo) Asset() string        { return u.asset }
func (u testUtxo) IsConfidential() bool { return false }
func (u testUtxo) IsRevealed() bool     { return false }

// The shape of seqdex #198: a taker address after a number of lifts holds one coin far above
// ten times the target, beside change coins that together do not reach it. No combination of
// coins lands in [target, 10 × target], and the enumeration walked towards all 2^n of them
// before falling back to the large coin.
func TestSelectUnspentsLargeCoinBesideShortChange(t *testing.T) {
	const n = 28
	utxos := []Utxo{testUtxo{hash: "big", value: 1_000_000_000, asset: "a"}}
	for i := 0; i < n; i++ {
		utxos = append(utxos, testUtxo{hash: "small", index: uint32(i), value: 1, asset: "a"})
	}
	start := time.Now()
	coins, change, err := SelectUnspents(utxos, 100, "a")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(coins) != 1 || coins[0].Hash() != "big" || change != 1_000_000_000-100 {
		t.Fatalf("got %d coins, change %d", len(coins), change)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("one selection over %d coins took %s", n+1, elapsed)
	}
}
