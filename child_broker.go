package bee

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// LocalChildPurpose identifies a parent-owned call that consumes capacity at
// the configured child/specialist endpoint.
type LocalChildPurpose string

const (
	LocalChildPurposeChild              LocalChildPurpose = "child"
	LocalChildPurposeSpecialist         LocalChildPurpose = "specialist"
	LocalChildPurposeReconReview        LocalChildPurpose = "recon_review"
	LocalChildPurposeMemoryExtraction   LocalChildPurpose = "memory_extraction"
	LocalChildPurposeEvidenceExtraction LocalChildPurpose = "evidence_extraction"
	LocalChildPurposeClaimVerification  LocalChildPurpose = "claim_verification"
	LocalChildPurposeMemoryRerank       LocalChildPurpose = "memory_rerank"
	LocalChildPurposeWebResearch        LocalChildPurpose = "web_research"
	LocalChildPurposeSessionTitle       LocalChildPurpose = "session_title"
)

// LocalChildPriority is ordered from highest to lowest. FIFO ordering is
// preserved within a priority lane.
type LocalChildPriority int

const (
	LocalChildPriorityBackground LocalChildPriority = 0
	LocalChildPriorityNormal     LocalChildPriority = 1
	LocalChildPriorityHigh       LocalChildPriority = 2
)

const (
	DefaultLocalChildBrokerCapacity = MaxBoundedChildInstances
	DefaultLocalChildBrokerQueue    = 32
	MaxLocalChildBrokerCapacity     = 32
	MaxLocalChildBrokerQueue        = 1024
	localChildFairnessAfter         = 100 * time.Millisecond
	localChildFairnessMaxPromotions = int(LocalChildPriorityHigh - LocalChildPriorityBackground)
)

// LocalChildBrokerConfig controls the shared parent-owned endpoint broker.
// Disabled preserves the pre-broker direct-call behavior for compatibility.
type LocalChildBrokerConfig struct {
	Enabled        bool `json:"enabled"`
	MaxConcurrency int  `json:"max_concurrency"`
	MaxQueue       int  `json:"max_queue"`
}

func DefaultLocalChildBrokerConfig() LocalChildBrokerConfig {
	return LocalChildBrokerConfig{Enabled: true, MaxConcurrency: DefaultLocalChildBrokerCapacity, MaxQueue: DefaultLocalChildBrokerQueue}
}

func (config LocalChildBrokerConfig) effective() LocalChildBrokerConfig {
	defaults := DefaultLocalChildBrokerConfig()
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = defaults.MaxConcurrency
	}
	if config.MaxQueue <= 0 {
		config.MaxQueue = defaults.MaxQueue
	}
	return config
}

func (config LocalChildBrokerConfig) Validate() error {
	if config.MaxConcurrency < 0 || config.MaxConcurrency > MaxLocalChildBrokerCapacity {
		return errors.New("child broker max_concurrency is invalid")
	}
	if config.MaxQueue < 0 || config.MaxQueue > MaxLocalChildBrokerQueue {
		return errors.New("child broker max_queue is invalid")
	}
	return nil
}

type LocalChildBrokerSnapshot struct {
	Enabled        bool                       `json:"enabled"`
	Capacity       int                        `json:"capacity"`
	InFlight       int                        `json:"in_flight"`
	Queued         int                        `json:"queued"`
	Acquisitions   uint64                     `json:"acquisitions"`
	Cancellations  uint64                     `json:"cancellations"`
	Completed      uint64                     `json:"completed"`
	FailedCalls    uint64                     `json:"failed_calls"`
	QueueWaitNanos uint64                     `json:"queue_wait_nanos"`
	LatencyNanos   uint64                     `json:"latency_nanos"`
	ByPurpose      map[LocalChildPurpose]int  `json:"by_purpose,omitempty"`
	ByPriority     map[LocalChildPriority]int `json:"by_priority,omitempty"`
}

type localChildRequest struct {
	purpose  LocalChildPurpose
	priority LocalChildPriority
	seq      uint64
	queuedAt time.Time
	ready    chan struct{}
	stop     chan struct{}
	granted  bool
	canceled bool
}

// LocalChildCapacityBroker is a parent-owned scheduler, not a provider and not
// a runner pool. Callers acquire once around one endpoint operation and release
// exactly once. Broker-aware operations must not call one another while holding
// a lease; that boundary prevents reentrant deadlocks.
type LocalChildCapacityBroker struct {
	mu             sync.Mutex
	config         LocalChildBrokerConfig
	inFlight       int
	seq            uint64
	queue          []*localChildRequest
	byPurpose      map[LocalChildPurpose]int
	byPriority     map[LocalChildPriority]int
	acquisitions   uint64
	cancellations  uint64
	completed      uint64
	failedCalls    uint64
	queueWaitNanos uint64
	latencyNanos   uint64
	now            func() time.Time
	fairnessAfter  time.Duration
}

func NewLocalChildCapacityBroker(config LocalChildBrokerConfig) (*LocalChildCapacityBroker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config = config.effective()
	broker := &LocalChildCapacityBroker{config: config, byPurpose: make(map[LocalChildPurpose]int), byPriority: make(map[LocalChildPriority]int), now: time.Now, fairnessAfter: localChildFairnessAfter}
	return broker, nil
}

func (broker *LocalChildCapacityBroker) Acquire(ctx context.Context, purpose LocalChildPurpose, priority LocalChildPriority) (*LocalChildLease, error) {
	if broker == nil {
		return nil, errors.New("child broker is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	broker.mu.Lock()
	if !broker.config.Enabled {
		broker.mu.Unlock()
		return &LocalChildLease{}, nil
	}
	if len(broker.queue) >= broker.config.MaxQueue && broker.inFlight >= broker.config.MaxConcurrency {
		broker.mu.Unlock()
		return nil, errors.New("child broker queue is full")
	}
	broker.seq++
	req := &localChildRequest{purpose: purpose, priority: priority, seq: broker.seq, queuedAt: broker.clockNow(), ready: make(chan struct{}), stop: make(chan struct{})}
	broker.queue = append(broker.queue, req)
	broker.dispatchLocked()
	broker.mu.Unlock()

	go broker.cancelOnContext(ctx, req)
	<-req.ready
	if req.canceled {
		return nil, ctx.Err()
	}
	return &LocalChildLease{broker: broker, purpose: purpose, acquiredAt: broker.clockNow()}, nil
}

func (broker *LocalChildCapacityBroker) cancelOnContext(ctx context.Context, req *localChildRequest) {
	select {
	case <-ctx.Done():
		broker.mu.Lock()
		if !req.granted && !req.canceled {
			req.canceled = true
			broker.removeLocked(req)
			broker.cancellations++
			close(req.ready)
			broker.dispatchLocked()
		}
		broker.mu.Unlock()
	case <-req.stop:
	}
}

func (broker *LocalChildCapacityBroker) dispatchLocked() {
	if !broker.config.Enabled {
		for _, req := range broker.queue {
			req.granted = true
			close(req.ready)
		}
		broker.queue = nil
		return
	}
	for broker.inFlight < broker.config.MaxConcurrency && len(broker.queue) > 0 {
		sort.SliceStable(broker.queue, func(i, j int) bool {
			left := broker.effectivePriorityLocked(broker.queue[i])
			right := broker.effectivePriorityLocked(broker.queue[j])
			if left != right {
				return left > right
			}
			return broker.queue[i].seq < broker.queue[j].seq
		})
		req := broker.queue[0]
		broker.queue = broker.queue[1:]
		if req.canceled {
			continue
		}
		req.granted = true
		close(req.stop)
		broker.inFlight++
		broker.acquisitions++
		broker.byPurpose[req.purpose]++
		broker.byPriority[req.priority]++
		broker.queueWaitNanos += uint64(broker.clockNow().Sub(req.queuedAt))
		close(req.ready)
	}
}

func (broker *LocalChildCapacityBroker) removeLocked(target *localChildRequest) {
	for i, req := range broker.queue {
		if req == target {
			broker.queue = append(broker.queue[:i], broker.queue[i+1:]...)
			return
		}
	}
}

func (broker *LocalChildCapacityBroker) release(purpose LocalChildPurpose) {
	broker.releaseOutcome(purpose, time.Time{}, false)
}

func (broker *LocalChildCapacityBroker) releaseOutcome(purpose LocalChildPurpose, acquiredAt time.Time, failed bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if !broker.config.Enabled {
		return
	}
	if broker.inFlight > 0 {
		broker.inFlight--
	}
	if broker.byPurpose[purpose] > 0 {
		broker.byPurpose[purpose]--
	}
	broker.completed++
	if failed {
		broker.failedCalls++
	}
	if !acquiredAt.IsZero() {
		broker.latencyNanos += uint64(broker.clockNow().Sub(acquiredAt))
	}
	broker.dispatchLocked()
}

func (broker *LocalChildCapacityBroker) clockNow() time.Time {
	if broker != nil && broker.now != nil {
		return broker.now()
	}
	return time.Now()
}

func (broker *LocalChildCapacityBroker) effectivePriorityLocked(req *localChildRequest) LocalChildPriority {
	priority := req.priority
	if req.queuedAt.IsZero() {
		return priority
	}
	threshold := broker.fairnessAfter
	if threshold <= 0 {
		threshold = localChildFairnessAfter
	}
	age := broker.clockNow().Sub(req.queuedAt)
	if age <= 0 {
		return priority
	}
	promotions := int(age / threshold)
	if promotions > localChildFairnessMaxPromotions {
		promotions = localChildFairnessMaxPromotions
	}
	priority += LocalChildPriority(promotions)
	if priority > LocalChildPriorityHigh {
		priority = LocalChildPriorityHigh
	}
	return priority
}

func (broker *LocalChildCapacityBroker) Snapshot() LocalChildBrokerSnapshot {
	if broker == nil {
		return LocalChildBrokerSnapshot{}
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	purpose := make(map[LocalChildPurpose]int, len(broker.byPurpose))
	for key, value := range broker.byPurpose {
		purpose[key] = value
	}
	priority := make(map[LocalChildPriority]int, len(broker.byPriority))
	for key, value := range broker.byPriority {
		priority[key] = value
	}
	return LocalChildBrokerSnapshot{Enabled: broker.config.Enabled, Capacity: broker.config.MaxConcurrency, InFlight: broker.inFlight, Queued: len(broker.queue), Acquisitions: broker.acquisitions, Cancellations: broker.cancellations, Completed: broker.completed, FailedCalls: broker.failedCalls, QueueWaitNanos: broker.queueWaitNanos, LatencyNanos: broker.latencyNanos, ByPurpose: purpose, ByPriority: priority}
}

type LocalChildLease struct {
	broker     *LocalChildCapacityBroker
	purpose    LocalChildPurpose
	once       sync.Once
	acquiredAt time.Time
}

func (lease *LocalChildLease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.broker != nil {
			lease.broker.releaseOutcome(lease.purpose, lease.acquiredAt, false)
		}
	})
}

// ReleaseError records the provider outcome while releasing the lease.
func (lease *LocalChildLease) ReleaseError(err error) {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.broker != nil {
			lease.broker.releaseOutcome(lease.purpose, lease.acquiredAt, err != nil)
		}
	})
}

func (broker *LocalChildCapacityBroker) queueWait(ctx context.Context, purpose LocalChildPurpose, priority LocalChildPriority) (*LocalChildLease, time.Duration, error) {
	started := time.Now()
	lease, err := broker.Acquire(ctx, purpose, priority)
	return lease, time.Since(started), err
}
