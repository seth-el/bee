package bee

import (
	"context"
	"time"
)

// contextWithOptionalTimeout preserves the parent context while treating a
// zero/unset timeout as unlimited. Explicit positive timeouts remain bounded;
// callers retain their own startup, task-budget, tool, and frame limits.
func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
