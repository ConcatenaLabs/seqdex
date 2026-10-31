package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

// A store whose one large value was rewritten many times, as the wallet's
// state is, copies into a store of about that value's size, with every live key
// and its latest value, and without a key deleted before.
func TestCompactKeepsOnlyLatestValues(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	db, err := badger.Open(badger.DefaultOptions(src).WithLogger(nil).WithCompression(options.ZSTD).WithValueThreshold(1024))
	if err != nil {
		t.Fatal(err)
	}
	last := make([]byte, 256<<10)
	for i := 0; i < 300; i++ {
		v := make([]byte, 256<<10)
		_, _ = rand.Read(v)
		last = v
		if err := db.Update(func(txn *badger.Txn) error { return txn.Set([]byte("wallet"), v) }); err != nil {
			t.Fatal(err)
		}
	}
	must(t, db.Update(func(txn *badger.Txn) error {
		return txn.SetEntry(badger.NewEntry([]byte("utxo"), []byte("small")).WithMeta(7))
	}))
	must(t, db.Update(func(txn *badger.Txn) error { return txn.Set([]byte("gone"), []byte("x")) }))
	must(t, db.Update(func(txn *badger.Txn) error { return txn.Delete([]byte("gone")) }))
	must(t, db.Update(func(txn *badger.Txn) error {
		return txn.SetEntry(badger.NewEntry([]byte("ttl"), []byte("y")).WithTTL(time.Hour))
	}))
	must(t, db.Close())

	if err := run(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := run(src, dst); err == nil {
		t.Fatal("a destination that exists must be refused")
	}
	// Measured before the copy is opened again: an open store preallocates its next value log.
	in, outSize := dirSize(src), dirSize(dst)
	t.Logf("source %d bytes, copy %d bytes", in, outSize)
	if outSize*10 > in {
		t.Errorf("the copy (%d bytes) is not much smaller than the source (%d bytes)", outSize, in)
	}
	out, err := badger.Open(badger.DefaultOptions(dst).WithLogger(nil).WithCompression(options.ZSTD))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	must(t, out.View(func(txn *badger.Txn) error {
		w, err := txn.Get([]byte("wallet"))
		if err != nil {
			return err
		}
		v, _ := w.ValueCopy(nil)
		if !bytes.Equal(v, last) {
			t.Error("the wallet's value is not its latest")
		}
		u, err := txn.Get([]byte("utxo"))
		if err != nil || u.UserMeta() != 7 {
			t.Errorf("utxo: %v, meta %d", err, u.UserMeta())
		}
		if _, err := txn.Get([]byte("gone")); err != badger.ErrKeyNotFound {
			t.Errorf("a deleted key came back: %v", err)
		}
		tt, err := txn.Get([]byte("ttl"))
		if err != nil || tt.ExpiresAt() == 0 {
			t.Errorf("ttl: %v, expires %d", err, tt.ExpiresAt())
		}
		return nil
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func dirSize(dir string) int64 {
	var n int64
	m, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range m {
		if fi, err := statSize(f); err == nil {
			n += fi
		}
	}
	return n
}

func statSize(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
