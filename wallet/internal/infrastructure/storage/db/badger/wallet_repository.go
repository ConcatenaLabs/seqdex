package dbbadger

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/dgraph-io/badger/v4"
	log "github.com/sirupsen/logrus"
	"github.com/timshannon/badgerhold/v4"
)

const (
	//since there can be only 1 wallet in database,
	//key is hardcoded for easier retrival
	walletKey = "wallet"
)

type walletRepository struct {
	store *badgerhold.Store
	// stateFile holds the wallet when the store is on disk. The wallet is one value of a few
	// megabytes (every account and every address it derived), and badger keeps each version of
	// it in its value log until its memtable flushes, which pointers to it almost never fill:
	// rewritten every few seconds, it grew a store to 905 GB. In a file of its own it is replaced
	// in place. A store whose wallet is only in badger is read from there until its first change.
	stateFile        string
	chEvents         chan domain.WalletEvent
	externalChEvents chan domain.WalletEvent
	lock             *sync.Mutex
	// One write of the wallet at a time: each reads the stored wallet, changes it and stores
	// it back, so two at once would each store their own change over the other's.
	writeLock *sync.Mutex

	log func(format string, a ...interface{})
}

func NewWalletRepository(store *badgerhold.Store) domain.WalletRepository {
	return newWalletRepository(store, "")
}

func newWalletRepository(store *badgerhold.Store, stateFile string) *walletRepository {
	chEvents := make(chan domain.WalletEvent, 10)
	extrernalChEvents := make(chan domain.WalletEvent, 10)
	lock := &sync.Mutex{}
	logFn := func(format string, a ...interface{}) {
		format = fmt.Sprintf("wallet repository: %s", format)
		log.Debugf(format, a...)
	}
	return &walletRepository{
		store: store, stateFile: stateFile, chEvents: chEvents, externalChEvents: extrernalChEvents, lock: lock,
		writeLock: &sync.Mutex{}, log: logFn,
	}
}

func (r *walletRepository) CreateWallet(
	ctx context.Context, wallet *domain.Wallet,
) error {
	if err := r.insertWallet(ctx, wallet); err != nil {
		return err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType: domain.WalletCreated,
	})

	return nil
}

func (r *walletRepository) GetWallet(
	ctx context.Context,
) (*domain.Wallet, error) {
	return r.getWallet(ctx)
}

func (r *walletRepository) UnlockWallet(
	ctx context.Context, password string,
) error {
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			if err := w.Unlock(password); err != nil {
				return nil, err
			}
			return w, nil
		},
	); err != nil {
		return err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType: domain.WalletUnlocked,
	})

	return nil
}

func (r *walletRepository) LockWallet(
	ctx context.Context, password string,
) error {
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			if err := w.Lock(password); err != nil {
				return nil, err
			}
			return w, nil
		},
	); err != nil {
		return err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType: domain.WalletLocked,
	})

	return nil
}

func (r *walletRepository) UpdateWallet(
	ctx context.Context, updateFn func(v *domain.Wallet) (*domain.Wallet, error),
) error {
	r.writeLock.Lock()
	defer r.writeLock.Unlock()

	before, err := r.getWallet(ctx)
	if err != nil {
		return err
	}
	wallet, err := r.getWallet(ctx)
	if err != nil {
		return err
	}

	updatedWallet, err := updateFn(wallet)
	if err != nil {
		return err
	}

	// A change that changes nothing stored (an unlock, a lock, a label set to itself) is not
	// written: a client unlocking on every call rewrote the whole wallet every few seconds.
	if reflect.DeepEqual(before, updatedWallet) {
		return nil
	}
	return r.updateWallet(ctx, updatedWallet)
}

func (r *walletRepository) CreateAccount(
	ctx context.Context, accountName string, birthdayBlock uint32, unconf bool,
) (*domain.AccountInfo, error) {
	var accountInfo *domain.AccountInfo
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			account, err := w.CreateAccount(accountName, birthdayBlock, unconf)
			if err != nil {
				return nil, err
			}
			if account == nil {
				return nil, fmt.Errorf("account %s already existing", accountName)
			}
			accountInfo = &account.AccountInfo
			return w, nil
		},
	); err != nil {
		return nil, err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType:            domain.WalletAccountCreated,
		AccountName:          accountInfo.Namespace,
		AccountBirthdayBlock: birthdayBlock,
	})

	return accountInfo, nil
}

func (r *walletRepository) DeriveNextExternalAddressesForAccount(
	ctx context.Context, accountName string, numOfAddress uint64,
) ([]domain.AddressInfo, error) {
	if numOfAddress == 0 {
		numOfAddress = 1
	}

	var addressesInfo []domain.AddressInfo
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			for i := 0; i < int(numOfAddress); i++ {
				addrInfo, err := w.DeriveNextExternalAddressForAccount(accountName)
				if err != nil {
					return nil, err
				}
				addressesInfo = append(addressesInfo, *addrInfo)
			}
			return w, nil
		},
	); err != nil {
		return nil, err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType:        domain.WalletAccountAddressesDerived,
		AccountName:      addressesInfo[0].Account,
		AccountAddresses: addressesInfo,
	})

	return addressesInfo, nil
}

func (r *walletRepository) DeriveNextInternalAddressesForAccount(
	ctx context.Context, accountName string, numOfAddress uint64,
) ([]domain.AddressInfo, error) {
	if numOfAddress == 0 {
		numOfAddress = 1
	}

	var addressesInfo []domain.AddressInfo
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			for i := 0; i < int(numOfAddress); i++ {
				addrInfo, err := w.DeriveNextInternalAddressForAccount(accountName)
				if err != nil {
					return nil, err
				}
				addressesInfo = append(addressesInfo, *addrInfo)
			}
			return w, nil
		},
	); err != nil {
		return nil, err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType:        domain.WalletAccountAddressesDerived,
		AccountName:      addressesInfo[0].Account,
		AccountAddresses: addressesInfo,
	})

	return addressesInfo, nil
}

func (r *walletRepository) DeleteAccount(
	ctx context.Context, accountName string,
) error {
	var namespace string
	if err := r.UpdateWallet(
		ctx, func(w *domain.Wallet) (*domain.Wallet, error) {
			account, err := w.GetAccount(accountName)
			if err != nil {
				return nil, err
			}
			namespace = account.Namespace
			if err := w.DeleteAccount(accountName); err != nil {
				return nil, err
			}
			return w, nil
		},
	); err != nil {
		return err
	}

	go r.publishEvent(domain.WalletEvent{
		EventType:   domain.WalletAccountDeleted,
		AccountName: namespace,
	})

	return nil
}

func (r *walletRepository) GetEventChannel() chan domain.WalletEvent {
	return r.externalChEvents
}

func (r *walletRepository) insertWallet(
	ctx context.Context, wallet *domain.Wallet,
) error {
	var err error

	if r.stateFile != "" && ctx.Value("tx") == nil {
		if _, err := r.getWallet(ctx); err == nil {
			return fmt.Errorf("wallet is already initialized")
		}
		return r.writeState(wallet)
	}

	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		err = r.store.TxInsert(tx, walletKey, *wallet)
	} else {
		err = r.store.Insert(walletKey, *wallet)
	}
	if err != nil {
		if err == badgerhold.ErrKeyExists {
			return fmt.Errorf("wallet is already initialized")
		}
		return err
	}

	return nil
}

func (r *walletRepository) getWallet(ctx context.Context) (*domain.Wallet, error) {
	var err error
	var wallet domain.Wallet

	if r.stateFile != "" && ctx.Value("tx") == nil {
		b, err := os.ReadFile(r.stateFile)
		if err == nil {
			if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&wallet); err != nil {
				return nil, fmt.Errorf("reading %s: %w", r.stateFile, err)
			}
			return &wallet, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// Not moved out of the store yet: read from there.
	}

	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		err = r.store.TxGet(tx, walletKey, &wallet)
	} else {
		err = r.store.Get(walletKey, &wallet)
	}

	if err != nil {
		if err == badgerhold.ErrNotFound {
			return nil, fmt.Errorf("wallet is not initialized")
		}
		return nil, err
	}

	return &wallet, nil
}

func (r *walletRepository) updateWallet(
	ctx context.Context, wallet *domain.Wallet,
) error {
	if r.stateFile != "" && ctx.Value("tx") == nil {
		return r.writeState(wallet)
	}
	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		return r.store.TxUpdate(tx, walletKey, *wallet)
	}
	return r.store.Update(walletKey, *wallet)
}

// writeState replaces the wallet's file: written beside it, synced, then renamed over it, so a
// crash leaves the old wallet or the new one, never a torn one.
func (r *walletRepository) writeState(wallet *domain.Wallet) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(wallet); err != nil {
		return err
	}
	tmp := r.stateFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.stateFile); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(r.stateFile)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (r *walletRepository) publishEvent(event domain.WalletEvent) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.log("publish event %s", event.EventType)
	r.chEvents <- event

	// send over channel without blocking in case nobody is listening.
	select {
	case r.externalChEvents <- event:
	default:
	}
}

func (r *walletRepository) reset() {
	r.store.Badger().DropAll()
	if r.stateFile != "" {
		_ = os.Remove(r.stateFile)
	}
}

func (r *walletRepository) close() {
	r.store.Close()
	close(r.chEvents)
	close(r.externalChEvents)
}
