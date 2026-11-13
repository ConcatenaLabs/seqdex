package oceanwallet

import (
	"context"
	"errors"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type service struct {
	addr string
	conn *grpc.ClientConn

	wallet  *wallet
	account *account
	tx      *tx
	notify  *notify
}

func NewService(addr string) (ports.WalletService, error) {
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	svc := &service{
		addr:    addr,
		conn:    conn,
		wallet:  newWallet(conn),
		account: newAccount(conn),
		tx:      newTx(conn),
	}
	if err := waitForWallet(nil, func(ctx context.Context) error {
		_, err := svc.Wallet().Status(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	svc.notify, _ = newNotify(conn)

	return svc, nil
}

func (s *service) Wallet() ports.Wallet {
	return s.wallet
}

func (s *service) Account() ports.Account {
	return s.account
}

func (s *service) Transaction() ports.Transaction {
	return s.tx
}

func (s *service) Notification() ports.Notification {
	return s.notify
}

func (s *service) Close() {
	if s.notify != nil {
		s.notify.closeOnce.Do(func() { close(s.notify.closed) })
	}
	s.conn.Close()
}

// walletWait is how long a client waits for the wallet daemon to answer, at its start and when
// a stream to it breaks, before it gives up as it did before it waited. The wallet daemon is
// restarted 10 s after it stops (its unit's RestartSec) and answers a few seconds later; a
// client that exited instead came back with every other one at the same moment, and lost what
// it held in memory, such as the handlers of the trades it had accepted.
var walletWait = 3 * time.Minute

// errClosed is what waitForWallet returns once the client is closed.
var errClosed = errors.New("the wallet client is closed")

// waitForWallet calls `try` until it succeeds or walletWait has passed, 1 s apart at first and
// at most 10 s apart, each call with a 10 s deadline, and returns the last error; or errClosed
// as soon as `closed` is closed.
func waitForWallet(closed <-chan struct{}, try func(ctx context.Context) error) error {
	deadline := time.Now().Add(walletWait)
	pause := time.Second
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := try(ctx)
		cancel()
		if err == nil {
			if attempt > 1 {
				log.Infof("ocean wallet: answering again after %d attempts", attempt)
			}
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return err
		}
		if pause > left {
			pause = left
		}
		log.WithError(err).Warnf("ocean wallet: not answering, trying again in %s", pause.Round(time.Millisecond))
		select {
		case <-closed:
			return errClosed
		case <-time.After(pause):
		}
		if pause *= 2; pause > 10*time.Second {
			pause = 10 * time.Second
		}
	}
}
