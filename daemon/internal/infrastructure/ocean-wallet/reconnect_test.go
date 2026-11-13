package oceanwallet

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/ocean/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeWallet answers Status and sends what `txs` carries on every tx stream.
type fakeWallet struct {
	pb.UnimplementedWalletServiceServer
	pb.UnimplementedNotificationServiceServer
	txs chan string
}

func (f *fakeWallet) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{Initialized: true, Unlocked: true}, nil
}

func (f *fakeWallet) TransactionNotifications(
	_ *pb.TransactionNotificationsRequest, s pb.NotificationService_TransactionNotificationsServer,
) error {
	for {
		select {
		case id := <-f.txs:
			if err := s.Send(&pb.TransactionNotificationsResponse{Txid: id}); err != nil {
				return err
			}
		case <-s.Context().Done():
			return nil
		}
	}
}

func (f *fakeWallet) UtxosNotifications(
	_ *pb.UtxosNotificationsRequest, s pb.NotificationService_UtxosNotificationsServer,
) error {
	<-s.Context().Done()
	return nil
}

// serve starts the fake wallet on addr ("127.0.0.1:0" for any port) and returns its server and
// the address it listens on.
func serve(t *testing.T, addr string, f *fakeWallet) (*grpc.Server, string) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := grpc.NewServer()
	pb.RegisterWalletServiceServer(srv, f)
	pb.RegisterNotificationServiceServer(srv, f)
	go srv.Serve(lis)
	return srv, lis.Addr().String()
}

func withWait(t *testing.T, d time.Duration) func() int {
	t.Helper()
	oldWait, oldGiveUp := walletWait, giveUp
	var mu sync.Mutex
	gaveUp := []string{}
	walletWait = d
	giveUp = func(err error, what string) {
		mu.Lock()
		gaveUp = append(gaveUp, what)
		mu.Unlock()
	}
	t.Cleanup(func() { walletWait, giveUp = oldWait, oldGiveUp })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(gaveUp)
	}
}

func nextTx(t *testing.T, svc *service, within time.Duration) string {
	t.Helper()
	select {
	case n := <-svc.notify.GetTxNotifications():
		return n.(txNotifyInfo).GetTxid()
	case <-time.After(within):
		t.Fatalf("no tx notification within %s", within)
		return ""
	}
}

// The wallet stops and comes back on its address: the client opens its streams again and
// carries on, instead of ending the process.
func TestTheStreamsOpenAgainWhenTheWalletComesBack(t *testing.T) {
	gaveUp := withWait(t, 30*time.Second)
	f := &fakeWallet{txs: make(chan string, 1)}
	srv, addr := serve(t, "127.0.0.1:0", f)
	w, err := NewService(addr)
	require.NoError(t, err)
	defer w.Close()
	svc := w.(*service)

	f.txs <- "before"
	require.Equal(t, "before", nextTx(t, svc, 5*time.Second))

	srv.Stop() // the wallet is killed
	time.Sleep(3 * time.Second)
	srv2, _ := serve(t, addr, f) // and back, ten seconds later on the box
	defer srv2.Stop()

	f.txs <- "after"
	require.Equal(t, "after", nextTx(t, svc, 20*time.Second))
	require.Zero(t, gaveUp())
}

// A wallet that stays away longer than the wait ends the process as before.
func TestAWalletAwayTooLongEndsTheProcess(t *testing.T) {
	gaveUp := withWait(t, 2*time.Second)
	f := &fakeWallet{txs: make(chan string, 1)}
	srv, addr := serve(t, "127.0.0.1:0", f)
	w, err := NewService(addr)
	require.NoError(t, err)
	defer w.Close()

	srv.Stop()
	start := time.Now()
	require.Eventually(t, func() bool { return gaveUp() == 2 }, 15*time.Second, 100*time.Millisecond,
		"both streams give up once the wait has passed")
	require.GreaterOrEqual(t, time.Since(start), 1900*time.Millisecond, "not before the wait has passed")
}

// At its start a client waits for a wallet that is not answering yet.
func TestTheClientWaitsForTheWalletAtItsStart(t *testing.T) {
	withWait(t, 30*time.Second)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	lis.Close()

	f := &fakeWallet{txs: make(chan string, 1)}
	go func() {
		time.Sleep(3 * time.Second)
		srv, _ := serve(t, addr, f)
		t.Cleanup(srv.Stop)
	}()
	start := time.Now()
	w, err := NewService(addr)
	require.NoError(t, err)
	defer w.Close()
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second)

	// and none longer than the wait
	walletWait = 2 * time.Second
	lis2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gone := lis2.Addr().String()
	lis2.Close()
	_, err = NewService(gone)
	require.Error(t, err)
}

// A client closed while it waits for the wallet stops waiting, and gives nothing up.
func TestAClosedClientStopsWaiting(t *testing.T) {
	gaveUp := withWait(t, 30*time.Second)
	f := &fakeWallet{txs: make(chan string, 1)}
	srv, addr := serve(t, "127.0.0.1:0", f)
	w, err := NewService(addr)
	require.NoError(t, err)
	srv.Stop()
	time.Sleep(1500 * time.Millisecond)
	w.Close()
	time.Sleep(3 * time.Second)
	require.Zero(t, gaveUp())
}
