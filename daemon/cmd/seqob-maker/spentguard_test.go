package main

import (
	"context"
	"errors"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
)

type coin struct {
	ports.Utxo
	txid string
	vout uint32
}

func (c coin) GetTxid() string  { return c.txid }
func (c coin) GetIndex() uint32 { return c.vout }

// A wallet as Ocean behaves: selecting a coin locks it for the wallet's own window
// (2 minutes here), a coin locked again stays out of selection for another window, and a
// spend is seen only once the wallet's scanner reports it (late, on the testnet).
type oceanLike struct {
	now    func() time.Time
	coins  []coin
	locked map[string]time.Time
	seen   map[string]bool // spends the wallet has seen
	window time.Duration
}

func (w *oceanLike) lock(us []ports.Utxo) {
	for _, u := range us {
		w.locked[coinKey(u)] = w.now().Add(w.window)
	}
}

func (w *oceanLike) selectOne() (ports.Utxo, error) {
	for _, c := range w.coins {
		k := coinKey(c)
		if w.seen[k] || w.locked[k].After(w.now()) {
			continue
		}
		w.lock([]ports.Utxo{c})
		return c, nil
	}
	return nil, errors.New("insufficient funds")
}

// The testnet failure: the lift after a fill was co-signed with the coin that fill spent.
// With the guard, the coin a settled swap spent is locked again before every co-sign, so
// the next lifts take other coins while the wallet still lists it.
func TestTheLiftAfterAFillNeverTakesTheCoinItSpent(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }
	w := &oceanLike{now: now, window: 2 * time.Minute, locked: map[string]time.Time{}, seen: map[string]bool{},
		coins: []coin{{txid: "c1", vout: 0}, {txid: "c2", vout: 1}, {txid: "c3", vout: 0}}}
	g := newSpentGuard()
	g.now = now
	lock := func(_ context.Context, us []ports.Utxo) error { w.lock(us); return nil }
	lift := func() (ports.Utxo, error) {
		_, us, err := guardedCompleteSwap(g, lock, func() (string, []ports.Utxo, error) {
			u, err := w.selectOne()
			if err != nil {
				return "", nil, err
			}
			return "signed", []ports.Utxo{u}, nil
		}, nil)
		if err != nil {
			return nil, err
		}
		return us[0], nil
	}
	settle := func() { g.hold(g.takeLast(), settledHold) }

	// Three takers in a row, each 3 minutes after the last: the wallet's own lock has
	// expired every time and it has not yet seen any spend.
	var took []string
	for i := 0; i < 3; i++ {
		u, err := lift()
		if err != nil {
			t.Fatalf("lift %d refused: %v", i+1, err)
		}
		took = append(took, coinKey(u))
		settle()
		clock = clock.Add(3 * time.Minute)
	}
	if took[0] == took[1] || took[1] == took[2] || took[0] == took[2] {
		t.Fatalf("a lift co-signed with a coin an earlier fill spent: %v", took)
	}

	// The control: without the guard the same wallet, at the same moment, hands back
	// the first fill's coin again (its lock expired, its spend not yet seen).
	if u, _ := w.selectOne(); u == nil || coinKey(u) != took[0] {
		t.Fatalf("the scenario does not reproduce the testnet failure (got %v)", u)
	}

	// A fourth lift finds nothing it may spend and is refused rather than co-signed
	// with a spent coin.
	clock = clock.Add(3 * time.Minute)
	if _, err := lift(); err == nil {
		t.Fatal("a lift was co-signed although every coin is spent by our own swaps")
	}

	// Once the wallet has long seen the spends, the guard forgets them.
	clock = clock.Add(settledHold)
	if n := len(g.held()); n != 0 {
		t.Fatalf("%d coin(s) still held after %v", n, settledHold)
	}
}

func TestALiftIsRefusedWhenTheSpentCoinsCannotBeLocked(t *testing.T) {
	g := newSpentGuard()
	g.hold([]ports.Utxo{coin{txid: "c1"}}, settledHold)
	called := false
	_, _, err := guardedCompleteSwap(g,
		func(context.Context, []ports.Utxo) error { return errors.New("wallet unreachable") },
		func() (string, []ports.Utxo, error) { called = true; return "signed", nil, nil }, nil)
	if err == nil || called {
		t.Fatalf("co-signed without locking our spent coins (err=%v, called=%v)", err, called)
	}
}

func TestARungThatLeftTheBookIsPostedAgain(t *testing.T) {
	for st, want := range map[seqobv1.OfferStatus]bool{
		seqobv1.OfferStatus_OFFER_STATUS_FILLED:           true,
		seqobv1.OfferStatus_OFFER_STATUS_UTXO_INVALIDATED: true,
		seqobv1.OfferStatus_OFFER_STATUS_PARTIAL:          false,
		seqobv1.OfferStatus_OFFER_STATUS_OPEN:             false,
		seqobv1.OfferStatus_OFFER_STATUS_CANCELLED:        false,
		seqobv1.OfferStatus_OFFER_STATUS_EXPIRED:          false,
	} {
		if offerLeftBook(st) != want {
			t.Fatalf("offerLeftBook(%s) = %v, want %v", st, !want, want)
		}
	}
}

// The testnet after the maker's restart: its wallet still listed a coin spent hours
// earlier, so the guard's memory had nothing to lock and the lift was co-signed with it
// (bad-txns-inputs-missingorspent). With the node asked, that coin is held and the
// selection made again.
func TestACoinTheWalletStillListsAfterItsSpendIsNotCoSigned(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }
	w := &oceanLike{now: now, window: 2 * time.Minute, locked: map[string]time.Time{}, seen: map[string]bool{},
		coins: []coin{{txid: "old", vout: 0}, {txid: "c2", vout: 1}}}
	onChainSpent := map[string]bool{"old:0": true} // spent before this process started
	spent := func(u ports.Utxo) (bool, error) { return onChainSpent[coinKey(u)], nil }
	lock := func(_ context.Context, us []ports.Utxo) error { w.lock(us); return nil }
	sel := func() (string, []ports.Utxo, error) {
		u, err := w.selectOne()
		if err != nil {
			return "", nil, err
		}
		return "signed", []ports.Utxo{u}, nil
	}

	// Control: without the node's answer the fresh guard co-signs with the spent coin.
	if _, us, _ := guardedCompleteSwap(newSpentGuard(), lock, sel, nil); coinKey(us[0]) != "old:0" {
		t.Fatalf("the scenario does not reproduce the testnet failure (took %s)", coinKey(us[0]))
	}
	clock = clock.Add(3 * time.Minute) // the wallet's own lock expires

	g := newSpentGuard()
	g.now = now
	_, us, err := guardedCompleteSwap(g, lock, sel, spent)
	if err != nil || coinKey(us[0]) != "c2:1" {
		t.Fatalf("co-signed %v (err %v), want the unspent c2:1", us, err)
	}
	// The spent coin stays held, so later lifts do not try it again.
	held := map[string]bool{}
	for _, u := range g.held() {
		held[coinKey(u)] = true
	}
	if !held["old:0"] {
		t.Fatal("the spent coin is not held")
	}

	// Every coin the wallet offers is spent: refused after a few tries, never co-signed.
	onChainSpent["c2:1"] = true
	clock = clock.Add(settledHold + time.Minute)
	if _, _, err := guardedCompleteSwap(newSpentGuard(), lock, sel, spent); err == nil {
		t.Fatal("co-signed although the node says every selected coin is spent")
	}
}

func TestANodeThatCannotBeAskedDoesNotStopTheCoSign(t *testing.T) {
	_, us, err := guardedCompleteSwap(newSpentGuard(),
		func(context.Context, []ports.Utxo) error { return nil },
		func() (string, []ports.Utxo, error) { return "signed", []ports.Utxo{coin{txid: "c1"}}, nil },
		func(ports.Utxo) (bool, error) { return false, errors.New("node down") })
	if err != nil || len(us) != 1 {
		t.Fatalf("a node error stopped the co-sign: %v", err)
	}
}
