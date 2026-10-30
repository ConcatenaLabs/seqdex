package elements_scanner

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many scanners asking for one key at once make one call; the cache never holds
// more than its bound; a timed entry is asked again once it is old.
func TestBoundedCache(t *testing.T) {
	c := newBoundedCache[int, int](100, 0)
	var calls int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.getOrLoad(7, func() (int, error) {
				atomic.AddInt64(&calls, 1)
				time.Sleep(50 * time.Millisecond)
				return 49, nil
			}, nil)
			if err != nil || v != 49 {
				t.Errorf("got %d, %v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("200 callers at once made %d calls, not 1", calls)
	}
	for i := 0; i < 1000; i++ {
		c.put(i, i)
	}
	if n := c.len(); n > 100 {
		t.Fatalf("%d entries past the bound of 100", n)
	}
	tc := newBoundedCache[string, int](4, 30*time.Millisecond)
	n := 0
	load := func() (int, error) { n++; return n, nil }
	a, _ := tc.getOrLoad("tip", load, nil)
	b, _ := tc.getOrLoad("tip", load, nil)
	time.Sleep(40 * time.Millisecond)
	d, _ := tc.getOrLoad("tip", load, nil)
	if a != 1 || b != 1 || d != 2 {
		t.Fatalf("timed entry: %d %d %d, want 1 1 2", a, b, d)
	}
	// an answer keep refuses (a header not found yet) is asked again
	k := newBoundedCache[string, *int](4, 0)
	m := 0
	for i := 0; i < 3; i++ {
		_, _ = k.getOrLoad("h", func() (*int, error) { m++; return nil, nil }, func(p *int) bool { return p != nil })
	}
	if m != 3 {
		t.Fatalf("a refused answer was kept: %d loads", m)
	}
}
