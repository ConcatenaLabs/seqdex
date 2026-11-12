package grpc_interface

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/aejkcs50/seqdex/wallet/api-spec/protobuf/gen/go/ocean/v1"
	"github.com/aejkcs50/seqdex/wallet/internal/core/application"
	"github.com/aejkcs50/seqdex/wallet/internal/infrastructure/storage/db/inmemory"
	grpc_handler "github.com/aejkcs50/seqdex/wallet/internal/interfaces/grpc/handler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// A stop ends every notification stream at once, so the server stops at once rather than wait
// for streams its clients keep open for good: seqdexd and every maker hold some, and a stop that
// ended only one of them waited for the rest until systemd killed the process, so the wallet's
// own stop (its scan heights saved, its store closed) never ran.
func TestAStopEndsEveryNotificationStream(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	notifications := application.NewNotificationService(inmemory.NewRepoManager(), nil)
	chClose := make(chan struct{})
	srv := grpc.NewServer()
	pb.RegisterNotificationServiceServer(srv, grpc_handler.NewNotificationHandler(notifications, chClose))
	go srv.Serve(lis)
	s := &service{grpcServer: srv, chCloseStreamConnections: chClose, closeStreams: &sync.Once{},
		log: func(string, ...interface{}) {}, warn: func(error, string, ...interface{}) {}}

	conn, err := grpc.Dial(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewNotificationServiceClient(conn)
	ended := make(chan error, 4)
	for i := 0; i < 2; i++ {
		txs, err := client.TransactionNotifications(context.Background(), &pb.TransactionNotificationsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		utxos, err := client.UtxosNotifications(context.Background(), &pb.UtxosNotificationsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, err := txs.Recv(); ended <- err }()
		go func() { _, err := utxos.Recv(); ended <- err }()
	}
	time.Sleep(300 * time.Millisecond) // the four streams are open on the server

	start := time.Now()
	s.stop(true)
	took := time.Since(start)
	if took > 2*time.Second {
		t.Fatalf("the stop took %s with four notification streams open", took)
	}
	for i := 0; i < 4; i++ {
		select {
		case err := <-ended:
			if err == nil {
				t.Fatalf("stream %d ended with no error", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 4 streams ended", i)
		}
	}
	t.Logf("the stop took %s with four notification streams open", took)
	s.stop(true) // a second stop is harmless
}
