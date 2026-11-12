package domain_test

import (
	"testing"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/stretchr/testify/require"
)

// A request answered while the wallet is locked (seqdexd locks it on its way
// down, or before it unlocks it at start) gets an error, never a panic: a
// panic in one gRPC call takes the whole daemon down with every account's
// scan, and its scan heights are not saved.
func TestALockedWalletAnswersWithAnErrorNotAPanic(t *testing.T) {
	w, err := newTestWallet()
	require.NoError(t, err)
	require.NoError(t, w.Unlock(password))

	account, err := w.CreateAccount("maker", 0, false)
	require.NoError(t, err)
	require.NotNil(t, account)

	key, err := account.GetMasterBlindingKey()
	require.NoError(t, err)
	require.NotEmpty(t, key)

	require.NoError(t, w.Lock(password))

	t.Run("an account's master blinding key", func(t *testing.T) {
		require.NotPanics(t, func() {
			key, err = account.GetMasterBlindingKey()
		})
		require.ErrorIs(t, err, domain.ErrWalletLocked)
		require.Empty(t, key)
	})

	t.Run("a new account", func(t *testing.T) {
		var created *domain.Account
		require.NotPanics(t, func() {
			created, err = w.CreateAccount("another", 0, false)
		})
		require.ErrorIs(t, err, domain.ErrWalletLocked)
		require.Nil(t, created)
	})

	t.Run("a new address", func(t *testing.T) {
		require.NotPanics(t, func() {
			_, err = w.DeriveNextExternalAddressForAccount(account.Namespace)
		})
		require.Error(t, err)
	})

	t.Run("every derived address", func(t *testing.T) {
		require.NotPanics(t, func() {
			_, err = w.AllDerivedAddressesForAccount(account.Namespace)
		})
		require.Error(t, err)
	})
}
