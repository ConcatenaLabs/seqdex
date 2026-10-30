package application

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
)

func bareNotifications() *NotificationService {
	return &NotificationService{
		utxoSubs: map[chan domain.UtxoEvent]struct{}{},
		txSubs:   map[chan domain.TransactionEvent]struct{}{},
		log:      func(string, ...interface{}) {},
	}
}

// With no client listening, events are held nowhere; each client listening gets
// every event; a client whose stream ends is let go; one that stops reading
// never holds up the wallet.
func TestNotificationsHoldNothingForAbsentClients(t *testing.T) {
	ns := bareNotifications()
	before := runtime.NumGoroutine()
	for i := 0; i < 10000; i++ {
		ns.publishUtxo(domain.UtxoEvent{EventType: domain.UtxoAdded})
		ns.publishTx(domain.TransactionEvent{EventType: domain.TransactionAdded})
	}
	time.Sleep(100 * time.Millisecond)
	if g := runtime.NumGoroutine(); g > before+2 {
		t.Fatalf("%d goroutines after 20,000 events with no client, %d before", g, before)
	}

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	a, _ := ns.GetUtxoChannel(ctxA)
	b, _ := ns.GetUtxoChannel(ctxB)
	for i := 0; i < 3; i++ {
		ns.publishUtxo(domain.UtxoEvent{EventType: domain.UtxoSpent})
	}
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("each client gets every event: %d and %d of 3", len(a), len(b))
	}

	stopA()
	time.Sleep(50 * time.Millisecond)
	ns.mu.Lock()
	n := len(ns.utxoSubs)
	ns.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d clients listening after one stream ended, want 1", n)
	}

	// b stops reading: past its buffer its events are dropped, and publishing goes on
	done := make(chan struct{})
	go func() {
		for i := 0; i < notifyBuffer+500; i++ {
			ns.publishUtxo(domain.UtxoEvent{EventType: domain.UtxoAdded})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a client that stopped reading held publishing up")
	}
	if len(b) != notifyBuffer || ns.dropped == 0 {
		t.Fatalf("a client behind: %d buffered (want %d), %d dropped", len(b), notifyBuffer, ns.dropped)
	}
	stopB()
}
