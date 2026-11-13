package dbbadger_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/core/ports"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/stretchr/testify/require"
)

// storeWithWallet is a store on disk holding a wallet with account "a", opened again so that
// nothing of the wallet is kept in memory yet: the first read reads the file.
func storeWithWallet(t *testing.T) (ports.RepoManager, domain.WalletRepository, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	rm, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	require.NoError(t, rm.WalletRepository().CreateWallet(ctx, newTestWallet(t)))
	require.NoError(t, rm.WalletRepository().UnlockWallet(ctx, "password"))
	_, err = rm.WalletRepository().CreateAccount(ctx, "a", 1, false)
	require.NoError(t, err)
	rm.Close()

	rm, err = dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	t.Cleanup(rm.Close)
	return rm, rm.WalletRepository(), filepath.Join(dir, "wallet.state")
}

// holdFirstRead holds the first read of the wallet's file between reading it and keeping what
// it read. It returns a channel closed once that read is held, and the function that lets it go.
func holdFirstRead(repo domain.WalletRepository) (held chan struct{}, letGo func()) {
	held, release := make(chan struct{}), make(chan struct{})
	var taken atomic.Bool // only the first read is held: the others pass
	dbbadger.SetWalletLoadedHook(repo, func() {
		if taken.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	})
	return held, func() { close(release) }
}

// A read of the file that began before a write, and ends after a later write failed and
// dropped what was kept, does not put back the wallet it read: the next read sees the write.
// Kept, the old wallet would also be what the next write starts from and writes over the file.
func TestAReadInFlightDoesNotPutBackAnOlderWalletAfterAFailedWrite(t *testing.T) {
	ctx := context.Background()
	_, repo, state := storeWithWallet(t)
	held, letGo := holdFirstRead(repo)

	done := make(chan error, 1)
	go func() {
		_, err := repo.GetWallet(ctx) // reads the wallet with account "a" only, and is held
		done <- err
	}()
	<-held

	_, err := repo.CreateAccount(ctx, "b", 1, false) // written, and kept
	require.NoError(t, err)
	// the next write fails before it replaces the file: where it writes is a directory
	require.NoError(t, os.Mkdir(state+".tmp", 0o700))
	err = repo.UpdateWallet(ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
		return w, w.SetLabelForAccount("b", "never written")
	})
	require.Error(t, err)
	require.NoError(t, os.Remove(state+".tmp"))

	letGo()
	require.NoError(t, <-done)

	w, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	_, err = w.GetAccount("b")
	require.NoError(t, err, "the read in flight put back the wallet from before account b")
	_, err = w.GetAccount("never written")
	require.ErrorIs(t, err, domain.ErrAccountNotFound)

	// and a write after it starts from the file's wallet: both accounts are still there
	_, err = repo.DeriveNextExternalAddressesForAccount(ctx, "a", 1)
	require.NoError(t, err)
	w, err = repo.GetWallet(ctx)
	require.NoError(t, err)
	_, err = w.GetAccount("b")
	require.NoError(t, err, "a write started from the older wallet and wrote it over account b")
}

// A read of the file in flight across a reset does not put the wallet back: after the reset
// there is none, and a new one can be made.
func TestAReadInFlightDoesNotPutBackTheWalletAfterAReset(t *testing.T) {
	ctx := context.Background()
	rm, repo, _ := storeWithWallet(t)
	held, letGo := holdFirstRead(repo)

	done := make(chan error, 1)
	go func() {
		_, err := repo.GetWallet(ctx)
		done <- err
	}()
	<-held

	rm.Reset()

	letGo()
	require.NoError(t, <-done, "the read that began before the reset is answered with what it read")

	_, err := repo.GetWallet(ctx)
	require.Error(t, err, "the read in flight put the wallet back after the reset")
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)), "a new wallet is refused: the old one was put back")
	w, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	require.Empty(t, w.Accounts)
}
