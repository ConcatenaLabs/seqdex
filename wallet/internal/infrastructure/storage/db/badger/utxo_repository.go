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
	return &utxoRepository{store, chEvents, externalChEvents, lock, logFn}
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

func (r *utxoRepository) GetAllUtxos(
	ctx context.Context,
) ([]*domain.Utxo, error) {
	return r.getAllUtxos(ctx)
}

func (r *utxoRepository) GetSpendableUtxos(
	ctx context.Context,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("SpentStatus").Eq(domain.UtxoStatus{}).
		And("LockTimestamp").Eq(int64(0))

	return r.findUtxos(ctx, query)
}

func (r *utxoRepository) GetAllUtxosForAccount(
	ctx context.Context, accountName string,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("AccountName").Eq(accountName)

	return r.findUtxos(ctx, query)
}

func (r *utxoRepository) GetSpendableUtxosForAccount(
	ctx context.Context, accountName string, scripts [][]byte,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("SpentStatus").Eq(domain.UtxoStatus{}).
		And("LockTimestamp").Eq(int64(0)).And("AccountName").Eq(accountName)

	utxos, err := r.findUtxos(ctx, query)
	if err != nil {
		return nil, err
	}

	if len(scripts) <= 0 {
		return utxos, nil
	}

	filteredUtxos := make([]*domain.Utxo, 0, len(utxos))
	for _, u := range utxos {
		for _, script := range scripts {
			if bytes.Equal(u.Script, script) {
				filteredUtxos = append(filteredUtxos, u)
				break
			}
		}
	}
	return filteredUtxos, nil
}

func (r *utxoRepository) GetLockedUtxosForAccount(
	ctx context.Context, accountName string, scripts [][]byte,
) ([]*domain.Utxo, error) {
	query := badgerhold.Where("SpentStatus").Eq(domain.UtxoStatus{}).
		And("LockTimestamp").Gt(int64(0)).And("AccountName").Eq(accountName)

	utxos, err := r.findUtxos(ctx, query)
	if err != nil {
		return nil, err
	}

	if len(scripts) <= 0 {
		return utxos, nil
	}

	filteredUtxos := make([]*domain.Utxo, 0, len(utxos))
	for _, u := range utxos {
		for _, script := range scripts {
			if bytes.Equal(u.Script, script) {
				filteredUtxos = append(filteredUtxos, u)
				break
			}
		}
	}
	return filteredUtxos, nil
}

func (r *utxoRepository) GetBalanceForAccount(
	ctx context.Context, accountName string,
) (map[string]*domain.Balance, error) {
	utxos, err := r.GetAllUtxosForAccount(ctx, accountName)
	if err != nil {
		return nil, err
	}

	balance := make(map[string]*domain.Balance)
	for _, u := range utxos {
		if u.IsSpent() {
			continue
		}

		if _, ok := balance[u.Asset]; !ok {
			balance[u.Asset] = &domain.Balance{}
		}

		b := balance[u.Asset]
		if u.IsLocked() {
			b.Locked += u.Value
		} else {
			if u.IsConfirmed() {
				b.Confirmed += u.Value
			} else {
				b.Unconfirmed += u.Value
			}
		}
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

func (r *utxoRepository) DeleteUtxosForAccount(
	ctx context.Context, accountName string,
) error {
	query := badgerhold.Where("AccountName").Eq(accountName)

	utxos, err := r.findUtxos(ctx, query)
	if err != nil {
		return err
	}

	utxoKeys := make([]domain.UtxoKey, 0, len(utxos))
	for _, u := range utxos {
		utxoKeys = append(utxoKeys, u.Key())
	}
	return r.deleteUtxos(ctx, utxoKeys)
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

func (r *utxoRepository) getAllUtxos(ctx context.Context) ([]*domain.Utxo, error) {
	return r.findUtxos(ctx, nil)
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

func (r *utxoRepository) findUtxos(
	ctx context.Context, query *badgerhold.Query,
) ([]*domain.Utxo, error) {
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
// scanner confirms each UTXO it finds that way: an account catching up confirmed a block's
// UTXOs one whole-store scan each, and kept two cores busy without catching up. The hash keeps
// one byte of the output index, so a record found there for another output (256 apart in one
// transaction) sends the lookup back to the query.
func (r *utxoRepository) utxoAt(ctx context.Context, key domain.UtxoKey) (*domain.Utxo, error) {
	utxo, found, err := r.storedAt(ctx, key)
	if err != nil || found {
		return utxo, err
	}
	utxos, err := r.findUtxos(ctx, badgerhold.Where("TxID").Eq(key.TxID).And("VOut").Eq(key.VOut))
	if err != nil || len(utxos) == 0 {
		return nil, err
	}
	return utxos[0], nil
}

// storedAt reads the record under `key`'s hash: found when it is `key`'s UTXO; not found, with
// a nil UTXO, when the hash holds nothing or another output's.
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
		return nil, false, nil
	}
	return &utxo, true, nil
}

func (r *utxoRepository) updateUtxo(
	ctx context.Context, utxo *domain.Utxo,
) error {
	key := utxo.Key()
	// Replaced under its key's hash, where it is stored, unless that holds another output's.
	if _, found, err := r.storedAt(ctx, key); err != nil {
		return err
	} else if found {
		if tx, ok := ctx.Value("tx").(*badger.Txn); ok {
			return r.store.TxUpdate(tx, key.Hash(), *utxo)
		}
		return r.store.Update(key.Hash(), *utxo)
	}
	query := badgerhold.Where("TxID").Eq(key.TxID).And("VOut").Eq(key.VOut)
	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		return r.store.TxUpdateMatching(
			tx, domain.Utxo{}, query, func(record interface{}) error {
				u := record.(*domain.Utxo)
				*u = *utxo
				return nil
			},
		)
	}

	return r.store.UpdateMatching(domain.Utxo{}, query, func(record interface{}) error {
		u := record.(*domain.Utxo)
		*u = *utxo
		return nil
	})
}

func (r *utxoRepository) insertUtxo(
	ctx context.Context, utxo *domain.Utxo,
) (bool, error) {
	var err error
	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		err = r.store.TxInsert(tx, utxo.Key().Hash(), *utxo)
	} else {
		err = r.store.Insert(utxo.Key().Hash(), *utxo)
	}
	if err != nil {
		if err == badgerhold.ErrKeyExists {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *utxoRepository) deleteUtxos(
	ctx context.Context, keys []domain.UtxoKey,
) error {
	query := &badgerhold.Query{}
	for _, key := range keys {
		qq := badgerhold.Where("TxID").Eq(key.TxID).And("VOut").Eq(key.VOut)
		query = query.Or(qq)
	}

	if ctx.Value("tx") != nil {
		tx := ctx.Value("tx").(*badger.Txn)
		return r.store.TxDeleteMatching(tx, &domain.Utxo{}, query)
	}

	return r.store.DeleteMatching(&domain.Utxo{}, query)
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
