package bee

import (
	"context"
	"errors"
	"time"
)

type RetrySleep func(context.Context, time.Duration) error

type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Sleep       RetrySleep
	Retryable   func(error) bool
}

func (policy RetryPolicy) Run(ctx context.Context, operation func(context.Context) error) error {
	if ctx == nil {
		return errors.New("retry context is nil")
	}
	if operation == nil {
		return errors.New("retry operation is nil")
	}
	attempts := policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	sleep := policy.Sleep
	if sleep == nil {
		sleep = retrySleep
	}
	delay := policy.BaseDelay
	if delay < 0 {
		delay = 0
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := operation(ctx)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if policy.Retryable == nil || !policy.Retryable(err) || attempt+1 >= attempts {
			return err
		}
		if policy.MaxDelay > 0 && delay > policy.MaxDelay {
			delay = policy.MaxDelay
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		if delay > 0 && delay < policy.MaxDelay {
			delay *= 2
		}
	}
	return nil
}

func retrySleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
