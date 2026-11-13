package dbbadger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

// The wallet is decoded once and kept: every read after a write sees the write (an account, a
// label, derived addresses, a deleted account, a reset and a new wallet), and a reader that
// changes the wallet it was given changes nothing kept.
func TestTheKeptWalletFollowsEveryWrite(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.WalletRepository()
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)))
	require.NoError(t, repo.UnlockWallet(ctx, "password"))

	info, err := repo.CreateAccount(ctx, "acc", 1, false)
	require.NoError(t, err)
	w, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	acc, err := w.GetAccount("acc")
	require.NoError(t, err, "a read after an account is made sees it")

	addrs, err := repo.DeriveNextExternalAddressesForAccount(ctx, "acc", 2)
	require.NoError(t, err)
	w, err = repo.GetWallet(ctx)
	require.NoError(t, err)
	acc, _ = w.GetAccount("acc")
	require.Equal(t, uint(2), acc.NextExternalIndex)
	for _, a := range addrs {
		require.Contains(t, acc.DerivationPathByScript, a.Script, "a read after a derivation sees its address")
	}
	held := len(acc.DerivationPathByScript)

	// a reader that changes what it was given: the kept wallet stays as stored
	w.Accounts[info.Namespace].Label = "changed by a reader"
	w.Accounts[info.Namespace].NextExternalIndex = 99
	delete(w.AccountsByLabel, "acc")
	_, err = w.DeriveNextExternalAddressForAccount(info.Namespace)
	require.NoError(t, err)
	w2, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	acc2, err := w2.GetAccount("acc")
	require.NoError(t, err)
	require.Equal(t, "acc", acc2.Label)
	require.Equal(t, uint(2), acc2.NextExternalIndex)
	require.Len(t, acc2.DerivationPathByScript, held, "a reader's derivation reached the kept wallet")

	require.NoError(t, repo.UpdateWallet(ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
		return w, w.SetLabelForAccount("acc", "renamed")
	}))
	w3, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	_, err = w3.GetAccount("renamed")
	require.NoError(t, err, "a read after a label is set sees it")

	require.NoError(t, repo.DeleteAccount(ctx, "renamed"))
	w4, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	_, err = w4.GetAccount(info.Namespace)
	require.ErrorIs(t, err, domain.ErrAccountNotFound, "a read after a deletion does not see the account")

	rm.Reset()
	_, err = repo.GetWallet(ctx)
	require.Error(t, err, "after a reset there is no wallet")
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)), "a restore writes a new wallet")
	w5, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	require.Empty(t, w5.Accounts, "a read after the restore sees the new wallet")
}

// Derivations at once each keep their address: none is lost to another's write.
func TestDerivationsAtOnceAreAllKept(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.WalletRepository()
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)))
	require.NoError(t, repo.UnlockWallet(ctx, "password"))
	_, err = repo.CreateAccount(ctx, "acc", 1, false)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.DeriveNextExternalAddressesForAccount(ctx, "acc", 1)
			require.NoError(t, err)
			_, err = repo.GetWallet(ctx)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	w, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	acc, _ := w.GetAccount("acc")
	require.Equal(t, uint(20), acc.NextExternalIndex)
	require.Len(t, acc.DerivationPathByScript, 20)
}

// A write whose caller has gone, or that waited longer than its limit, writes nothing.
func TestAWriteLeavesWithItsCaller(t *testing.T) {
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager(t.TempDir(), nil)
	require.NoError(t, err)
	defer rm.Close()
	repo := rm.WalletRepository()
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)))
	require.NoError(t, repo.UnlockWallet(ctx, "password"))
	_, err = repo.CreateAccount(ctx, "acc", 1, false)
	require.NoError(t, err)

	hold := make(chan struct{})
	held := make(chan struct{})
	go repo.UpdateWallet(ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
		close(held)
		<-hold
		return w, nil
	})
	<-held

	gone, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = repo.DeriveNextExternalAddressesForAccount(gone, "acc", 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	dbbadger.SetWalletWriteWait(100 * time.Millisecond)
	defer dbbadger.SetWalletWriteWait(30 * time.Second)
	_, err = repo.DeriveNextExternalAddressesForAccount(ctx, "acc", 1)
	require.True(t, errors.Is(err, dbbadger.ErrWalletBusy), "got %v", err)

	close(hold)
	time.Sleep(50 * time.Millisecond)
	w, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	acc, _ := w.GetAccount("acc")
	require.Equal(t, uint(0), acc.NextExternalIndex, "a derivation given up on derived nothing")
}
