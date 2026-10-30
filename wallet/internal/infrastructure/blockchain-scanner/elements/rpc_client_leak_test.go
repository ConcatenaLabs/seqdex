package elements_scanner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openSockets counts this process's open socket file descriptors.
func openSockets(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd here")
	}
	n := 0
	for _, e := range ents {
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.HasPrefix(l, "socket:") {
			n++
		}
	}
	return n
}

// A node that answers every third call slower than the client waits, as a node
// whose work queue is full does: the calls that time out must give their
// connections back, and the client must never hold more connections than its
// bound, however many callers there are at once.
func TestRpcClientHoldsNoConnectionAfterATimeout(t *testing.T) {
	var calls int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&calls, 1)%3 == 0 {
			select { // slower than the client's timeout
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"blocks":1},"error":null,"id":1}`))
	}))
	defer srv.Close()
	defer close(release)

	c := &rpcClient{serverAddr: srv.URL, httpClient: newHTTPClient(false, 1), timeout: 1}
	before, goBefore := openSockets(t), runtime.NumGoroutine()

	var wg sync.WaitGroup
	var maxOpen int64
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.call("getblockchaininfo", nil)
			if n := int64(openSockets(t)); n > atomic.LoadInt64(&maxOpen) {
				atomic.StoreInt64(&maxOpen, n)
			}
		}()
	}
	wg.Wait()
	// the slow answers arrive after the callers gave up on them
	time.Sleep(6 * time.Second)
	c.httpClient.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)
	after, goAfter := openSockets(t), runtime.NumGoroutine()
	t.Logf("sockets: %d before, at most %d during 120 calls (%d timed out), %d after; goroutines %d before, %d after",
		before, maxOpen, atomic.LoadInt64(&calls)/3, after, goBefore, goAfter)
	if after > before+2 {
		t.Fatalf("%d sockets still open after the calls ended (%d before)", after, before)
	}
	if goAfter > goBefore+5 {
		t.Fatalf("%d goroutines left behind (%d before)", goAfter, goBefore)
	}
	// the server's sockets count too: this process holds both ends
	if maxOpen > int64(before)+2*rpcMaxConns+4 {
		t.Fatalf("%d sockets open at once, past the client's bound of %d connections", maxOpen, rpcMaxConns)
	}
}
