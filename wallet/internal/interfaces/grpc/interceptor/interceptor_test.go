package grpc_interceptor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A handler that panics refuses its one call with Internal; the process, and
// every other call, stream and scan in it, carries on.
func TestAPanickingHandlerRefusesItsCallAndNothingElse(t *testing.T) {
	panicking := func(context.Context, interface{}) (interface{}, error) {
		var p *struct{ x int }
		return p.x, nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/ocean.v1.WalletService/GetInfo"}

	var err error
	require.NotPanics(t, func() {
		_, err = unaryChain()(context.Background(), nil, info, panicking)
	})
	require.Equal(t, codes.Internal, status.Code(err))

	streaming := func(interface{}, grpc.ServerStream) error {
		var p *struct{ x int }
		_ = p.x
		return nil
	}
	sinfo := &grpc.StreamServerInfo{FullMethod: "/ocean.v1.NotificationService/TransactionNotifications"}
	require.NotPanics(t, func() {
		err = streamChain()(nil, fakeStream{}, sinfo, streaming)
	})
	require.Equal(t, codes.Internal, status.Code(err))
}

type fakeStream struct{ grpc.ServerStream }

func (fakeStream) Context() context.Context { return context.Background() }
