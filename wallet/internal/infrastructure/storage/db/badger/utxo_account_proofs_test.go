package dbbadger_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

// An account's reads (balance, spendable, locked) leave out a coin's range and surjection
// proofs, and keep everything else; a read by key, which a spend uses, keeps them.
func TestAccountReadsLeaveOutTheProofs(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.UtxoRepository()

	proof := bytes.Repeat([]byte{7}, 4174)
	conf := coinOf("acc", 1, false)
	conf.ValueCommitment = bytes.Repeat([]byte{8}, 33)
	conf.AssetCommitment = bytes.Repeat([]byte{10}, 33)
	conf.ValueBlinder = bytes.Repeat([]byte{1}, 32)
	conf.AssetBlinder = bytes.Repeat([]byte{2}, 32)
	conf.Nonce = bytes.Repeat([]byte{3}, 33)
	conf.RangeProof = proof
	conf.SurjectionProof = proof[:67]
	locked := coinOf("acc", 2, false)
	locked.LockTimestamp, locked.LockExpiryTimestamp = 1700000000, 4000000000
	plain := coinOf("acc", 3, false)
	addCoins(t, repo, []*domain.Utxo{conf, locked, plain, coinOf("other", 4, false)})

	spendable, err := repo.GetSpendableUtxosForAccount(ctx, "acc", nil)
	require.NoError(t, err)
	require.Len(t, spendable, 2)
	for _, u := range spendable {
		require.Nil(t, u.RangeProof)
		require.Nil(t, u.SurjectionProof)
		if u.Key() == conf.Key() {
			want := *conf
			want.RangeProof, want.SurjectionProof = nil, nil
			require.Equal(t, want, *u, "every other field is read")
			require.True(t, u.IsConfidential() && u.IsRevealed())
		}
	}
	lockedOnes, err := repo.GetLockedUtxosForAccount(ctx, "acc", nil)
	require.NoError(t, err)
	require.Len(t, lockedOnes, 1)
	require.Equal(t, locked.Key(), lockedOnes[0].Key())

	bal, err := repo.GetBalanceForAccount(ctx, "acc")
	require.NoError(t, err)
	var total uint64
	for _, b := range bal {
		total += b.Confirmed + b.Unconfirmed + b.Locked
	}
	require.Equal(t, conf.Value+locked.Value+plain.Value, total)

	byKey, err := repo.GetUtxosByKey(ctx, []domain.UtxoKey{conf.Key()})
	require.NoError(t, err)
	require.Len(t, byKey, 1)
	require.Equal(t, proof, byKey[0].RangeProof, "a read by key keeps the proofs")
	require.Equal(t, proof[:67], byKey[0].SurjectionProof)
}
