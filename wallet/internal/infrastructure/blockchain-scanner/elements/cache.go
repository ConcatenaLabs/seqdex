package elements_scanner

import (
	"sync"
	"time"
)

// boundedCache holds at most `max` entries, each for at most `ttl` (0: until it
// is evicted). Every account's scanner reads the same chain from one node: the
// tip, each block's hash, header, filter and, on a match, the block itself.
// Shared here, each is asked of the node once instead of once per account,
// which is what kept a node with many accounts busy enough to time calls out.
// When full it drops the oldest half; it never grows past `max`.
type boundedCache[K comparable, V any] struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	items   map[K]cacheItem[V]
	order   []K
	loading map[K]*cacheLoad[V]
}

// cacheLoad is one call to the node that every caller asking for the same key
// while it runs waits on, instead of making its own.
type cacheLoad[V any] struct {
	done chan struct{}
	v    V
	err  error
}

type cacheItem[V any] struct {
	v  V
	at time.Time
}

func newBoundedCache[K comparable, V any](max int, ttl time.Duration) *boundedCache[K, V] {
	return &boundedCache[K, V]{max: max, ttl: ttl, items: make(map[K]cacheItem[V], max), loading: map[K]*cacheLoad[V]{}}
}

func (c *boundedCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[k]
	if !ok || (c.ttl > 0 && time.Since(it.at) > c.ttl) {
		var zero V
		return zero, false
	}
	return it.v, true
}

func (c *boundedCache[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[k]; !ok {
		if len(c.order) >= c.max {
			drop := c.order[:c.max/2]
			for _, d := range drop {
				delete(c.items, d)
			}
			c.order = append([]K(nil), c.order[c.max/2:]...)
		}
		c.order = append(c.order, k)
	}
	c.items[k] = cacheItem[V]{v: v, at: time.Now()}
}

func (c *boundedCache[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// getOrLoad answers from the cache, or calls load once for every caller that
// asks for k while it runs, and keeps what it gives unless it fails or keep says
// no (an answer that may still change, such as a block not found yet).
func (c *boundedCache[K, V]) getOrLoad(k K, load func() (V, error), keep func(V) bool) (V, error) {
	if v, ok := c.get(k); ok {
		return v, nil
	}
	c.mu.Lock()
	if l, ok := c.loading[k]; ok {
		c.mu.Unlock()
		<-l.done
		return l.v, l.err
	}
	l := &cacheLoad[V]{done: make(chan struct{})}
	c.loading[k] = l
	c.mu.Unlock()

	l.v, l.err = load()
	if l.err == nil && (keep == nil || keep(l.v)) {
		c.put(k, l.v)
	}
	c.mu.Lock()
	delete(c.loading, k)
	c.mu.Unlock()
	close(l.done)
	return l.v, l.err
}
