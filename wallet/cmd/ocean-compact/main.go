// Command ocean-compact copies an Ocean badger store into a new directory,
// keeping only the latest value of each key: a store whose value log grew past
// what its collector reclaims (the wallet's whole state is one value, rewritten
// on every change) comes back to the size of what it holds. The daemon must be
// stopped. The source is only read; the destination must not exist.
//
//	ocean-compact -src <store dir> -dst <new dir>
//
// It prints the keys copied and both sizes, and fails unless every key reads
// back from the destination with the same value.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

func main() {
	src := flag.String("src", "", "the store to compact (read only)")
	dst := flag.String("dst", "", "the new store (must not exist)")
	flag.Parse()
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: ocean-compact -src <store dir> -dst <new dir>")
		os.Exit(2)
	}
	if err := run(*src, *dst); err != nil {
		fmt.Fprintln(os.Stderr, "ocean-compact:", err)
		os.Exit(1)
	}
}

func run(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s exists: name a new directory", dst)
	}
	in, err := badger.Open(badger.DefaultOptions(src).WithLogger(nil).WithCompression(options.ZSTD))
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	out, err := badger.Open(badger.DefaultOptions(dst).WithLogger(nil).WithCompression(options.ZSTD))
	if err != nil {
		return fmt.Errorf("opening %s: %w", dst, err)
	}
	copied, err := copyLatest(in, out)
	if err != nil {
		out.Close()
		return err
	}
	if err := verify(in, out); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	fmt.Printf("copied %d keys; %s holds %s, %s holds %s\n", copied, src, size(src), dst, size(dst))
	return nil
}

// copyLatest writes the latest live version of every key of in into out, with
// its user meta and expiry.
func copyLatest(in, out *badger.DB) (int, error) {
	wb := out.NewWriteBatch()
	n := 0
	err := in.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			v, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("reading %x: %w", item.Key(), err)
			}
			e := badger.NewEntry(item.KeyCopy(nil), v).WithMeta(item.UserMeta())
			if exp := item.ExpiresAt(); exp > 0 {
				e.ExpiresAt = exp
			}
			if err := wb.SetEntry(e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		wb.Cancel()
		return 0, err
	}
	return n, wb.Flush()
}

// verify reads every key of in back from out.
func verify(in, out *badger.DB) error {
	return in.View(func(src *badger.Txn) error {
		return out.View(func(dst *badger.Txn) error {
			it := src.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			for it.Rewind(); it.Valid(); it.Next() {
				want, err := it.Item().ValueCopy(nil)
				if err != nil {
					return err
				}
				got, err := dst.Get(it.Item().Key())
				if err != nil {
					return fmt.Errorf("key %x is missing from the copy: %w", it.Item().Key(), err)
				}
				gv, err := got.ValueCopy(nil)
				if err != nil {
					return err
				}
				if !bytes.Equal(want, gv) || got.UserMeta() != it.Item().UserMeta() {
					return fmt.Errorf("key %x differs in the copy", it.Item().Key())
				}
			}
			return nil
		})
	})
}

func size(dir string) string {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, err := d.Info(); err == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}
