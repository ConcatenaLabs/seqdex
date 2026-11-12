package wallet

import (
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
)

type notifyingWallet struct {
	ports.WalletService
	ch chan ports.WalletTxNotification
}
type notifyingAPI struct {
	ports.Notification
	ch chan ports.WalletTxNotification
}

func (w *notifyingWallet) Notification() ports.Notification { return notifyingAPI{ch: w.ch} }
func (n notifyingAPI) GetTxNotifications() chan ports.WalletTxNotification {
	return n.ch
}

type someTx struct{ ports.WalletTxNotification }

// A handler that panics on one notification is skipped for that one: the
// listener goroutine, the daemon, and every other handler carry on, and the
// panicking handler still sees the notifications after it.
func TestAPanickingTxHandlerDoesNotStopTheListener(t *testing.T) {
	ch := make(chan ports.WalletTxNotification)
	s := &Service{
		wallet: &notifyingWallet{ch: ch},
		txNotificationHandlers: txNotificationQueue{
			&sync.Mutex{}, make([]func(ports.WalletTxNotification) bool, 0),
		},
	}
	seen := make(chan int, 8)
	calls := 0
	s.RegisterHandlerForTxEvent(func(ports.WalletTxNotification) bool {
		calls++
		seen <- calls
		if calls == 1 {
			var m map[string]*int
			_ = *m["absent"]
		}
		return false
	})
	done := make(chan struct{})
	go func() { s.listenToTxNotifications(); close(done) }()

	ch <- someTx{}
	ch <- someTx{}
	for want := 1; want <= 2; want++ {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("call %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("notification %d never reached the handler", want)
		}
	}
	close(ch)
	<-done
}
