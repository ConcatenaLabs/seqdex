package grpc_interceptor

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The requests whose work grows with an account: its coins (a balance, a list, a coin
// selection, a spend that selects its coins) or its derived addresses (a list of them, each
// derived again). The daemon runs at most heavyParallel of them at once, whatever the number of
// clients: after a restart every maker and the swap daemon ask at the same moment, and run all
// at once they took the daemon past its memory limit again (seqdex #181).
var heavyMethods = map[string]bool{
	"/ocean.v1.AccountService/ListAddresses":   true,
	"/ocean.v1.AccountService/Balance":         true,
	"/ocean.v1.AccountService/ListUtxos":       true,
	"/ocean.v1.TransactionService/SelectUtxos": true,
	"/ocean.v1.TransactionService/Transfer":    true,
	"/ocean.v1.TransactionService/Mint":        true,
	"/ocean.v1.TransactionService/Burn":        true,
}

var (
	heavyParallel = 16
	heavyWait     = 30 * time.Second
	heavySlots    = make(chan struct{}, heavyParallel)
)

// SetHeavyRequestBound sets how many of the heavy requests run at once (n, at least 1) and how
// long one waits for its turn (wait) before it is refused with codes.ResourceExhausted, which a
// client retries. Called before the server starts.
func SetHeavyRequestBound(n int, wait time.Duration) {
	if n < 1 {
		n = 1
	}
	if wait <= 0 {
		wait = time.Second
	}
	heavyParallel, heavyWait = n, wait
	heavySlots = make(chan struct{}, n)
}

// boundHeavy runs a heavy request once one of the slots is free. A request waits while its
// caller is there, at most heavyWait; then it is refused, never left hanging.
func boundHeavy(
	ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (interface{}, error) {
	if !heavyMethods[info.FullMethod] {
		return handler(ctx, req)
	}
	slots := heavySlots
	select {
	case slots <- struct{}{}:
	default:
		t := time.NewTimer(heavyWait)
		select {
		case slots <- struct{}{}:
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-t.C:
			return nil, status.Error(codes.ResourceExhausted, fmt.Sprintf(
				"the wallet is busy: %d requests that read an account are running and this one waited %s; try again",
				cap(slots), heavyWait))
		}
	}
	defer func() { <-slots }()
	return handler(ctx, req)
}
