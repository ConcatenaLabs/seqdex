package dbbadger_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/aejkcs50/seqdex/wallet/pkg/seqnet"
	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	dbbadger "github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/badger"
)

type plainCypher struct{}

func (plainCypher) Encrypt(m, _ []byte) ([]byte, error) { return m, nil }
func (plainCypher) Decrypt(m, _ []byte) ([]byte, error) { return m, nil }

type memMnemonic struct {
	sync.RWMutex
	m []string
}

func (s *memMnemonic) Set(m string)   { s.Lock(); s.m = strings.Split(m, " "); s.Unlock() }
func (s *memMnemonic) Unset()         { s.Lock(); s.m = nil; s.Unlock() }
func (s *memMnemonic) IsSet() bool    { s.RLock(); defer s.RUnlock(); return len(s.m) > 0 }
func (s *memMnemonic) Get() []string  { s.RLock(); defer s.RUnlock(); return s.m }

// Accounts created while other writers keep deriving addresses (as a restarted Ocean's makers
// and scanner do) are all kept: each write of the wallet starts from the one before it.
func TestAccountsCreatedDuringOtherWritesAreKept(t *testing.T) {
	domain.MnemonicCypher = plainCypher{}
	domain.MnemonicStore = &memMnemonic{}
	ctx := context.Background()
	rm, err := dbbadger.NewRepoManager("", nil)
	require.NoError(t, err)
	repo := rm.WalletRepository()
	mnemonic := strings.Split("leave dice fine decrease dune ribbon ocean earn lunar account silver admit cheap fringe disorder trade because trade steak clock grace video jacket equal", " ")
	w, err := domain.NewWallet(mnemonic, "password", "m/84'/1'", seqnet.Regtest, 1, nil)
	require.NoError(t, err)
	require.NoError(t, repo.CreateWallet(ctx, w))
	require.NoError(t, repo.UnlockWallet(ctx, "password"))
	_, err = repo.CreateAccount(ctx, "busy", 1, false)
	require.NoError(t, err)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = repo.DeriveNextExternalAddressesForAccount(ctx, "busy", 1)
			}
		}()
	}
	const n = 40
	for i := 0; i < n; i++ {
		_, err := repo.CreateAccount(ctx, fmt.Sprintf("lq-sc-%d", i), 1, false)
		require.NoError(t, err)
	}
	close(stop)
	wg.Wait()

	got, err := repo.GetWallet(ctx)
	require.NoError(t, err)
	missing := []string{}
	for i := 0; i < n; i++ {
		if _, err := got.GetAccount(fmt.Sprintf("lq-sc-%d", i)); err != nil {
			missing = append(missing, fmt.Sprintf("lq-sc-%d (%v)", i, err))
		}
	}
	require.Empty(t, missing, "accounts lost to a concurrent write")
}
