package aistudio

import (
	"context"
	"sync"
)

type remoteAuthBudgetKey struct{}
type remoteAuthBudget struct {
	mu        sync.Mutex
	operation string
}

// WithRemoteAuthBudget preserves one budget across worker init, RPC replay,
// account attempts and continuation rounds of the same logical request.
func WithRemoteAuthBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(remoteAuthBudgetKey{}).(*remoteAuthBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, remoteAuthBudgetKey{}, &remoteAuthBudget{})
}

func TakeRemoteAuthBudget(ctx context.Context, operation string) error {
	budget, ok := ctx.Value(remoteAuthBudgetKey{}).(*remoteAuthBudget)
	if !ok || !ValidSessionOperationID(operation) {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.operation != "" && budget.operation != operation {
		return SessionError("RECOVERY_BUDGET_EXHAUSTED")
	}
	budget.operation = operation
	return nil
}
