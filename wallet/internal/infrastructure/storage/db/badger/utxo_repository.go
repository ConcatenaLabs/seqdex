package dbbadger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/dgraph-io/badger/v4"
	log "github.com/sirupsen/logrus"
	"github.com/timshannon/badgerhold/v4"
)

type utxoRepository struct {
	store            *badgerhold.Store
	chEvents         chan domain.UtxoEvent
	externalChEvents chan domain.UtxoEvent
	lock             *sync.Mutex
	reads            readSlots

	log func(format string, a ...interface{})
}

func NewUtxoRepository(store *badgerhold.Store) domain.UtxoRepository {
	return newUtxoRepository(store)
}

func newUtxoRepository(store *badgerhold.Store) *utxoRepository {
	chEvents := make(chan domain.UtxoEvent)
	externalChEvents := make(chan domain.UtxoEvent)
	lock := &sync.Mutex{}
	logFn := func(format string, a ...interface{}) {
		format = fmt.Sprintf("utxo repository: %s", format)
		log.Debugf(format, a...)
	}
	return &utxoRepository{store, chEvents, externalChEvents, lock, newReadSlots(), logFn}
}

func (r *utxoRepository) AddUtxos(
	ctx context.Context, utxos []*domain.Utxo,
) (int, error) {
	return r.addUtxos(ctx, utxos)
}

func (r *utxoRepository) GetUtxosByKey(
	ctx context.Context, utxoKeys []domain.UtxoKey,
) ([]*domain.Utxo, error) {
	utxos := make([]*domain.Utxo, 0, len(utxoKeys))
	for _, key := range utxoKeys {
		utxo, err := r.utxoAt(ctx, key)
		if err != nil {
			return nil, err
		}
		if utxo != nil {
			utxos = append(utxos, utxo)
		}
	}

	return utxos, nil
}

// GetAllUtxos, GetSpendableUtxos and GetAllUtxosForAccount read every coin in the store
// (the last, spent ones included, which only the store as a whole holds); nothing the
// daemon serves calls them. The reads of an account's unspent coins below use the index.

func (r *utxoRepository) GetAllUtxos(
	ctx context.Context,
) ([]*domain.Utxo, error) {
	return r.scanUtxos(ctx, nil)
}

func (r *utxoRepository) GetSpendableUtxos(
	ctx context.Context,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("SpentStatus").Eq(domain.UtxoStatus{}).
		And("LockTimestamp").Eq(int64(0))

	return r.scanUtxos(ctx, query)
}

func (r *utxoRepository) GetAllUtxosForAccount(
	ctx context.Context, accountName string,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("AccountName").Eq(accountName)

	return r.scanUtxos(ctx, query)
}

func (r *utxoRepository) GetSpendableUtxosForAccount(
	ctx context.Context, accountName string, scripts [][]byte,
) ([]*domain.Utxo, error) {
	return r.accountUtxos(ctx, accountName, func(u *domain.Utxo) bool {
		return !u.IsLocked() && hasScript(u, scripts)
	})
}

func (r *utxoRepository) GetLockedUtxosForAccount(
	ctx context.Context, accountName string, scripts [][]byte,
) ([]*domain.Utxo, error) {
	return r.accountUtxos(ctx, accountName, func(u *domain.Utxo) bool {
		return u.IsLocked() && hasScript(u, scripts)
	})
}

// hasScript is whether `u` pays one of `scripts`, or any script when none is given.
func hasScript(u *domain.Utxo, scripts [][]byte) bool {
	if len(scripts) <= 0 {
		return true
	}
	for _, script := range scripts {
		if bytes.Equal(u.Script, script) {
			return true
		}
	}
	return false
}

func (r *utxoRepository) GetBalanceForAccount(
	ctx context.Context, accountName string,
) (map[string]*domain.Balance, error) {
	// summed as the coins are read: a balance holds no list of them
	balance := make(map[string]*domain.Balance)
	if err := r.eachAccountCoin(ctx, accountName, func(u *domain.Utxo) {
		b, ok := balance[u.Asset]
		if !ok {
			b = &domain.Balance{}
			balance[u.Asset] = b
		}
		if u.IsLocked() {
			b.Locked += u.Value
		} else if u.IsConfirmed() {
			b.Confirmed += u.Value
		} else {
			b.Unconfirmed += u.Value
		}
	}); err != nil {
		return nil, err
	}
	return balance, nil
}

func (r *utxoRepository) SpendUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, txid string,
) (int, error) {
	return r.spendUtxos(ctx, utxoKeys, txid)
}

func (r *utxoRepository) ConfirmSpendUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, status domain.UtxoStatus,
) (int, error) {
	return r.confirmSpendUtxos(ctx, utxoKeys, status)
}

func (r *utxoRepository) ConfirmUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, status domain.UtxoStatus,
) (int, error) {
	return r.confirmUtxos(ctx, utxoKeys, status)
}

func (r *utxoRepository) LockUtxos(
	ctx context.Context,
	utxoKeys []domain.UtxoKey, timestamp, expiryTimestamp int64,
) (int, error) {
	return r.lockUtxos(ctx, utxoKeys, timestamp, expiryTimestamp)
}

func (r *utxoRepository) UnlockUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey,
) (int, error) {
	return r.unlockUtxos(ctx, utxoKeys)
}

// DeleteUtxosForAccount deletes every coin of the account, spent ones included, with its
// index keys, in one transaction.
func (r *utxoRepository) DeleteUtxosForAccount(
	ctx context.Context, accountName string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.write(ctx, func(tx *badger.Txn) error {
		query := badgerhold.Where("AccountName").Eq(accountName)
		if err := r.store.TxDeleteMatching(tx, &domain.Utxo{}, query); err != nil {
			return err
		}
		prefix := accountIndexPrefixOf(accountName)
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := tx.NewIterator(opts)
		var keys [][]byte
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		it.Close()
		for _, k := range keys {
			if err := tx.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *utxoRepository) GetEventChannel() chan domain.UtxoEvent {
	return r.externalChEvents
}

func (r *utxoRepository) addUtxos(
	ctx context.Context, utxos []*domain.Utxo,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, u := range utxos {
		done, err := r.insertUtxo(ctx, u)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, u.Info())
		}
	}

	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoAdded,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) spendUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, txid string,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, key := range utxoKeys {
		done, info, err := r.spendUtxo(ctx, key, txid)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, *info)
		}
	}
	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoSpent,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) confirmSpendUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, status domain.UtxoStatus,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, key := range utxoKeys {
		done, info, err := r.confirmSpendUtxo(ctx, key, status)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, *info)
		}
	}
	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoConfirmedSpend,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) confirmUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey, status domain.UtxoStatus,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, key := range utxoKeys {
		done, info, err := r.confirmUtxo(ctx, key, status)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, *info)
		}
	}

	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoConfirmed,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) lockUtxos(
	ctx context.Context,
	utxoKeys []domain.UtxoKey, timestamp, expiryTimestamp int64,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, key := range utxoKeys {
		done, info, err := r.lockUtxo(ctx, key, timestamp, expiryTimestamp)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, *info)
		}
	}

	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoLocked,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) unlockUtxos(
	ctx context.Context, utxoKeys []domain.UtxoKey,
) (int, error) {
	count := 0
	utxosInfo := make([]domain.UtxoInfo, 0)
	for _, key := range utxoKeys {
		done, info, err := r.unlockUtxo(ctx, key)
		if err != nil {
			return -1, err
		}
		if done {
			count++
			utxosInfo = append(utxosInfo, *info)
		}
	}

	if count > 0 {
		go r.publishEvent(domain.UtxoEvent{
			EventType: domain.UtxoUnlocked,
			Utxos:     utxosInfo,
		})
	}

	return count, nil
}

func (r *utxoRepository) spendUtxo(
	ctx context.Context, key domain.UtxoKey, txid string,
) (bool, *domain.UtxoInfo, error) {
	utxo, err := r.utxoAt(ctx, key)
	if err != nil {
		return false, nil, err
	}

	if utxo == nil {
		return false, nil, nil
	}

	if utxo.IsSpent() {
		return false, nil, nil
	}

	if err := utxo.Spend(txid); err != nil {
		return false, nil, err
	}
	if err := r.updateUtxo(ctx, utxo); err != nil {
		return false, nil, err
	}

	utxoInfo := utxo.Info()
	return true, &utxoInfo, nil
}

func (r *utxoRepository) confirmSpendUtxo(
	ctx context.Context, key domain.UtxoKey, status domain.UtxoStatus,
) (bool, *domain.UtxoInfo, error) {
	utxo, err := r.utxoAt(ctx, key)
	if err != nil {
		return false, nil, err
	}

	if utxo == nil {
		return false, nil, nil
	}

	if utxo.IsConfirmedSpent() {
		return false, nil, nil
	}

	if err := utxo.ConfirmSpend(status); err != nil {
		return false, nil, err
	}
	if err := r.updateUtxo(ctx, utxo); err != nil {
		return false, nil, err
	}

	utxoInfo := utxo.Info()
	return true, &utxoInfo, nil
}

func (r *utxoRepository) confirmUtxo(
	ctx context.Context, key domain.UtxoKey, status domain.UtxoStatus,
) (bool, *domain.UtxoInfo, error) {
	utxo, err := r.utxoAt(ctx, key)
	if err != nil {
		return false, nil, err
	}

	if utxo == nil {
		return false, nil, nil
	}

	if utxo.IsConfirmed() {
		return false, nil, nil
	}

	if err := utxo.Confirm(status); err != nil {
		return false, nil, err
	}
	if err := r.updateUtxo(ctx, utxo); err != nil {
		return false, nil, err
	}

	utxoInfo := utxo.Info()
	return true, &utxoInfo, nil
}

func (r *utxoRepository) lockUtxo(
	ctx context.Context, key domain.UtxoKey, timestamp, expiryTimestamp int64,
) (bool, *domain.UtxoInfo, error) {
	utxo, err := r.utxoAt(ctx, key)
	if err != nil {
		return false, nil, err
	}

	if utxo == nil {
		return false, nil, nil
	}

	if utxo.IsLocked() {
		return false, nil, nil
	}

	utxo.Lock(timestamp, expiryTimestamp)
	if err := r.updateUtxo(ctx, utxo); err != nil {
		return false, nil, err
	}

	utxoInfo := utxo.Info()
	return true, &utxoInfo, nil
}

func (r *utxoRepository) unlockUtxo(
	ctx context.Context, key domain.UtxoKey,
) (bool, *domain.UtxoInfo, error) {
	utxo, err := r.utxoAt(ctx, key)
	if err != nil {
		return false, nil, err
	}

	if utxo == nil {
		return false, nil, nil
	}

	if !utxo.IsLocked() {
		return false, nil, nil
	}

	utxo.Unlock()
	if err := r.updateUtxo(ctx, utxo); err != nil {
		return false, nil, err
	}

	utxoInfo := utxo.Info()
	return true, &utxoInfo, nil
}

// scanUtxos runs a query over the whole store, in a read slot, and stops once its caller has
// gone.
func (r *utxoRepository) scanUtxos(
	ctx context.Context, query *badgerhold.Query,
) ([]*domain.Utxo, error) {
	done, err := r.reads.take(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	stopped := func(*badgerhold.RecordAccess) (bool, error) {
		return true, ctx.Err()
	}
	if query == nil {
		return r.findUtxos(ctx, badgerhold.Where("TxID").MatchFunc(stopped))
	}
	return r.findUtxos(ctx, query.And("TxID").MatchFunc(stopped))
}

func (r *utxoRepository) findUtxos(
	ctx context.Context, query *badgerhold.Query,
) ([]*domain.Utxo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var list []domain.Utxo
	var utxos []*domain.Utxo
	var err error
	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		err = r.store.TxFind(tx, &utxos, query)
	} else {
		err = r.store.Find(&utxos, query)
	}
	if err != nil {
		if err == badgerhold.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}

	for i := range list {
		u := &list[i]
		utxos = append(utxos, u)
	}
	return utxos, nil
}

// utxoAt is the UTXO of `key`, read under the key it is stored at (its key's hash), or nil.
// A query matching TxID and VOut decodes every UTXO in the store to compare them, and the
// scanner confirms each UTXO it finds, and each spend it sees, that way: an account catching
// up made a whole-store scan per output, and kept two cores busy without catching up. Every
// UTXO is inserted under its key's hash, so nothing there means no such UTXO. The hash keeps
// one byte of the output index: only a record there for another output (256 apart in one
// transaction) sends the lookup back to the query.
func (r *utxoRepository) utxoAt(ctx context.Context, key domain.UtxoKey) (*domain.Utxo, error) {
	utxo, held, err := r.storedAt(ctx, key)
	if err != nil || utxo != nil || !held {
		return utxo, err
	}
	utxos, err := r.findUtxos(ctx, badgerhold.Where("TxID").Eq(key.TxID).And("VOut").Eq(key.VOut))
	if err != nil || len(utxos) == 0 {
		return nil, err
	}
	return utxos[0], nil
}

// storedAt reads the record under `key`'s hash: the UTXO when it is `key`'s; nil with `held`
// when the hash holds another output's; nil and not held when it holds nothing.
func (r *utxoRepository) storedAt(ctx context.Context, key domain.UtxoKey) (*domain.Utxo, bool, error) {
	var utxo domain.Utxo
	var err error
	if tx, ok := ctx.Value("tx").(*badger.Txn); ok {
		err = r.store.TxGet(tx, key.Hash(), &utxo)
	} else {
		err = r.store.Get(key.Hash(), &utxo)
	}
	if errors.Is(err, badgerhold.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if utxo.TxID != key.TxID || utxo.VOut != key.VOut {
		return nil, true, nil
	}
	return &utxo, true, nil
}

// updateUtxo replaces the stored record of `utxo` and its index key, in one transaction.
func (r *utxoRepository) updateUtxo(
	ctx context.Context, utxo *domain.Utxo,
) error {
	key := utxo.Key()
	return r.write(ctx, func(tx *badger.Txn) error {
		txCtx := context.WithValue(ctx, "tx", tx)
		// Replaced under its key's hash, where it is stored, unless that holds another output's.
		found, held, err := r.storedAt(txCtx, key)
		if err != nil {
			return err
		}
		if found != nil {
			if err := r.store.TxUpdate(tx, key.Hash(), *utxo); err != nil {
				return err
			}
			return indexUtxo(tx, found, utxo)
		}
		if !held {
			return nil
		}
		var was *domain.Utxo
		query := badgerhold.Where("TxID").Eq(key.TxID).And("VOut").Eq(key.VOut)
		if err := r.store.TxUpdateMatching(
			tx, domain.Utxo{}, query, func(record interface{}) error {
				u := record.(*domain.Utxo)
				prev := *u
				was = &prev
				*u = *utxo
				return nil
			},
		); err != nil {
			return err
		}
		if was == nil {
			return nil
		}
		return indexUtxo(tx, was, utxo)
	})
}

func (r *utxoRepository) insertUtxo(
	ctx context.Context, utxo *domain.Utxo,
) (bool, error) {
	err := r.write(ctx, func(tx *badger.Txn) error {
		if err := r.store.TxInsert(tx, utxo.Key().Hash(), *utxo); err != nil {
			return err
		}
		return indexUtxo(tx, nil, utxo)
	})
	if err != nil {
		if errors.Is(err, badgerhold.ErrKeyExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *utxoRepository) publishEvent(event domain.UtxoEvent) {
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

func (r *utxoRepository) reset() {
	r.store.Badger().DropAll()
}

func (r *utxoRepository) close() {
	r.store.Close()
	close(r.chEvents)
	close(r.externalChEvents)
}
