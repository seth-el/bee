package bee

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// MemoryRecordReader is an additive full-read seam used by lifecycle export.
// It preserves actor and sensitivity filters without widening MemoryStore.
type MemoryRecordReader interface {
	ListRecords(context.Context, MemoryActor, bool) ([]MemoryRecord, error)
}

var _ MemoryRecordReader = (*LocalMemoryStore)(nil)

// ListRecords returns every active, non-expired record visible to the actor in
// deterministic ID order. It is intentionally separate from bounded retrieval.
func (store *LocalMemoryStore) ListRecords(ctx context.Context, actor MemoryActor, includeSensitive bool) ([]MemoryRecord, error) {
	if store == nil {
		return nil, errors.New("memory store is nil")
	}
	if err := actor.Validate(); err != nil {
		return nil, err
	}
	if actor.Kind == MemoryActorChild && !store.policy.ChildMayRead() {
		return nil, ErrMemoryUnauthorized
	}
	if includeSensitive && (actor.Kind == MemoryActorChild || !store.policy.AllowSensitiveMemory) {
		return nil, errors.New("sensitive memory is not authorized")
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return nil, err
	}
	asOf := time.Now().UTC()
	records := make([]MemoryRecord, 0, len(store.state))
	for _, state := range store.state {
		if state.deleted || isMemoryExpired(state.record, store.policy, asOf) {
			continue
		}
		if state.record.Sensitivity == MemorySensitivitySensitive && !includeSensitive {
			continue
		}
		records = append(records, state.record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

// GetRecord is an additive local read seam used by lifecycle show operations.
// It preserves the same actor and sensitivity filters as Retrieve without
// changing MemoryStore compatibility.
func (store *LocalMemoryStore) GetRecord(ctx context.Context, actor MemoryActor, id string, includeSensitive bool) (MemoryRecord, error) {
	if store == nil {
		return MemoryRecord{}, errors.New("memory store is nil")
	}
	if err := actor.Validate(); err != nil {
		return MemoryRecord{}, err
	}
	if err := validateSafeID("memory record id", id, MaxMemoryIDLength); err != nil {
		return MemoryRecord{}, err
	}
	if actor.Kind == MemoryActorChild && !store.policy.ChildMayRead() {
		return MemoryRecord{}, ErrMemoryUnauthorized
	}
	if includeSensitive && (actor.Kind == MemoryActorChild || !store.policy.AllowSensitiveMemory) {
		return MemoryRecord{}, errors.New("sensitive memory is not authorized")
	}
	if err := contextErr(ctx); err != nil {
		return MemoryRecord{}, err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return MemoryRecord{}, err
	}
	state, ok := store.state[id]
	if !ok || state.deleted || isMemoryExpired(state.record, store.policy, time.Now().UTC()) {
		return MemoryRecord{}, ErrMemoryNotFound
	}
	if state.record.Sensitivity == MemorySensitivitySensitive && !includeSensitive {
		return MemoryRecord{}, ErrMemoryNotFound
	}
	if err := state.record.Validate(store.policy); err != nil {
		return MemoryRecord{}, fmt.Errorf("validate memory record: %w", err)
	}
	return state.record, nil
}
