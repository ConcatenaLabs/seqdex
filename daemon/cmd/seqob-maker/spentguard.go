package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// How long a coin the maker's own swap spends stays held: while the taker has the
// co-signed swap and may broadcast it (inFlightHold), and once it settled until the
// wallet has long seen the spend (settledHold). Holding a spent coin longer costs
// nothing; releasing one early lets the next lift co-sign with it.
const (
	inFlightHold = 10 * time.Minute
	settledHold  = 30 * time.Minute
)

// spentGuard remembers the coins this maker's own swaps spend and locks them in the
// account's wallet again before every co-sign. The wallet locks a coin it selects only
// for its own window, and goes on listing a coin whose spend it has not yet seen: on
// the testnet the lift after a fill was answered with the coin that fill had just
// spent, the taker's broadcast was refused, and the book kept a dead ask.
type spentGuard struct {
	mu    sync.Mutex
	coins map[string]heldCoin
	now   func() time.Time
	last  []ports.Utxo // what the latest co-sign selected, for the serve loop to take
}

// takeLast hands over (once) what the latest co-sign selected.
func (g *spentGuard) takeLast() []ports.Utxo {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.last
	g.last = nil
	return l
}

type heldCoin struct {
	utxo  ports.Utxo
	until time.Time
}

func newSpentGuard() *spentGuard {
	return &spentGuard{coins: map[string]heldCoin{}, now: time.Now}
}

func coinKey(u ports.Utxo) string { return fmt.Sprintf("%s:%d", u.GetTxid(), u.GetIndex()) }

// hold keeps these coins held for at least d from now.
func (g *spentGuard) hold(us []ports.Utxo, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	until := g.now().Add(d)
	for _, u := range us {
		k := coinKey(u)
		if h, ok := g.coins[k]; ok && h.until.After(until) {
			continue
		}
		g.coins[k] = heldCoin{u, until}
	}
}

// held is every coin still held; the expired ones are forgotten.
func (g *spentGuard) held() []ports.Utxo {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	out := make([]ports.Utxo, 0, len(g.coins))
	for k, h := range g.coins {
		if !h.until.After(now) {
			delete(g.coins, k)
			continue
		}
		out = append(out, h.utxo)
	}
	return out
}

// How many times a co-sign selects again after the node says a selected coin is spent.
const spentRetries = 3

// guardedCompleteSwap wraps the wallet's CompleteSwap for one maker account: it locks
// every held coin first, so the wallet's selection cannot take one, then holds the
// coins this swap selected for as long as it is in flight. It answers the coins too,
// so the serve loop can hold them longer once the swap settles.
//
// spent, when set, asks the node whether a selected coin is spent (mempool included).
// The wallet can list a coin long after it was spent (on the testnet hours later, and
// across a restart, which this guard's memory does not survive): such a coin is held and
// the selection made again, so the maker never co-signs a swap the node will refuse with
// bad-txns-inputs-missingorspent. A node that cannot be asked does not stop the co-sign.
func guardedCompleteSwap(
	g *spentGuard, lock func(context.Context, []ports.Utxo) error,
	complete func() (string, []ports.Utxo, error),
	spent func(ports.Utxo) (bool, error),
) (string, []ports.Utxo, error) {
	for attempt := 0; ; attempt++ {
		if held := g.held(); len(held) > 0 {
			if err := lock(context.Background(), held); err != nil {
				// Refuse rather than co-sign blind: the wallet could hand back a coin a
				// swap of ours already spends, and the taker would broadcast for nothing.
				return "", nil, fmt.Errorf("cannot lock %d coin(s) our own swaps spend: %w", len(held), err)
			}
		}
		signed, utxos, err := complete()
		if err != nil {
			return "", nil, err
		}
		var gone []ports.Utxo
		if spent != nil {
			for _, u := range utxos {
				if s, err := spent(u); err == nil && s {
					gone = append(gone, u)
				}
			}
		}
		if len(gone) == 0 {
			g.hold(utxos, inFlightHold)
			g.mu.Lock()
			g.last = utxos
			g.mu.Unlock()
			return signed, utxos, nil
		}
		// The co-signed swap is dropped unsent; its spent coins stay held so the next
		// selection cannot take them.
		g.hold(gone, settledHold)
		fmt.Printf("co-sign: the wallet selected %d coin(s) the node says are spent (%s); selecting again\n", len(gone), coinKey(gone[0]))
		if attempt+1 >= spentRetries {
			return "", nil, fmt.Errorf("the wallet keeps selecting spent coins (%d tries); not co-signing", spentRetries)
		}
	}
}

// nodeCoinSpent asks the node at rawURL (the maker's -node-rpc) whether a coin is spent:
// gettxout with the mempool included answers null for a spent or unknown output. Nil when
// no node is configured.
func nodeCoinSpent(rawURL string) func(ports.Utxo) (bool, error) {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	port, _ := strconv.Atoi(u.Port())
	pass, _ := u.User.Password()
	rpc := xchain.NewRPC(u.Hostname(), port, u.User.Username(), pass)
	return func(c ports.Utxo) (bool, error) {
		var out json.RawMessage
		if err := rpc.Call(&out, "gettxout", c.GetTxid(), c.GetIndex(), true); err != nil {
			return false, err
		}
		return len(out) == 0 || string(out) == "null", nil
	}
}
