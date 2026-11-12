package scanner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/vulpemventures/go-elements/block"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"
)

// forkChain is fakeChain plus headers, and an orphaned branch the node still knows: blocks
// staleFrom..staleTo of the branch, whose first block's parent is the main chain's.
type forkChain struct {
	*fakeChain
	staleFrom, staleTo uint32
	forget             bool // the node no longer knows the orphaned branch
	// cached: height answers still naming an orphaned block, as the scanners' shared height cache
	// can for a few seconds after a reorg; ForgetHeights drops them
	cached  map[uint32]*chainhash.Hash
	forgets int
}

func (c *forkChain) GetBlockHashByHeight(ctx context.Context, h uint32) (*chainhash.Hash, error) {
	c.mu.Lock()
	x, ok := c.cached[h]
	c.mu.Unlock()
	if ok {
		return x, nil
	}
	return c.fakeChain.GetBlockHashByHeight(ctx, h)
}

func (c *forkChain) ForgetHeights() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
	c.forgets++
}

// BlockOnActiveChain answers as the node does, from no cache.
func (c *forkChain) BlockOnActiveChain(_ context.Context, hash chainhash.Hash) (uint32, *chainhash.Hash, bool, error) {
	if h := heightOf(c.fakeChain, &hash); h != ^uint32(0) {
		if h == 0 {
			return 0, nil, true, nil
		}
		return h, hashAt(h - 1), true, nil
	}
	for h := c.staleFrom; h <= c.staleTo && !c.forget; h++ {
		if staleAt(h).IsEqual(&hash) {
			prev := hashAt(h - 1)
			if h > c.staleFrom {
				prev = staleAt(h - 1)
			}
			return h, prev, false, nil
		}
	}
	return 0, nil, false, repository.ErrBlockNotFound
}

func payment(to []byte) *transaction.Transaction {
	tx := transaction.NewTx(2)
	tx.AddOutput(transaction.NewTxOutput(make([]byte, 33), []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, to))
	return tx
}

func reportedAt(rs []Report, h uint32) bool {
	for _, r := range rs {
		if r.BlockHeight == h {
			return true
		}
	}
	return false
}

// Two accounts scan through the same reorg while the shared height answers still name the
// orphaned block at 4998 (read moments before by another account's pass): the fork point is asked
// of the node, so each account scans the new branch from 4998 and sees a payment there. Taking the
// cached answer for the truth made 4999 the restart and missed the payment.
func TestTwoAccountsThroughAReorgWithAStaleHeightAnswer(t *testing.T) {
	main := &fakeChain{tip: 5000, txs: map[uint32][]*transaction.Transaction{4998: {payment(script(2))}}}
	c := &forkChain{fakeChain: main, staleFrom: 4998, staleTo: 4999}
	a, b := New(c, c, c, hashAt(0)).(*scannerService), New(c, c, c, hashAt(0)).(*scannerService)
	a.Watch(WithStartBlock(5000), WithWatchItem(&UnspentWatchItem{outputScript: script(1)}), WithPersistentWatch(), WithLastBlockHash(staleAt(4999)))
	b.Watch(WithStartBlock(5000), WithWatchItem(&UnspentWatchItem{outputScript: script(2)}), WithPersistentWatch(), WithLastBlockHash(staleAt(4999)))
	for i, s := range []*scannerService{a, b} {
		c.mu.Lock()
		c.cached = map[uint32]*chainhash.Hash{4998: staleAt(4998)} // stale again for each
		c.mu.Unlock()
		if h := s.resumeAfterReorg(staleAt(4999), 5000); h != 4998 {
			t.Fatalf("account %d resumes at %d, not after the fork point 4997", i, h)
		}
	}
	c.mu.Lock()
	c.cached = map[uint32]*chainhash.Hash{4998: staleAt(4998)}
	c.mu.Unlock()
	pass(t, a)
	rs := pass(t, b)
	if !reportedAt(rs, 4998) {
		t.Fatalf("the payment in the new branch's block 4998 was not seen: %v", rs)
	}
	if c.forgets == 0 {
		t.Fatal("a reorg left the shared height answers in place")
	}
}

// A pass that crosses a reorg while the height answers are mixed (4998 still the orphaned block,
// 4999 already the new branch's) stops where the blocks stop linking, and the next pass scans the
// new branch from the fork point: the new branch's 4998 is never skipped.
func TestAPassThatCrossesAReorgScansTheNewBranch(t *testing.T) {
	main := &fakeChain{tip: 5005, txs: map[uint32][]*transaction.Transaction{4998: {payment(script(2))}}}
	c := &forkChain{fakeChain: main, staleFrom: 4998, staleTo: 4998,
		cached: map[uint32]*chainhash.Hash{4998: staleAt(4998)}}
	s := New(c, c, c, hashAt(0)).(*scannerService)
	s.Watch(WithStartBlock(4990), WithWatchItem(&UnspentWatchItem{outputScript: script(2)}), WithPersistentWatch())
	var all []Report
	for i := 0; i < 4; i++ {
		all = append(all, pass(t, s)...)
	}
	if !reportedAt(all, 4998) {
		t.Fatalf("the new branch's block 4998 was skipped: %v", all)
	}
	if r := s.requestsQueue.peek(); r == nil || r.StartHeight != 5006 {
		t.Fatalf("the watch ends at the tip: %+v", r)
	}
}


func staleAt(h uint32) *chainhash.Hash {
	x := chainhash.DoubleHashH([]byte(fmt.Sprintf("orphaned block %d", h)))
	return &x
}

func (c *forkChain) GetBlockHeader(_ context.Context, hash chainhash.Hash) (*block.Header, error) {
	for h := c.staleFrom; h <= c.staleTo && !c.forget; h++ {
		if staleAt(h).IsEqual(&hash) {
			prev := hashAt(h - 1)
			if h > c.staleFrom {
				prev = staleAt(h - 1)
			}
			return &block.Header{Height: h, PrevBlockHash: prev.CloneBytes()}, nil
		}
	}
	if h := heightOf(c.fakeChain, &hash); h != ^uint32(0) {
		var prev []byte
		if h > 0 {
			prev = hashAt(h - 1).CloneBytes()
		}
		return &block.Header{Height: h, PrevBlockHash: prev}, nil
	}
	return nil, repository.ErrBlockNotFound
}

// A reorg resumes the scan at the block after the fork point, found by walking the orphaned
// branch back. A fixed rollback of 1,000 blocks made each one-block reorg cost every account of
// the wallet a thousand blocks (399 such rollbacks were logged on the testnet in one morning).
func TestAReorgRescansFromTheForkPoint(t *testing.T) {
	c := &forkChain{fakeChain: &fakeChain{tip: 5000, txs: map[uint32][]*transaction.Transaction{}}, staleFrom: 4998, staleTo: 4999}
	s := New(c, c, c, hashAt(0)).(*scannerService)
	// the watch scanned the orphaned 4998 and 4999 and waits at 5000
	s.Watch(WithStartBlock(5000), WithWatchItem(&UnspentWatchItem{outputScript: script(1)}), WithPersistentWatch(), WithLastBlockHash(staleAt(4999)))
	pass(t, s)
	if c.filterGet != 3 {
		t.Fatalf("a two-block reorg rescans the blocks from the fork point, 4998 to 5000: %d filters read", c.filterGet)
	}
	// the node no longer knows the orphaned branch: the bounded rollback, as before
	c2 := &forkChain{fakeChain: &fakeChain{tip: 5000, txs: map[uint32][]*transaction.Transaction{}}, staleFrom: 4998, staleTo: 4999, forget: true}
	s2 := New(c2, c2, c2, hashAt(0)).(*scannerService)
	if h := s2.resumeAfterReorg(staleAt(4999), 5000); h != 5000-reorgRollbackHorizon {
		t.Fatalf("an unknown branch rolls back the horizon: %d", h)
	}
	// a block reported orphaned that is on the chain after all (a stale height lookup): nothing
	// is rescanned
	if h := s.resumeAfterReorg(hashAt(4999), 5000); h != 5000 {
		t.Fatalf("a block still on the chain: resume at %d", h)
	}
}

// Scanners far behind the tip read blocks at most SetCatchUpParallel at a time; a scanner
// following the tip never waits for a slot.
func TestCatchingUpIsBounded(t *testing.T) {
	SetCatchUpParallel(2)
	defer SetCatchUpParallel(4)
	var now, most atomic.Int32
	mk := func() *scannerService {
		c := &fakeChain{tip: 300, txs: map[uint32][]*transaction.Transaction{}}
		c.onFilter = func(h uint32) {
			if h+catchUpLag >= 300 {
				return // the last blocks: following the tip, no slot needed
			}
			n := now.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(200 * time.Microsecond)
			now.Add(-1)
		}
		s := newTestScanner(c)
		watchAll(s, 3)
		return s
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		s := mk()
		wg.Add(1)
		go func() { defer wg.Done(); pass(t, s) }()
	}
	wg.Wait()
	if m := most.Load(); m > 2 || m < 1 {
		t.Fatalf("at most 2 scanners read blocks at once while catching up: %d did", m)
	}

	// every slot taken: a scanner at the tip still scans its new block
	for i := 0; i < 2; i++ {
		catchUp <- struct{}{}
	}
	c := &fakeChain{tip: 300, txs: map[uint32][]*transaction.Transaction{}}
	s := newTestScanner(c)
	s.Watch(WithStartBlock(300), WithWatchItem(&UnspentWatchItem{outputScript: script(1)}), WithPersistentWatch())
	done := make(chan struct{})
	go func() { pass(t, s); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a scanner following the tip waited for a catch-up slot")
	}
}

// the main chain's headers: each block's parent is the block below it
func (c *fakeChain) GetBlockHeader(_ context.Context, hash chainhash.Hash) (*block.Header, error) {
	h := heightOf(c, &hash)
	if h == ^uint32(0) {
		return nil, repository.ErrBlockNotFound
	}
	var prev []byte
	if h > 0 {
		prev = hashAt(h - 1).CloneBytes()
	}
	return &block.Header{Height: h, PrevBlockHash: prev}, nil
}
