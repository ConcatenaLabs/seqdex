package dbbadger_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

// coinOf is the i-th coin of `account`, spent or not, confirmed at a height.
func coinOf(account string, i int, spent bool) *domain.Utxo {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", account, i)))
	u := &domain.Utxo{
		UtxoKey:         domain.UtxoKey{TxID: hex.EncodeToString(h[:]), VOut: uint32(i % 3)},
		Value:           uint64(1000 + i),
		Asset:           fmt.Sprintf("%064x", 1+i%2),
		Script:          append([]byte{0x00, 0x14}, h[:20]...),
		AccountName:     account,
		ConfirmedStatus: domain.UtxoStatus{BlockHeight: uint64(100 + i), BlockTime: 1700000000, BlockHash: "aa"},
	}
	if spent {
		u.SpentStatus = domain.UtxoStatus{Txid: fmt.Sprintf("%064x", i+1)}
	}
	return u
}

func addCoins(t testing.TB, repo domain.UtxoRepository, utxos []*domain.Utxo) {
	ctx := context.Background()
	for len(utxos) > 0 {
		n := len(utxos)
		if n > 5000 {
			n = 5000
		}
		_, err := repo.AddUtxos(ctx, utxos[:n])
		require.NoError(t, err)
		utxos = utxos[n:]
	}
}

// storeWith opens a store holding `own` unspent coins of "target" and `others` spent coins of
// 80 other accounts.
func storeWith(t *testing.T, own, others int) domain.UtxoRepository {
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(rm.Close)
	repo := rm.UtxoRepository()
	utxos := make([]*domain.Utxo, 0, own+others)
	for i := 0; i < own; i++ {
		utxos = append(utxos, coinOf("target", i, false))
	}
	for i := 0; i < others; i++ {
		utxos = append(utxos, coinOf(fmt.Sprintf("bip84-account%d", 1+i%80), i, true))
	}
	addCoins(t, repo, utxos)
	return repo
}

// readsTake is how long `rounds` balance, spendable-coin and locked-coin reads of "target" take.
func readsTake(t *testing.T, repo domain.UtxoRepository, own, rounds int) time.Duration {
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < rounds; i++ {
		b, err := repo.GetBalanceForAccount(ctx, "target")
		require.NoError(t, err)
		var total uint64
		for _, v := range b {
			total += v.Confirmed
		}
		require.NotZero(t, total)
		s, err := repo.GetSpendableUtxosForAccount(ctx, "target", nil)
		require.NoError(t, err)
		require.Len(t, s, own)
		l, err := repo.GetLockedUtxosForAccount(ctx, "target", nil)
		require.NoError(t, err)
		require.Empty(t, l)
	}
	return time.Since(start)
}

// An account's balance and coin lists cost its own coins: ten times more coins of other
// accounts in the store leave the time of a read where it was.
func TestAnAccountsReadsDoNotGrowWithTheStore(t *testing.T) {
	if testing.Short() {
		t.Skip("fills two stores")
	}
	const own, rounds = 50, 10
	small := storeWith(t, own, 20000)
	big := storeWith(t, own, 200000)
	readsTake(t, small, own, 1)
	readsTake(t, big, own, 1)
	ts := readsTake(t, small, own, rounds)
	tb := readsTake(t, big, own, rounds)
	t.Logf("%d rounds of reads of 50 coins: beside 20,000 other coins %s, beside 200,000 %s (x%.1f)",
		rounds, ts, tb, float64(tb)/float64(ts))
	require.Less(t, float64(tb), 3*float64(ts)+float64(20*time.Millisecond),
		"a read grows with other accounts' coins")
}

// A read whose caller has gone stops: an account of 200,000 unspent coins read under a 20 ms
// deadline returns the deadline's error well before a full read would end.
func TestAnAbandonedReadStops(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a store")
	}
	repo := storeWith(t, 200000, 0)
	start := time.Now()
	_, err := repo.GetSpendableUtxosForAccount(context.Background(), "target", nil)
	require.NoError(t, err)
	full := time.Since(start)

	for name, read := range map[string]func(ctx context.Context) error{
		"balance": func(ctx context.Context) error {
			_, err := repo.GetBalanceForAccount(ctx, "target")
			return err
		},
		"spendable": func(ctx context.Context) error {
			_, err := repo.GetSpendableUtxosForAccount(ctx, "target", nil)
			return err
		},
		"locked": func(ctx context.Context) error {
			_, err := repo.GetLockedUtxosForAccount(ctx, "target", nil)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		start := time.Now()
		err := read(ctx)
		took := time.Since(start)
		cancel()
		t.Logf("%s: a full read takes %s; abandoned after 20 ms it returned after %s with %v",
			name, full, took, err)
		require.ErrorIs(t, err, context.DeadlineExceeded, "%s: the read ran to its end", name)
		require.Less(t, took, full/2+50*time.Millisecond, name)
	}
}
