package dbbadger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

func utxoOf(tx int, vout uint32) *domain.Utxo {
	return &domain.Utxo{
		UtxoKey:     domain.UtxoKey{TxID: fmt.Sprintf("%064x", tx), VOut: vout},
		Value:       1000,
		Asset:       fmt.Sprintf("%064x", 1),
		Script:      []byte{0x00, 0x14},
		AccountName: "acc",
	}
}

// Confirming, spending, locking and unlocking find each UTXO under its key: 3,000 UTXOs are
// confirmed one by one, as the scanner reports them, in a fraction of what a scan of the store
// for each would take, and every state change reads back.
func TestUtxosAreFoundUnderTheirKey(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.UtxoRepository()
	const n = 3000
	utxos := make([]*domain.Utxo, n)
	keys := make([]domain.UtxoKey, n)
	for i := range utxos {
		utxos[i] = utxoOf(i, 0)
		keys[i] = utxos[i].Key()
	}
	added, err := repo.AddUtxos(ctx, utxos)
	require.NoError(t, err)
	require.Equal(t, n, added)

	start := time.Now()
	for _, k := range keys {
		c, err := repo.ConfirmUtxos(ctx, []domain.UtxoKey{k}, domain.UtxoStatus{BlockHeight: 7})
		require.NoError(t, err)
		require.Equal(t, 1, c)
	}
	took := time.Since(start)
	t.Logf("confirmed %d UTXOs one by one in %s", n, took)
	require.Less(t, took, 30*time.Second)

	got, err := repo.GetUtxosByKey(ctx, keys[:3])
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, u := range got {
		require.Equal(t, keys[i], u.Key())
		require.True(t, u.IsConfirmed())
	}
	c, err := repo.ConfirmUtxos(ctx, keys[:1], domain.UtxoStatus{BlockHeight: 8})
	require.NoError(t, err)
	require.Equal(t, 0, c, "a confirmed UTXO is not confirmed again")

	l, err := repo.LockUtxos(ctx, keys[1:2], time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.Equal(t, 1, l)
	got, err = repo.GetUtxosByKey(ctx, keys[1:2])
	require.NoError(t, err)
	require.True(t, got[0].IsLocked())
	u, err := repo.UnlockUtxos(ctx, keys[1:2])
	require.NoError(t, err)
	require.Equal(t, 1, u)

	s, err := repo.SpendUtxos(ctx, keys[2:3], fmt.Sprintf("%064x", 999999))
	require.NoError(t, err)
	require.Equal(t, 1, s)
	got, err = repo.GetUtxosByKey(ctx, keys[2:3])
	require.NoError(t, err)
	require.True(t, got[0].IsSpent())
	require.Equal(t, uint64(1000), got[0].Value, "the rest of the record is kept")
}

// The key's hash keeps one byte of the output index: an output 256 apart from one stored is not
// taken for it.
func TestAnOutputSharingAKeyHashIsNotTakenForAnother(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.UtxoRepository()
	_, err = repo.AddUtxos(ctx, []*domain.Utxo{utxoOf(1, 0)})
	require.NoError(t, err)
	other := domain.UtxoKey{TxID: fmt.Sprintf("%064x", 1), VOut: 256}
	got, err := repo.GetUtxosByKey(ctx, []domain.UtxoKey{other})
	require.NoError(t, err)
	require.Empty(t, got)
	c, err := repo.ConfirmUtxos(ctx, []domain.UtxoKey{other}, domain.UtxoStatus{BlockHeight: 7})
	require.NoError(t, err)
	require.Equal(t, 0, c)
	got, err = repo.GetUtxosByKey(ctx, []domain.UtxoKey{{TxID: fmt.Sprintf("%064x", 1), VOut: 0}})
	require.NoError(t, err)
	require.False(t, got[0].IsConfirmed(), "the stored output is untouched")
}

// A spend the scanner sees of an output the wallet never held is answered from the key's hash,
// holding nothing, without a scan of the store: 3,000 of them beside 3,000 stored UTXOs.
func TestASpendOfAnOutputNeverHeldIsNoScan(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.UtxoRepository()
	const n = 3000
	utxos := make([]*domain.Utxo, n)
	for i := range utxos {
		utxos[i] = utxoOf(i, 0)
	}
	_, err = repo.AddUtxos(ctx, utxos)
	require.NoError(t, err)
	start := time.Now()
	for i := 0; i < n; i++ {
		c, err := repo.ConfirmSpendUtxos(ctx, []domain.UtxoKey{{TxID: fmt.Sprintf("%064x", n+i), VOut: 1}}, domain.UtxoStatus{BlockHeight: 9})
		require.NoError(t, err)
		require.Equal(t, 0, c)
	}
	took := time.Since(start)
	t.Logf("%d spends of outputs never held in %s", n, took)
	require.Less(t, took, 10*time.Second)
}
