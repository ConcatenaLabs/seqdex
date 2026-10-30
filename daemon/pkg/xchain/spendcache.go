package xchain

import (
	"fmt"
	"sync"
)

// broadcaster is the subset of a chain client a spend needs to reach the network.
type broadcaster interface {
	Broadcast(rawHex string) (string, error)
}

// spendCache makes a claim or refund of one outpoint IDEMPOTENT. Every call used
// to build a fresh transaction to a fresh address and broadcast it; a broadcast
// whose reply was lost (node accepted, HTTP timed out) was then retried with a
// CONFLICTING spend of the same outpoint, which the node rejects, and the caller
// kept believing the claim had failed. The first raw transaction built for an
// (outpoint, kind) is remembered and re-sent on every retry; a node that already
// has it answers with its txid.
type spendCache struct {
	mu   sync.Mutex
	raws map[string]string // "txid:vout:kind" -> raw hex
}

// once builds the spend at most once and broadcasts whatever was built.
func (c *spendCache) once(b broadcaster, txid string, vout uint32, kind string, build func() (string, error)) (string, error) {
	key := fmt.Sprintf("%s:%d:%s", txid, vout, kind)
	c.mu.Lock()
	if c.raws == nil {
		c.raws = make(map[string]string)
	}
	raw, ok := c.raws[key]
	c.mu.Unlock()
	if !ok {
		var err error
		if raw, err = build(); err != nil {
			return "", err
		}
		c.mu.Lock()
		if prior, race := c.raws[key]; race {
			raw = prior
		} else {
			c.raws[key] = raw
		}
		c.mu.Unlock()
	}
	return b.Broadcast(raw)
}

// replace makes raw the spend re-sent for (outpoint, kind) from now on: the
// keeper's fee-bumped replacement, so that a retry re-sends the transaction the
// node holds rather than the one it replaced.
func (c *spendCache) replace(txid string, vout uint32, kind, raw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raws == nil {
		c.raws = make(map[string]string)
	}
	c.raws[fmt.Sprintf("%s:%d:%s", txid, vout, kind)] = raw
}
