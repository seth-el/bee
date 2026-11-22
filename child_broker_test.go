package bee

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLocalChildBrokerBoundsCancellationPriorityAndNoDoubleRelease(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 4})
	if err != nil {
		t.Fatal(err)
	}
	first, err := broker.Acquire(context.Background(), LocalChildPurposeChild, LocalChildPriorityNormal)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := broker.Acquire(ctx, LocalChildPurposeMemoryExtraction, LocalChildPriorityBackground); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation = %v", err)
	}
	background := make(chan *LocalChildLease, 1)
	high := make(chan *LocalChildLease, 1)
	go func() {
		lease, _ := broker.Acquire(context.Background(), LocalChildPurposeMemoryExtraction, LocalChildPriorityBackground)
		background <- lease
	}()
	go func() {
		lease, _ := broker.Acquire(context.Background(), LocalChildPurposeReconReview, LocalChildPriorityHigh)
		high <- lease
	}()
	time.Sleep(10 * time.Millisecond)
	first.Release()
	select {
	case lease := <-high:
		if lease == nil {
			t.Fatal("high priority lease is nil")
		}
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("high priority request did not run")
	}
	select {
	case lease := <-background:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("background request did not run")
	}
	snapshot := broker.Snapshot()
	if snapshot.Capacity != 1 || snapshot.InFlight != 0 || snapshot.Cancellations != 1 || snapshot.Completed != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	first.Release()
	if broker.Snapshot().Completed != 3 {
		t.Fatal("double release changed completion count")
	}
}

func TestLocalChildBrokerAccountsFailuresWaitAndLatency(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 4})
	if err != nil {
		t.Fatal(err)
	}
	first, err := broker.Acquire(context.Background(), LocalChildPurposeChild, LocalChildPriorityNormal)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan *LocalChildLease, 1)
	go func() {
		lease, _ := broker.Acquire(context.Background(), LocalChildPurposeMemoryExtraction, LocalChildPriorityBackground)
		queued <- lease
	}()
	time.Sleep(120 * time.Millisecond)
	first.ReleaseError(errors.New("provider failed"))
	select {
	case lease := <-queued:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("queued request did not run")
	}
	snapshot := broker.Snapshot()
	if snapshot.FailedCalls != 1 || snapshot.QueueWaitNanos == 0 || snapshot.LatencyNanos == 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestLocalChildBrokerAgesBackgroundThroughNormalToHigh(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 16})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	var clockMu sync.Mutex
	broker.mu.Lock()
	broker.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	broker.fairnessAfter = time.Second
	broker.mu.Unlock()
	first, err := broker.Acquire(context.Background(), LocalChildPurposeChild, LocalChildPriorityHigh)
	if err != nil {
		t.Fatal(err)
	}
	backgroundCh := make(chan *LocalChildLease, 1)
	go func() {
		lease, acquireErr := broker.Acquire(context.Background(), LocalChildPurposeMemoryRerank, LocalChildPriorityBackground)
		if acquireErr == nil {
			backgroundCh <- lease
		}
	}()
	waitForQueued := func(want int) {
		deadline := time.Now().Add(time.Second)
		for broker.Snapshot().Queued < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if broker.Snapshot().Queued < want {
			t.Fatalf("test requests were not queued: want %d, got %d", want, broker.Snapshot().Queued)
		}
	}
	waitForQueued(1)
	highCh := make(chan *LocalChildLease, 4)
	for i := 0; i < 4; i++ {
		go func() {
			lease, acquireErr := broker.Acquire(context.Background(), LocalChildPurposeReconReview, LocalChildPriorityHigh)
			if acquireErr == nil {
				highCh <- lease
			}
		}()
		waitForQueued(i + 2)
	}
	clockMu.Lock()
	clock = clock.Add(2 * time.Second)
	clockMu.Unlock()
	first.Release()
	select {
	case aged := <-backgroundCh:
		if aged == nil {
			t.Fatal("aged background request returned nil lease")
		}
		aged.Release()
	case <-time.After(time.Second):
		t.Fatal("background request starved despite deterministic aging")
	}
	for i := 0; i < 4; i++ {
		select {
		case high := <-highCh:
			high.Release()
		case <-time.After(time.Second):
			t.Fatal("queued high request did not complete")
		}
	}
	if snapshot := broker.Snapshot(); snapshot.Cancellations != 0 || snapshot.InFlight != 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestLocalChildBrokerZeroTimeoutUsesParentContextOnly(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := broker.Acquire(ctx, LocalChildPurposeSpecialist, LocalChildPriorityNormal)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	cancel()
	if _, err := broker.Acquire(ctx, LocalChildPurposeSpecialist, LocalChildPriorityNormal); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context = %v", err)
	}
}
