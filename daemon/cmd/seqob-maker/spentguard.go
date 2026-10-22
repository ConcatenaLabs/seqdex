package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
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

// guardedCompleteSwap wraps the wallet's CompleteSwap for one maker account: it locks
// every held coin first, so the wallet's selection cannot take one, then holds the
// coins this swap selected for as long as it is in flight. It answers the coins too,
// so the serve loop can hold them longer once the swap settles.
func guardedCompleteSwap(
	g *spentGuard, lock func(context.Context, []ports.Utxo) error,
	complete func() (string, []ports.Utxo, error),
) (string, []ports.Utxo, error) {
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
	g.hold(utxos, inFlightHold)
	g.mu.Lock()
	g.last = utxos
	g.mu.Unlock()
	return signed, utxos, nil
}
