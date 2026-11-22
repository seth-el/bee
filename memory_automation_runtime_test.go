package bee

import (
	"context"
	"testing"
)

type gatedMemoryExtractor struct {
	release chan struct{}
	result  MemoryExtractionResult
}

func (extractor gatedMemoryExtractor) Extract(ctx context.Context, _ TurnMemoryInput) (MemoryExtractionResult, error) {
	select {
	case <-extractor.release:
		return extractor.result, nil
	case <-ctx.Done():
		return MemoryExtractionResult{}, ctx.Err()
	}
}

// Memory extraction runs after the reply: the run's events end (and the
// stream closes) while extraction is still in progress, and its outcome then
// reaches /status instead of the run stream.
func TestCompletedRunInvokesBoundedMemoryExtractionWithoutChangingResponse(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	config.Memory.Automation.Enabled = true
	config.Memory.Automation.AutoSaveSafe = true
	config.Memory.Automation.TimeoutSeconds = 30
	app, err := NewApplication(config, &fakeProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := newLifecycleTestService(t)
	app.MemoryService = service
	app.MemoryActor = testMemoryParent()
	app.MemoryPolicy = localMemoryPolicy()
	release := make(chan struct{})
	app.MemoryExtractor = gatedMemoryExtractor{release: release, result: MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{automationCandidate("preference", "concise technical answers")}}}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "I prefer concise answers"})
	if err != nil {
		t.Fatal(err)
	}
	var sawEnd bool
	var sessionID SessionID
	for event := range events {
		if event.Type == RunMemoryExtraction {
			t.Fatal("memory extraction is no longer part of the run stream")
		}
		if event.Type == RunEnd {
			sawEnd = true
			sessionID = event.SessionID
			if event.Response == nil || event.Response.Message.Content != "reply" {
				t.Fatalf("run response = %#v", event.Response)
			}
		}
	}
	if !sawEnd {
		t.Fatal("run did not end while extraction was still pending")
	}
	close(release)
	app.WaitPostTurnWork()
	list, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent()})
	if err != nil || len(list.Records) != 1 {
		t.Fatalf("memory records = %#v, err=%v", list, err)
	}
	telemetry := app.commandStatusTelemetry(sessionID)
	if !telemetry.memoryExtractionSeen || telemetry.memoryExtraction.Saved != 1 {
		t.Fatalf("status telemetry = %#v", telemetry.memoryExtraction)
	}
}
