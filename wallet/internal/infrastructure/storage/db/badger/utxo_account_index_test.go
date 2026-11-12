package dbbadger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
)

func indexCoin(account string, i int) *domain.Utxo {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", account, i)))
	return &domain.Utxo{
		UtxoKey:     domain.UtxoKey{TxID: hex.EncodeToString(h[:]), VOut: uint32(i % 4)},
		Value:       uint64(1 + i),
		Asset:       fmt.Sprintf("%064x", 1+i%3),
		Script:      []byte{0x00, 0x14, byte(i % 5)},
		AccountName: account,
	}
}

// writeAsBefore stores `utxos` as a build without the index stored them: each under its key's
// hash, with no index key and no marker.
func writeAsBefore(t *testing.T, r *utxoRepository, utxos []*domain.Utxo) {
	for _, u := range utxos {
		require.NoError(t, r.store.Insert(u.Key().Hash(), *u))
	}
}

func keysOf(utxos []*domain.Utxo) []string {
	out := make([]string, 0, len(utxos))
	for _, u := range utxos {
		out = append(out, fmt.Sprintf("%s:%d/%d/%v", u.TxID, u.VOut, u.LockTimestamp, u.IsConfirmed()))
	}
	sort.Strings(out)
	return out
}

// scanned is what the queries over the whole store give for `account`: the reads as they were
// before the index.
func scanned(t *testing.T, r *utxoRepository, account string, scripts [][]byte) (spendable, locked []*domain.Utxo, balance map[string]domain.Balance) {
	all, err := r.GetAllUtxos(context.Background())
	require.NoError(t, err)
	balance = map[string]domain.Balance{}
	for _, u := range all {
		if u.AccountName != account || u.IsSpent() {
			continue
		}
		b := balance[u.Asset]
		if u.IsLocked() {
			b.Locked += u.Value
		} else if u.IsConfirmed() {
			b.Confirmed += u.Value
		} else {
			b.Unconfirmed += u.Value
		}
		balance[u.Asset] = b
		if !hasScript(u, scripts) {
			continue
		}
		if u.IsLocked() {
			locked = append(locked, u)
		} else {
			spendable = append(spendable, u)
		}
	}
	return
}

func requireReadsMatchScan(t *testing.T, r *utxoRepository, accounts []string, step string) {
	ctx := context.Background()
	for _, account := range accounts {
		for _, scripts := range [][][]byte{nil, {{0x00, 0x14, 1}, {0x00, 0x14, 3}}} {
			wantS, wantL, wantB := scanned(t, r, account, scripts)
			gotS, err := r.GetSpendableUtxosForAccount(ctx, account, scripts)
			require.NoError(t, err)
			gotL, err := r.GetLockedUtxosForAccount(ctx, account, scripts)
			require.NoError(t, err)
			require.Equal(t, keysOf(wantS), keysOf(gotS), "%s: %s spendable", step, account)
			require.Equal(t, keysOf(wantL), keysOf(gotL), "%s: %s locked", step, account)
			if scripts == nil {
				require.Equal(t, len(wantS)+len(wantL), indexKeys(t, r, account),
					"%s: %s: the index holds exactly the unspent coins", step, account)
			}
			gotB, err := r.GetBalanceForAccount(ctx, account)
			require.NoError(t, err)
			require.Len(t, gotB, len(wantB), "%s: %s balance", step, account)
			for asset, b := range wantB {
				require.Equal(t, b, *gotB[asset], "%s: %s balance of %s", step, account, asset)
			}
		}
	}
}

// indexKeys is how many index keys `account` has.
func indexKeys(t *testing.T, r *utxoRepository, account string) int {
	n := 0
	prefix := accountIndexPrefixOf(account)
	require.NoError(t, r.store.Badger().View(func(tx *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := tx.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			n++
		}
		return nil
	}))
	return n
}

func openRepo(t *testing.T, dir string) (*repoManager, *utxoRepository) {
	rm, err := NewRepoManager(dir, nil)
	require.NoError(t, err)
	m := rm.(*repoManager)
	return m, m.utxoRepository
}

// A store written by a build without the index opens, is indexed once, and reads the same
// balances and coin lists as the queries over the whole store; opened again it is not indexed
// a second time.
func TestAStoreWithoutTheIndexIsIndexedOnce(t *testing.T) {
	dir := t.TempDir()
	rm, r := openRepo(t, dir)
	accounts := []string{"a", "b", "ab", "c"}
	var utxos []*domain.Utxo
	for i := 0; i < 2000; i++ {
		u := indexCoin(accounts[i%len(accounts)], i)
		switch i % 7 {
		case 0, 1, 2:
			u.SpentStatus = domain.UtxoStatus{Txid: fmt.Sprintf("%064x", i)}
		case 3:
			u.LockTimestamp, u.LockExpiryTimestamp = 10, 20
		case 4:
			u.ConfirmedStatus = domain.UtxoStatus{BlockHeight: 5, BlockHash: "aa"}
		}
		utxos = append(utxos, u)
	}
	// As the build before left it: coins with no index keys and no marker.
	require.NoError(t, r.store.Badger().DropPrefix(accountIndexMarker, accountIndexPrefix))
	writeAsBefore(t, r, utxos)
	rm.Close()

	rm, r = openRepo(t, dir)
	requireReadsMatchScan(t, r, accounts, "after the one-time index")
	s, err := r.GetSpendableUtxosForAccount(context.Background(), "a", nil)
	require.NoError(t, err)
	require.NotEmpty(t, s)
	rm.Close()

	// Indexed already: coins written now without index keys are not picked up, which shows
	// the second open did not index again.
	rm, r = openRepo(t, dir)
	writeAsBefore(t, r, []*domain.Utxo{indexCoin("d", 9999)})
	rm.Close()
	rm, r = openRepo(t, dir)
	s, err = r.GetSpendableUtxosForAccount(context.Background(), "d", nil)
	require.NoError(t, err)
	require.Empty(t, s, "the store was indexed a second time")
	rm.Close()

	// After a build without the index has written to the store, a rebuild picks its coins up.
	SetRebuildAccountIndex(true)
	defer SetRebuildAccountIndex(false)
	rm, r = openRepo(t, dir)
	defer rm.Close()
	requireReadsMatchScan(t, r, append(accounts, "d"), "after a rebuild")
	s, err = r.GetSpendableUtxosForAccount(context.Background(), "d", nil)
	require.NoError(t, err)
	require.Len(t, s, 1)
}

// Every write keeps the index: after each step of a random run of adds, spends, confirms,
// locks, unlocks and account deletions, the reads equal the queries over the whole store.
func TestTheIndexFollowsEveryWrite(t *testing.T) {
	ctx := context.Background()
	rm, r := openRepo(t, t.TempDir())
	defer rm.Close()
	rng := rand.New(rand.NewSource(178))
	accounts := []string{"a", "b", "ab", "c"}
	var held []domain.UtxoKey
	next := 0
	for step := 0; step < 400; step++ {
		var what string
		pick := func(n int) []domain.UtxoKey {
			if len(held) == 0 {
				return nil
			}
			keys := make([]domain.UtxoKey, 0, n)
			for i := 0; i < n; i++ {
				keys = append(keys, held[rng.Intn(len(held))])
			}
			return keys
		}
		switch op := rng.Intn(20); {
		case op < 6:
			what = "add"
			var utxos []*domain.Utxo
			for i := 0; i < 1+rng.Intn(5); i++ {
				u := indexCoin(accounts[rng.Intn(len(accounts))], next)
				next++
				if rng.Intn(10) == 0 {
					u.SpentStatus = domain.UtxoStatus{Txid: "ff"}
				}
				utxos = append(utxos, u)
				held = append(held, u.Key())
			}
			if rng.Intn(4) == 0 && len(utxos) > 0 {
				utxos = append(utxos, utxos[0]) // added twice
			}
			_, err := r.AddUtxos(ctx, utxos)
			require.NoError(t, err)
		case op < 9:
			what = "spend"
			_, err := r.SpendUtxos(ctx, pick(2), fmt.Sprintf("%064x", step))
			require.NoError(t, err)
		case op < 11:
			what = "confirm spend"
			_, err := r.ConfirmSpendUtxos(ctx, pick(2), domain.UtxoStatus{Txid: "ee", BlockHeight: 9, BlockHash: "bb"})
			require.NoError(t, err)
		case op < 14:
			what = "confirm"
			_, err := r.ConfirmUtxos(ctx, pick(3), domain.UtxoStatus{BlockHeight: 7, BlockHash: "cc"})
			require.NoError(t, err)
		case op < 16:
			what = "lock"
			_, err := r.LockUtxos(ctx, pick(2), time.Now().Unix(), time.Now().Add(time.Hour).Unix())
			require.NoError(t, err)
		case op < 18:
			what = "unlock"
			_, err := r.UnlockUtxos(ctx, pick(2))
			require.NoError(t, err)
		case op < 19:
			what = "a key that is not held"
			_, err := r.SpendUtxos(ctx, []domain.UtxoKey{{TxID: fmt.Sprintf("%064x", 1<<40+step), VOut: 1}}, "dd")
			require.NoError(t, err)
		default:
			what = "delete an account"
			require.NoError(t, r.DeleteUtxosForAccount(ctx, accounts[rng.Intn(len(accounts))]))
		}
		requireReadsMatchScan(t, r, accounts, fmt.Sprintf("step %d (%s)", step, what))
	}
}

// Reads beyond the bound wait for a slot, and a caller that goes while it waits never starts
// its read.
func TestReadsWaitForASlotAndLeaveWhenTheCallerGoes(t *testing.T) {
	SetStoreReadsParallel(2)
	defer SetStoreReadsParallel(8)
	rm, r := openRepo(t, t.TempDir())
	defer rm.Close()
	_, err := r.AddUtxos(context.Background(), []*domain.Utxo{indexCoin("a", 1)})
	require.NoError(t, err)
	require.Equal(t, 2, cap(r.reads))

	var held []func()
	for i := 0; i < 2; i++ {
		done, err := r.reads.take(context.Background())
		require.NoError(t, err)
		held = append(held, done)
	}
	for name, read := range map[string]func(context.Context) error{
		"balance": func(ctx context.Context) error {
			_, err := r.GetBalanceForAccount(ctx, "a")
			return err
		},
		"spendable": func(ctx context.Context) error {
			_, err := r.GetSpendableUtxosForAccount(ctx, "a", nil)
			return err
		},
		"locked": func(ctx context.Context) error {
			_, err := r.GetLockedUtxosForAccount(ctx, "a", nil)
			return err
		},
		"whole store": func(ctx context.Context) error {
			_, err := r.GetAllUtxos(ctx)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		err := read(ctx)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded, name)
		require.Less(t, time.Since(start), time.Second, name)
	}

	// A slot freed lets a waiting read through.
	got := make(chan error, 1)
	go func() {
		_, err := r.GetSpendableUtxosForAccount(context.Background(), "a", nil)
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("a read ran with every slot taken")
	case <-time.After(100 * time.Millisecond):
	}
	held[0]()
	require.NoError(t, <-got)
	held[1]()
}

// An abandoned whole-store query stops too.
func TestAnAbandonedScanStops(t *testing.T) {
	rm, r := openRepo(t, t.TempDir())
	defer rm.Close()
	utxos := make([]*domain.Utxo, 0, 100000)
	for i := 0; i < cap(utxos); i++ {
		utxos = append(utxos, indexCoin("a", i))
	}
	_, err := r.AddUtxos(context.Background(), utxos)
	require.NoError(t, err)
	start := time.Now()
	_, err = r.GetAllUtxos(context.Background())
	require.NoError(t, err)
	full := time.Since(start)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = r.GetAllUtxosForAccount(ctx, "a")
	took := time.Since(start)
	t.Logf("a full scan takes %s; abandoned after 20 ms it returned after %s", full, took)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, took, full/2+50*time.Millisecond)
}
