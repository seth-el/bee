package bee

import "testing"

func TestShouldAutoRecallMemoryCueTable(t *testing.T) {
	for _, test := range []struct {
		prompt string
		want   bool
	}{
		{prompt: "Do you remember which inn we chose?", want: true},
		{prompt: "What did we decide about the rollout?", want: true},
		{prompt: "What were we working on yesterday?", want: true},
		{prompt: "You said earlier that this was safe — what did you mean?", want: true},
		{prompt: "What do you know about me?", want: true},
		{prompt: "Remind me what we agreed to do.", want: true},
		{prompt: "What happened last time?", want: true},
		{prompt: "We discussed this previously.", want: true},
		{prompt: "Please remember what we decided about the rollout.", want: true},
		{prompt: "Remember to buy milk.", want: false},
		{prompt: "Remember to use my usual editor.", want: true},
		{prompt: "Do you remember to use tabs for this project?", want: true},
		{prompt: "Can you explain memory allocation?", want: true},
		{prompt: "Please remember this recipe for later.", want: false},
		{prompt: "Tell me about the project.", want: true},
		{prompt: "Could you recommend a monitor for my desk?", want: true},
		{prompt: "hello", want: false},
		{prompt: "sounds good", want: false},
		{prompt: "go ahead", want: false},
		{prompt: "/status", want: false},
		{prompt: "Explain the project without using memory.", want: false},
	} {
		t.Run(test.prompt, func(t *testing.T) {
			if got := ShouldAutoRecallMemory(test.prompt); got != test.want {
				t.Fatalf("ShouldAutoRecallMemory(%q) = %v, want %v", test.prompt, got, test.want)
			}
		})
	}
}

func TestShouldAutoRecallConversationUsesOnlyBackwardReferences(t *testing.T) {
	for _, test := range []struct {
		prompt string
		want   bool
	}{
		{prompt: "What did we discuss about the rollout?", want: true},
		{prompt: "Continue where we left off", want: true},
		{prompt: "You mentioned earlier that this was cached", want: true},
		{prompt: "Tell me about rollout options", want: false},
		{prompt: "What do you know about my preferences?", want: false},
		{prompt: "What did we discuss? Don't use prior conversations.", want: false},
	} {
		if got := ShouldAutoRecallConversation(test.prompt); got != test.want {
			t.Fatalf("ShouldAutoRecallConversation(%q) = %v, want %v", test.prompt, got, test.want)
		}
	}
}

func TestHybridMemorySelectionInjectsOnlyRelevantOrdinaryHits(t *testing.T) {
	hits := []MemoryDisplayHit{
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "relevant"}}, Score: MemoryScore{Total: 0.5, Value: 0.5}},
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "weak"}}, Score: MemoryScore{Total: 0.2, Value: 0.2}},
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "semantic"}}, Score: MemoryScore{Total: 0.3, Value: 0.3, Semantic: 0.8}},
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "unscored"}}},
	}
	selected := selectHybridMemoryHits("Recommend a monitor", hits)
	if len(selected) != 2 || selected[0].Record.Record.ID != "relevant" || selected[1].Record.Record.ID != "semantic" {
		t.Fatalf("ordinary selected hits = %#v", selected)
	}
	selected = selectHybridMemoryHits("What did we decide about the monitor?", hits)
	if len(selected) != len(hits) {
		t.Fatalf("explicit selected hits = %#v", selected)
	}
}

func TestHybridMemoryRetrievalIsTightlyBounded(t *testing.T) {
	policy := DefaultMemoryPolicy()
	options := boundHybridMemoryRetrieval(MemoryRetrievalOptions{MaxResults: policy.MaxContextRecords, MaxBytes: policy.MaxContextBytes}, policy)
	if options.MaxResults != hybridMemoryMaxRecords || options.MaxBytes != hybridMemoryMaxBytes {
		t.Fatalf("hybrid bounds = %#v", options)
	}
}

func TestShouldAutoRecallMemoryIsCaseInsensitiveAndBounded(t *testing.T) {
	if !ShouldAutoRecallMemory("DO YOU REMEMBER what we chose?") {
		t.Fatal("uppercase recall cue was not detected")
	}
	prompt := "Do you remember" + string(make([]byte, maxMemoryRecallCueBytes+100))
	if !ShouldAutoRecallMemory(prompt) {
		t.Fatal("cue at the bounded prefix was not detected")
	}
	if ShouldAutoRecallMemory(string(make([]byte, maxMemoryRecallCueBytes+100)) + " previously") {
		t.Fatal("cue beyond the bounded prefix was detected")
	}
}

func TestNormalizeMemoryRecallQueryRemovesConversationalCues(t *testing.T) {
	for _, test := range []struct {
		prompt string
		want   string
	}{
		{prompt: "What did we decide about the rollout?", want: "rollout"},
		{prompt: "Do you remember which inn we chose?", want: "inn"},
		{prompt: "Remind me what we agreed to do about dark mode", want: "dark mode"},
		{prompt: "What is my usual editor?", want: "usual editor"},
		{prompt: "Could you recommend a monitor for my desk?", want: "monitor desk"},
	} {
		if got := NormalizeMemoryRecallQuery(test.prompt); got != test.want {
			t.Fatalf("NormalizeMemoryRecallQuery(%q) = %q, want %q", test.prompt, got, test.want)
		}
	}
}
