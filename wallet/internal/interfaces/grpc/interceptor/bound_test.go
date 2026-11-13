package grpc_interceptor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Over the bound a request waits its turn while its caller is there, at most the wait, and is
// then refused with ResourceExhausted: never left hanging. Requests outside the bound and those
// that find a free slot run at once.
func TestHeavyRequestsAreBounded(t *testing.T) {
	SetHeavyRequestBound(1, 200*time.Millisecond)
	defer SetHeavyRequestBound(16, 30*time.Second)
	heavy := &grpc.UnaryServerInfo{FullMethod: "/ocean.v1.AccountService/Balance"}
	light := &grpc.UnaryServerInfo{FullMethod: "/ocean.v1.WalletService/Status"}
	ok := func(context.Context, interface{}) (interface{}, error) { return "done", nil }

	release := make(chan struct{})
	running := make(chan struct{})
	go boundHeavy(context.Background(), nil, heavy, func(context.Context, interface{}) (interface{}, error) {
		close(running)
		<-release
		return nil, nil
	})
	<-running

	// a light request does not wait
	res, err := boundHeavy(context.Background(), nil, light, ok)
	require.NoError(t, err)
	require.Equal(t, "done", res)

	// a heavy one waits up to the bound's wait, then is refused with an error to retry
	start := time.Now()
	_, err = boundHeavy(context.Background(), nil, heavy, ok)
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "%v", err)
	require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	require.Less(t, time.Since(start), 2*time.Second)

	// one whose caller leaves first gets the caller's error at once
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = boundHeavy(ctx, nil, heavy, ok)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err), "%v", err)
	require.Less(t, time.Since(start), 150*time.Millisecond)

	// once the slot is free, the next one runs
	close(release)
	require.Eventually(t, func() bool {
		res, err := boundHeavy(context.Background(), nil, heavy, ok)
		return err == nil && res == "done"
	}, time.Second, 10*time.Millisecond)
}
