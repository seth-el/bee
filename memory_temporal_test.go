package bee

import (
	"context"
	"testing"
	"time"
)

func TestM5TemporalCurrentAndRangeFilters(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	active := rankedRecord("active", "durable_memory/bee", "routing", "current routing", .9, base)
	active.Status = MemoryStatusActive
	historical := rankedRecord("historical", "durable_memory/bee", "routing", "old routing", .9, base.Add(-48*time.Hour))
	historical.Status = MemoryStatusHistorical
	superseded := rankedRecord("superseded", "durable_memory/bee", "routing", "superseded routing", .9, base.Add(-24*time.Hour))
	superseded.Status = MemoryStatusSuperseded
	superseded.SupersededBy = active.ID
	future := rankedRecord("future", "durable_memory/bee", "routing", "future routing", .9, base.Add(24*time.Hour))
	appendRankedRecords(t, store, active, historical, superseded, future)

	current, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Namespace: "durable_memory/bee", Text: "routing", CurrentOnly: true, AsOf: base}, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Hits) != 1 || current.Hits[0].Record.ID != active.ID {
		t.Fatalf("current=%#v", current)
	}

	asOf, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Namespace: "durable_memory/bee", Text: "routing", AsOf: base.Add(-36 * time.Hour)}, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(asOf.Hits) != 1 || asOf.Hits[0].Record.ID != historical.ID {
		t.Fatalf("as-of=%#v", asOf)
	}

	since, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Namespace: "durable_memory/bee", Text: "routing", Since: base.Add(-30 * time.Hour), Before: base.Add(12 * time.Hour)}, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(since.Hits) != 2 || since.Hits[0].Record.ID == historical.ID {
		t.Fatalf("range=%#v", since)
	}
}

func TestM5SupersededRecordsRemainHistoricallyRetrievable(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	old := rankedRecord("old", "durable_memory/bee", "routing", "old routing", .9, at.Add(-time.Hour))
	old.Status = MemoryStatusSuperseded
	old.SupersededBy = "new"
	newRecord := rankedRecord("new", "durable_memory/bee", "routing", "new routing", .9, at)
	newRecord.Status = MemoryStatusActive
	appendRankedRecords(t, store, old, newRecord)
	all, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Namespace: "durable_memory/bee", Text: "routing", AsOf: at}, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Hits) != 2 {
		t.Fatalf("historical result=%#v", all)
	}
	current, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Namespace: "durable_memory/bee", Text: "routing", CurrentOnly: true, AsOf: at}, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Hits) != 1 || current.Hits[0].Record.ID != "new" {
		t.Fatalf("current result=%#v", current)
	}
}
