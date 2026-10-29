package scanner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/vulpemventures/go-elements/block"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"
)

// fakeChain is a chain of `tip`+1 blocks: each block's filter holds the
// scripts its transactions pay, and `failAt` makes reading one height's
// filter fail.
type fakeChain struct {
	repository.FilterRepository
	repository.BlockHeaderRepository
	mu        sync.Mutex
	tip       uint32
	txs       map[uint32][]*transaction.Transaction
	filterGet int
	failAt    map[uint32]error
}

func hashAt(h uint32) *chainhash.Hash {
	x := chainhash.DoubleHashH([]byte(fmt.Sprintf("block %d", h)))
	return &x
}

func heightOf(c *fakeChain, hash *chainhash.Hash) uint32 {
	for h := uint32(0); h <= c.tip; h++ {
		if hashAt(h).IsEqual(hash) {
			return h
		}
	}
	return ^uint32(0)
}

func (c *fakeChain) ChainTip(context.Context) (*block.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &block.Header{Height: c.tip}, nil
}

func (c *fakeChain) GetBlockHashByHeight(_ context.Context, h uint32) (*chainhash.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h > c.tip {
		return nil, repository.ErrBlockNotFound
	}
	return hashAt(h), nil
}

func (c *fakeChain) GetFilter(_ context.Context, key repository.FilterKey) (*repository.FilterEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filterGet++
	hash, _ := chainhash.NewHash(key.BlockHash)
	h := heightOf(c, hash)
	if err := c.failAt[h]; err != nil {
		return nil, err
	}
	items := [][]byte{[]byte("nothing watched pays here")}
	for _, tx := range c.txs[h] {
		for _, o := range tx.Outputs {
			items = append(items, o.Script)
		}
	}
	f, err := gcs.BuildGCSFilter(builder.DefaultP, builder.DefaultM, builder.DeriveKey(hash), items)
	if err != nil {
		return nil, err
	}
	return repository.NewFilterEntry(key, f)
}

func (c *fakeChain) GetBlock(hash *chainhash.Hash) (*block.Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := heightOf(c, hash)
	return &block.Block{Header: &block.Header{Height: h}, TransactionsData: &block.Transactions{Transactions: c.txs[h]}}, nil
}

func newTestScanner(c *fakeChain) *scannerService {
	return New(c, c, c, hashAt(0)).(*scannerService)
}

func script(i int) []byte { return []byte(fmt.Sprintf("watched script %02d", i)) }

func watchAll(s *scannerService, n int) {
	for i := 0; i < n; i++ {
		s.Watch(WithStartBlock(0), WithWatchItem(&UnspentWatchItem{outputScript: script(i)}), WithPersistentWatch())
	}
}

func pass(t *testing.T, s *scannerService) []Report {
	t.Helper()
	ch := make(chan Report, 100)
	if err := s.requestWorker(s.requestsQueue.peek(), ch); err != nil {
		t.Fatalf("pass: %v", err)
	}
	close(ch)
	var out []Report
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// A watch that matched nothing resumes at the first block it has not scanned:
// once caught up, each new block costs one filter read, not a rescan from the
// watch's start.
func TestUnmatchedWatchesResumeWhereTheyStopped(t *testing.T) {
	c := &fakeChain{tip: 1000, txs: map[uint32][]*transaction.Transaction{}}
	s := newTestScanner(c)
	watchAll(s, 20)

	pass(t, s)
	if c.filterGet != 1001 {
		t.Fatalf("the first pass reads every block's filter once: %d reads", c.filterGet)
	}
	c.mu.Lock()
	c.tip = 1001
	c.filterGet = 0
	c.mu.Unlock()
	pass(t, s)
	if c.filterGet != 1 {
		t.Fatalf("a new block costs one filter read, got %d (a rescan from the start)", c.filterGet)
	}
	if n := s.requestsQueue.len(); n != 20 {
		t.Fatalf("the 20 watches stay watched: %d queued", n)
	}
	for _, r := range s.requestsQueue.requests {
		if r.StartHeight != 1002 || r.LastBlockHash == nil || !r.LastBlockHash.IsEqual(hashAt(1001)) {
			t.Fatalf("a watch resumes at 1002 after block 1001: start %d", r.StartHeight)
		}
	}
}

// A failed read stops the pass and keeps every watch it carried, at the block
// it could not read.
func TestAnErrorKeepsEveryWatch(t *testing.T) {
	c := &fakeChain{tip: 1000, txs: map[uint32][]*transaction.Transaction{}, failAt: map[uint32]error{500: errors.New("node busy")}}
	s := newTestScanner(c)
	watchAll(s, 20)
	err := s.requestWorker(s.requestsQueue.peek(), make(chan Report, 1))
	if err == nil {
		t.Fatal("the pass reports the failed read")
	}
	if n := s.requestsQueue.len(); n != 20 {
		t.Fatalf("no watch is dropped by an error: %d queued", n)
	}
	for _, r := range s.requestsQueue.requests {
		if r.StartHeight != 500 {
			t.Fatalf("a watch resumes at the block it could not read: %d", r.StartHeight)
		}
	}
	// a filter near the tip the node has not indexed yet is waited for, never skipped
	c.failAt = map[uint32]error{998: repository.ErrFilterNotFound}
	if err := s.requestWorker(s.requestsQueue.peek(), make(chan Report, 1)); err != nil {
		t.Fatalf("a missing filter is a pause, not an error: %v", err)
	}
	for _, r := range s.requestsQueue.requests {
		if r.StartHeight != 998 {
			t.Fatalf("a block near the tip whose filter is missing is not skipped: %d", r.StartHeight)
		}
	}
}

// A watch matched by two transactions of one block is reported twice and
// watched again once.
func TestAMatchIsWatchedAgainOnce(t *testing.T) {
	pay := func(tag string) *transaction.Transaction {
		tx := transaction.NewTx(2)
		tx.AddOutput(transaction.NewTxOutput(make([]byte, 33), []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, script(3)))
		tx.AddOutput(transaction.NewTxOutput(make([]byte, 33), []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, []byte(tag)))
		return tx
	}
	c := &fakeChain{tip: 20, txs: map[uint32][]*transaction.Transaction{10: {pay("a"), pay("b")}}}
	s := newTestScanner(c)
	watchAll(s, 5)
	reports := pass(t, s)
	if len(reports) != 2 {
		t.Fatalf("one report a transaction: %d", len(reports))
	}
	// the matched watch was watched again from block 11 and has scanned on to the tip
	n := 0
	for _, r := range s.requestsQueue.requests {
		if string(r.Item.Bytes()) == string(script(3)) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the matched watch is queued once, not once a transaction: %d", n)
	}
	if q := s.requestsQueue.len(); q != 5 {
		t.Fatalf("five watches, five requests: %d", q)
	}
}

// A block with no filter well below the tip is read in full: its match is found, and the scan
// goes on past it. Only a block near the tip waits for its filter.
func TestAMissingFilterDeepIsScannedInFull(t *testing.T) {
	pay := transaction.NewTx(2)
	pay.AddOutput(transaction.NewTxOutput(make([]byte, 33), []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, script(2)))
	c := &fakeChain{tip: 1000, txs: map[uint32][]*transaction.Transaction{500: {pay}},
		failAt: map[uint32]error{500: repository.ErrFilterNotFound, 998: repository.ErrFilterNotFound}}
	s := newTestScanner(c)
	watchAll(s, 5)
	reports := pass(t, s)
	if len(reports) != 1 || reports[0].BlockHeight != 500 {
		t.Fatalf("the match in the block with no filter is found: %d reports", len(reports))
	}
	for _, r := range s.requestsQueue.requests {
		if r.StartHeight != 998 {
			t.Fatalf("the scan goes past the deep gap and waits at the missing filter near the tip (998): %d", r.StartHeight)
		}
	}
}

// The lowest height some watch has not scanned: the queue's lowest start, or a pass in flight
// below it, so a checkpoint never runs ahead of what is scanned.
func TestLowestUnscanned(t *testing.T) {
	c := &fakeChain{tip: 1000, txs: map[uint32][]*transaction.Transaction{}}
	s := newTestScanner(c)
	if _, ok := s.LowestUnscanned(); ok {
		t.Fatal("no watch, no height")
	}
	watchAll(s, 3)
	if h, _ := s.LowestUnscanned(); h != 0 {
		t.Fatalf("before a pass: %d", h)
	}
	pass(t, s)
	if h, _ := s.LowestUnscanned(); h != 1001 {
		t.Fatalf("after a pass to the tip: %d", h)
	}
	s.inFlight.Store(301) // a pass from 300 running
	if h, _ := s.LowestUnscanned(); h != 300 {
		t.Fatalf("a pass in flight from 300 bounds it: %d", h)
	}
}
