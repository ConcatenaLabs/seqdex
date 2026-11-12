package grpc_interceptor

import (
	"runtime/debug"

	middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recoverPanic turns a panic in a handler into an Internal error for that one
// call, logged with its stack. Without it a single request answered in a
// state the handler did not expect (a locked wallet, a closed store) takes the
// whole daemon down, with every account's scan and every stream with it.
func recoverPanic(p interface{}) error {
	log.Errorf("grpc: a handler panicked, the call is refused: %v\n%s", p, debug.Stack())
	return status.Errorf(codes.Internal, "internal error")
}

func unaryChain() grpc.UnaryServerInterceptor {
	return middleware.ChainUnaryServer(
		grpc_recovery.UnaryServerInterceptor(
			grpc_recovery.WithRecoveryHandler(recoverPanic),
		),
		unaryLogger,
	)
}

func streamChain() grpc.StreamServerInterceptor {
	return middleware.ChainStreamServer(
		grpc_recovery.StreamServerInterceptor(
			grpc_recovery.WithRecoveryHandler(recoverPanic),
		),
		streamLogger,
	)
}

// UnaryInterceptor returns the unary interceptor
func UnaryInterceptor() grpc.ServerOption {
	return grpc.UnaryInterceptor(unaryChain())
}

// StreamInterceptor returns the stream interceptor with a logrus log
func StreamInterceptor() grpc.ServerOption {
	return grpc.StreamInterceptor(streamChain())
}
