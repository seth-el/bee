package bee

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestMediaRetrieveFollowUpSelectsOnePriorRankThreeArtifact(t *testing.T) {
	searchClient := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "Blackstone Island one", "url": "https://example.com/one", "content": "Blackstone Island", "source": "example"},
		{"title": "Blackstone Island two", "url": "https://example.com/two", "content": "Blackstone Island", "source": "example"},
		{"title": "Blackstone Island three", "url": "https://example.com/three", "content": "Blackstone Island", "source": "example"},
	}})
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "search", Type: "function", Name: "media_search", Arguments: `{"kind":"video","query":"Blackstone Island"}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "I found three candidates."}},
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "retrieve", Type: "function", Name: MediaRetrieveToolName, Arguments: `{"rank":3}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "The selected video artifact is attached."}},
	}}
	store, artifacts, sessionID := newMediaTestStore(t)
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	media := NewMediaService(artifacts)
	media.CommandRun = &fakeMediaRunner{}
	media.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	app.Media = media
	app.MediaSearch = NewMediaSearchService(searchClient)
	registry, err := NewRegistry(MediaSearchTool{Service: app.MediaSearch}, MediaRetrieveTool{Search: app.MediaSearch, Media: app.Media})
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = NewExecutor(registry, ExecutionScope{Root: t.TempDir()})

	first, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "find Blackstone Island videos", MediaEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for range first {
	}
	second, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "How about the Blackstone Island one?", MediaEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var artifactsSeen, progressSeen int
	var retrieveOutput string
	for event := range second {
		switch event.Type {
		case RunArtifact:
			if event.Artifact != nil {
				artifactsSeen++
				if event.Artifact.Filename != "video.mp4" || event.Artifact.MediaType != "video/mp4" {
					t.Fatalf("unexpected artifact=%#v", event.Artifact)
				}
			}
		case RunMediaProgress:
			progressSeen++
		case RunToolResult:
			if event.ToolResult != nil && event.ToolResult.ToolName == MediaRetrieveToolName {
				retrieveOutput = event.ToolResult.Output
			}
		case RunFailure:
			t.Fatalf("follow-up failed: %s", event.Error)
		}
	}
	if artifactsSeen != 1 || progressSeen == 0 {
		t.Fatalf("artifacts=%d progress=%d", artifactsSeen, progressSeen)
	}
	var result MediaRetrieveModelResult
	if err := json.Unmarshal([]byte(retrieveOutput), &result); err != nil {
		t.Fatalf("retrieve output=%q err=%v", retrieveOutput, err)
	}
	if result.Status != "attached" || result.Rank != 3 || result.ArtifactCount != 1 {
		t.Fatalf("retrieve result=%#v", result)
	}
	for _, forbidden := range []string{"https://", "http://", "\"t_", "\"c_", "/tmp/"} {
		if strings.Contains(retrieveOutput, forbidden) {
			t.Fatalf("follow-up output leaked %q: %s", forbidden, retrieveOutput)
		}
	}
	if len(provider.requests) < 3 || !containsToolDefinition(provider.requests[2].Tools, MediaRetrieveToolName) {
		t.Fatalf("media_retrieve was not exposed to the follow-up model call: requests=%d", len(provider.requests))
	}
	stored, err := artifacts.List(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored artifacts=%#v", stored)
	}
}

func containsToolDefinition(definitions []ToolDefinition, name string) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}
