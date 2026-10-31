package dbbadger_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
	"github.com/aejkcs50/seqdex/wallet/pkg/seqnet"
	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
	"github.com/timshannon/badgerhold/v4"
)

func newTestWallet(t *testing.T) *domain.Wallet {
	domain.MnemonicCypher = plainCypher{}
	domain.MnemonicStore = &memMnemonic{}
	mnemonic := strings.Split("leave dice fine decrease dune ribbon ocean earn lunar account silver admit cheap fringe disorder trade because trade steak clock grace video jacket equal", " ")
	w, err := domain.NewWallet(mnemonic, "password", "m/84'/1'", seqnet.Regtest, 1, nil)
	require.NoError(t, err)
	return w
}

// On disk the wallet lives in its own file: an unlock, which changes nothing stored, writes
// nothing, and a change replaces the file and reads back after a reopen.
func TestTheWalletIsAFileAndAnUnlockWritesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rm, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	repo := rm.WalletRepository()
	require.NoError(t, repo.CreateWallet(ctx, newTestWallet(t)))
	require.NoError(t, repo.UnlockWallet(ctx, "password"))
	_, err = repo.CreateAccount(ctx, "acc", 1, false)
	require.NoError(t, err)

	state := filepath.Join(dir, "wallet.state")
	before, err := os.Stat(state)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 50; i++ {
		require.NoError(t, repo.UnlockWallet(ctx, "password"))
	}
	after, err := os.Stat(state)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "an unlock rewrote the wallet")

	_, err = repo.DeriveNextExternalAddressesForAccount(ctx, "acc", 3)
	require.NoError(t, err)
	changed, err := os.Stat(state)
	require.NoError(t, err)
	require.NotEqual(t, before.ModTime(), changed.ModTime(), "a derivation is written")
	rm.Close()

	rm2, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	defer rm2.Close()
	w, err := rm2.WalletRepository().GetWallet(ctx)
	require.NoError(t, err)
	acc, err := w.GetAccount("acc")
	require.NoError(t, err)
	require.Equal(t, uint(3), acc.NextExternalIndex)
	require.Error(t, rm2.WalletRepository().CreateWallet(ctx, newTestWallet(t)), "a second wallet is refused")
}

// A store from before the file holds the wallet in badger: it is read from there, and its first
// change moves it into the file.
func TestAWalletInTheStoreMovesToTheFileOnItsFirstChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := newTestWallet(t)
	db, err := badgerhold.Open(badgerhold.Options{Encoder: badgerhold.DefaultEncode, Decoder: badgerhold.DefaultDecode,
		Options: badger.DefaultOptions(filepath.Join(dir, "wallet")).WithLogger(nil)})
	require.NoError(t, err)
	require.NoError(t, db.Insert("wallet", *w))
	require.NoError(t, db.Close())

	rm, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	repo := rm.WalletRepository()
	got, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	require.Equal(t, w.RootPath, got.RootPath)
	_, err = os.Stat(filepath.Join(dir, "wallet.state"))
	require.True(t, os.IsNotExist(err), "not moved before a change")
	require.NoError(t, repo.UnlockWallet(ctx, "password"))
	_, err = repo.CreateAccount(ctx, "moved", 1, false)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "wallet.state"))
	require.NoError(t, err, "moved by its first change")
	rm.Close()

	rm2, err := dbbadger.NewRepoManager(dir, nil)
	require.NoError(t, err)
	defer rm2.Close()
	got, err = rm2.WalletRepository().GetWallet(ctx)
	require.NoError(t, err)
	_, err = got.GetAccount("moved")
	require.NoError(t, err)
}
