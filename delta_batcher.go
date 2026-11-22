package bee

import (
	"context"
	"strings"
	"time"
)

// Assistant delta delivery is intentionally synchronous: provider callbacks
// never start a timer or goroutine, so a run cannot outlive a pending flush.
// The first non-empty delta is delivered immediately; later rapid deltas are
// coalesced until the bounded time/size threshold or the provider turn ends.
const (
	assistantDeltaBatchWindow   = 24 * time.Millisecond
	assistantDeltaBatchMaxBytes = 4 << 10
)

type assistantDeltaBatcher struct {
	ctx       context.Context
	emit      StreamFunc
	now       func() time.Time
	pending   strings.Builder
	lastFlush time.Time
	emitted   bool
}

func newAssistantDeltaBatcher(ctx context.Context, emit StreamFunc) *assistantDeltaBatcher {
	return &assistantDeltaBatcher{ctx: ctx, emit: emit, now: time.Now}
}

func (batcher *assistantDeltaBatcher) Add(delta string) error {
	if delta == "" || batcher.emit == nil {
		return nil
	}
	if err := batcher.contextErr(); err != nil {
		return err
	}
	now := batcher.now()
	if !batcher.emitted {
		if err := batcher.emit(delta); err != nil {
			return err
		}
		batcher.emitted = true
		batcher.lastFlush = now
		return nil
	}
	batcher.pending.WriteString(delta)
	if batcher.pending.Len() >= assistantDeltaBatchMaxBytes || now.Sub(batcher.lastFlush) >= assistantDeltaBatchWindow {
		return batcher.flushAt(now)
	}
	return nil
}

func (batcher *assistantDeltaBatcher) Flush() error {
	if batcher == nil || batcher.pending.Len() == 0 {
		return nil
	}
	if err := batcher.contextErr(); err != nil {
		batcher.pending.Reset()
		return err
	}
	return batcher.flushAt(batcher.now())
}

func (batcher *assistantDeltaBatcher) flushAt(now time.Time) error {
	if batcher.pending.Len() == 0 {
		batcher.lastFlush = now
		return nil
	}
	delta := batcher.pending.String()
	if err := batcher.emit(delta); err != nil {
		return err
	}
	batcher.pending.Reset()
	batcher.lastFlush = now
	return nil
}

func (batcher *assistantDeltaBatcher) contextErr() error {
	if batcher == nil || batcher.ctx == nil {
		return nil
	}
	select {
	case <-batcher.ctx.Done():
		return batcher.ctx.Err()
	default:
		return nil
	}
}
