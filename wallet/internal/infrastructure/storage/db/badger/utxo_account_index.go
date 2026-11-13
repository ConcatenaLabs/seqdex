package dbbadger

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/dgraph-io/badger/v4"
	log "github.com/sirupsen/logrus"
	"github.com/timshannon/badgerhold/v4"
)

// The index of each account's unspent coins.
//
// An account's balance, spendable coins and locked coins are what the makers read, over and
// over. Read by a query on the account's name they decoded every coin in the store, spent
// ones included, so each read cost the whole store: on a store of millions of coins one read
// took minutes, and reads given up on by their callers piled up until the daemon stalled at
// its memory limit. The index keeps one key per unspent coin, under its account:
//
//	_acctUtxo:<account>\x00<txid>:<vout>  (empty value)
//
// so a read walks only the account's own unspent coins and fetches each by its key. The key
// is written or removed in the same transaction as the coin, by every write. A store written
// by a build without the index is indexed once, when it is opened, from one pass over its
// coins; the marker key is written after that pass, so a stop part-way means another pass.
//
// badgerhold's own index was not used: it keeps one value per account listing every key ever
// written under it, spent coins included, rewritten whole on each write, so the reads would
// still grow with the account's history. Nor the account in the coin's key: every coin is
// stored under its outpoint's hash, and moving them would be a rewrite of the store.
var (
	accountIndexPrefix = []byte("_acctUtxo:")
	accountIndexMarker = []byte("_acctUtxoIndex:v1")
)

// How often a read looks at its caller's context while it walks an account's coins.
const readCheckEvery = 256

func accountIndexPrefixOf(account string) []byte {
	b := make([]byte, 0, len(accountIndexPrefix)+len(account)+1)
	b = append(b, accountIndexPrefix...)
	b = append(b, account...)
	return append(b, 0)
}

func accountIndexKey(account string, key domain.UtxoKey) []byte {
	b := accountIndexPrefixOf(account)
	b = append(b, key.TxID...)
	b = append(b, ':')
	return strconv.AppendUint(b, uint64(key.VOut), 10)
}

// utxoKeyOfIndexKey is the coin an index key under `prefix` names.
func utxoKeyOfIndexKey(k, prefix []byte) (domain.UtxoKey, bool) {
	rest := k[len(prefix):]
	i := bytes.LastIndexByte(rest, ':')
	if i < 0 {
		return domain.UtxoKey{}, false
	}
	vout, err := strconv.ParseUint(string(rest[i+1:]), 10, 32)
	if err != nil {
		return domain.UtxoKey{}, false
	}
	return domain.UtxoKey{TxID: string(rest[:i]), VOut: uint32(vout)}, true
}

// indexUtxo writes or removes `utxo`'s index key as its state now is. `was` is the record
// it replaces, if any, whose key is removed when it named another account.
func indexUtxo(tx *badger.Txn, was, utxo *domain.Utxo) error {
	if was != nil && was.AccountName != utxo.AccountName {
		if err := tx.Delete(accountIndexKey(was.AccountName, was.Key())); err != nil {
			return err
		}
	}
	key := accountIndexKey(utxo.AccountName, utxo.Key())
	if utxo.IsSpent() {
		if was == nil || was.IsSpent() && was.AccountName == utxo.AccountName {
			return nil
		}
		return tx.Delete(key)
	}
	return tx.Set(key, []byte{})
}

// How many of the store's account reads may run at once. A read waits for a slot, and gives
// up waiting when its caller is gone, so no number of callers can run more reads than this.
var storeReadsParallel = 8

// SetStoreReadsParallel sets how many account reads may run at once, for every store opened
// afterwards.
func SetStoreReadsParallel(n int) {
	if n < 1 {
		n = 1
	}
	storeReadsParallel = n
}

// rebuildAccountIndex makes the next store opened index its coins again, from one pass.
var rebuildAccountIndex = false

// SetRebuildAccountIndex makes every store opened afterwards rebuild the index of its accounts'
// unspent coins. A build without the index writes coins without their index keys: after one
// has run on a store, the store's next start with this build sets it.
func SetRebuildAccountIndex(rebuild bool) {
	rebuildAccountIndex = rebuild
}

type readSlots chan struct{}

// take waits for a free read slot, or returns the caller's error once it has gone.
func (s readSlots) take(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s <- struct{}{}:
		return func() { <-s }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ensureAccountIndex indexes a store that has no index, or all of one when asked to: one pass
// over its coins, before anything reads or writes them.
func (r *utxoRepository) ensureAccountIndex(rebuild bool) error {
	db := r.store.Badger()
	if !rebuild {
		err := db.View(func(tx *badger.Txn) error {
			_, err := tx.Get(accountIndexMarker)
			return err
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
	}
	start := time.Now()
	log.Infof("utxo repository: indexing each account's unspent coins, once for this store")
	if err := db.DropPrefix(accountIndexMarker, accountIndexPrefix); err != nil {
		return err
	}
	batch := db.NewWriteBatch()
	defer batch.Cancel()
	var records, unspent int
	err := r.store.ForEach(nil, func(u *domain.Utxo) error {
		records++
		if records%1000000 == 0 {
			log.Infof("utxo repository: indexing: %d coins read, %d unspent, in %s",
				records, unspent, time.Since(start).Round(time.Second))
		}
		if u.IsSpent() {
			return nil
		}
		unspent++
		return batch.Set(accountIndexKey(u.AccountName, u.Key()), []byte{})
	})
	if err != nil {
		return err
	}
	if err := batch.Flush(); err != nil {
		return err
	}
	if err := db.Update(func(tx *badger.Txn) error {
		return tx.Set(accountIndexMarker, []byte(time.Now().UTC().Format(time.RFC3339)))
	}); err != nil {
		return err
	}
	took := time.Since(start)
	rate := 0.0
	if took > 0 {
		rate = float64(records) / took.Seconds()
	}
	log.Infof("utxo repository: indexed %d unspent coins of %d in the store in %s (%.0f coins/s)",
		unspent, records, took.Round(time.Millisecond), rate)
	return nil
}

// accountCoin is a coin as an account's reads need it: everything but its range and
// surjection proofs. Those are needed only to spend a coin, which reads it by its key, and they
// are most of a confidential coin (about 4.2 KB of its 4.5): decoded into this type they are
// skipped, not allocated. It is stored as a domain.Utxo, under that type's name.
type accountCoin struct {
	domain.UtxoKey
	Value               uint64
	Asset               string
	ValueCommitment     []byte
	AssetCommitment     []byte
	ValueBlinder        []byte
	AssetBlinder        []byte
	Script              []byte
	Nonce               []byte
	AccountName         string
	LockTimestamp       int64
	LockExpiryTimestamp int64
	SpentStatus         domain.UtxoStatus
	ConfirmedStatus     domain.UtxoStatus
}

// Type is the name domain.Utxo is stored under, so a read of an accountCoin finds the coin.
func (*accountCoin) Type() string { return "Utxo" }

// Indexes is never used: an accountCoin is only read, never written.
func (*accountCoin) Indexes() map[string]badgerhold.Index { return nil }

func (c *accountCoin) utxo() *domain.Utxo {
	return &domain.Utxo{
		UtxoKey: c.UtxoKey, Value: c.Value, Asset: c.Asset,
		ValueCommitment: c.ValueCommitment, AssetCommitment: c.AssetCommitment,
		ValueBlinder: c.ValueBlinder, AssetBlinder: c.AssetBlinder, Script: c.Script, Nonce: c.Nonce,
		AccountName: c.AccountName, LockTimestamp: c.LockTimestamp,
		LockExpiryTimestamp: c.LockExpiryTimestamp, SpentStatus: c.SpentStatus,
		ConfirmedStatus: c.ConfirmedStatus,
	}
}

// accountCoinAt is the coin of `key` without its proofs, or nil (see utxoAt).
func (r *utxoRepository) accountCoinAt(ctx context.Context, tx *badger.Txn, key domain.UtxoKey) (*domain.Utxo, error) {
	var c accountCoin
	err := r.store.TxGet(tx, key.Hash(), &c)
	if errors.Is(err, badgerhold.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.TxID == key.TxID && c.VOut == key.VOut {
		return c.utxo(), nil
	}
	// The hash holds another output's coin (256 apart in one transaction): the full read.
	u, err := r.utxoAt(context.WithValue(ctx, "tx", tx), key)
	if err != nil || u == nil {
		return nil, err
	}
	u.RangeProof, u.SurjectionProof = nil, nil
	return u, nil
}

// eachAccountCoin calls `fn` with each of `account`'s unspent coins, without their proofs,
// read from the index: the work is the account's own unspent coins, whatever else the store
// holds, and nothing is kept between two calls of `fn`. It stops with the caller's error once
// the caller has gone, and runs in one of the store's read slots.
func (r *utxoRepository) eachAccountCoin(
	ctx context.Context, account string, fn func(*domain.Utxo),
) error {
	done, err := r.reads.take(ctx)
	if err != nil {
		return err
	}
	defer done()

	read := func(tx *badger.Txn) error {
		prefix := accountIndexPrefixOf(account)
		keys := make([]domain.UtxoKey, 0)
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := tx.NewIterator(opts)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if len(keys)%readCheckEvery == 0 {
				if err := ctx.Err(); err != nil {
					it.Close()
					return err
				}
			}
			if key, ok := utxoKeyOfIndexKey(it.Item().Key(), prefix); ok {
				keys = append(keys, key)
			}
		}
		it.Close()

		for i, key := range keys {
			if i%readCheckEvery == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			u, err := r.accountCoinAt(ctx, tx, key)
			if err != nil {
				return err
			}
			// Every write keeps the index with its coin, so this skips nothing; it keeps a
			// read right should a key outlive its coin all the same.
			if u == nil || u.IsSpent() || u.AccountName != account {
				continue
			}
			fn(u)
		}
		return nil
	}

	if tx, ok := ctx.Value("tx").(*badger.Txn); ok {
		return read(tx)
	}
	return r.store.Badger().View(read)
}

// accountUtxos are `account`'s unspent coins that `keep` keeps, without their proofs.
func (r *utxoRepository) accountUtxos(
	ctx context.Context, account string, keep func(*domain.Utxo) bool,
) ([]*domain.Utxo, error) {
	var utxos []*domain.Utxo
	if err := r.eachAccountCoin(ctx, account, func(u *domain.Utxo) {
		if keep(u) {
			utxos = append(utxos, u)
		}
	}); err != nil {
		return nil, err
	}
	return utxos, nil
}

// write runs `fn` in the caller's transaction, or in one of its own, retried when it meets a
// concurrent write of the same keys.
func (r *utxoRepository) write(ctx context.Context, fn func(tx *badger.Txn) error) error {
	if tx, ok := ctx.Value("tx").(*badger.Txn); ok {
		return fn(tx)
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = r.store.Badger().Update(fn)
		if !errors.Is(err, badger.ErrConflict) {
			return err
		}
	}
	return err
}

func newReadSlots() readSlots {
	return make(readSlots, storeReadsParallel)
}
