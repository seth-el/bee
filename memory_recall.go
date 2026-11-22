package bee

import "strings"

const maxMemoryRecallCueBytes = 4096

const (
	hybridMemoryMaxRecords       = 8
	hybridMemoryMaxBytes         = 16 << 10
	hybridMemoryMinTotalScore    = 0.45
	hybridMemoryMinLexicalScore  = 0.15
	hybridMemoryMinSemanticScore = 0.65
)

// ShouldAutoRecallMemory reports whether a prompt is substantive enough for a
// bounded hybrid memory lookup. Hybrid recall deliberately searches broadly
// and lets relevance filtering decide whether any result reaches the prompt.
// Only control-like, acknowledgement, write-memory, and explicit opt-out
// prompts are skipped.
func ShouldAutoRecallMemory(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if prompt == "" {
		return false
	}
	if len(prompt) > maxMemoryRecallCueBytes {
		prompt = prompt[:maxMemoryRecallCueBytes]
	}
	if strings.HasPrefix(prompt, "/") || memoryRecallOptOut(prompt) {
		return false
	}
	if IsExplicitMemoryRecall(prompt) {
		return true
	}
	if memoryWriteIntent(prompt) {
		return false
	}
	tokens := memoryTokens(prompt)
	if len(tokens) == 0 {
		return false
	}
	switch strings.Join(tokens, " ") {
	case "hi", "hello", "hey", "thanks", "thank you", "okay", "ok", "got it",
		"sounds good", "perfect", "great", "nice", "yes", "yes please", "no",
		"go ahead", "please do", "do it", "proceed", "never mind", "nevermind":
		return false
	default:
		return true
	}
}

// IsExplicitMemoryRecall identifies direct requests to use prior durable or
// conversational knowledge. Explicit requests accept unscored compatibility
// results that broad hybrid recall would otherwise suppress.
func IsExplicitMemoryRecall(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if prompt == "" || memoryRecallOptOut(prompt) {
		return false
	}
	if len(prompt) > maxMemoryRecallCueBytes {
		prompt = prompt[:maxMemoryRecallCueBytes]
	}
	for _, cue := range []string{
		"do you remember",
		"what did we decide",
		"what were we working on",
		"you said earlier",
		"you mentioned earlier",
		"what do you know about me",
		"remind me what",
		"last time",
		"previously",
		"we decided",
		"we agreed",
		"we chose",
		"continue where we left",
		"pick up where we left",
		"based on what you know about me",
		"based on my preferences",
		"what do i prefer",
		"which do i prefer",
		"my usual",
		"as usual",
	} {
		if strings.Contains(prompt, cue) {
			return true
		}
	}
	return false
}

// ShouldAutoRecallConversation is narrower than durable-memory recall. Old
// conversation snippets are injected only when the prompt explicitly refers
// to prior discussion or unfinished work.
func ShouldAutoRecallConversation(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if prompt == "" || memoryRecallOptOut(prompt) {
		return false
	}
	if len(prompt) > maxMemoryRecallCueBytes {
		prompt = prompt[:maxMemoryRecallCueBytes]
	}
	for _, cue := range []string{
		"do you remember", "what did we decide", "what were we working on",
		"what did we discuss", "what did we talk about", "you said earlier",
		"you mentioned earlier", "remind me what", "last time", "previously",
		"we decided", "we agreed", "we chose", "continue where we left",
		"pick up where we left", "prior conversation", "previous conversation",
		"earlier conversation", "previous session", "prior session",
	} {
		if strings.Contains(prompt, cue) {
			return true
		}
	}
	return false
}

func memoryRecallOptOut(prompt string) bool {
	for _, cue := range []string{
		"don't use memory", "do not use memory", "without using memory",
		"don't search memory", "do not search memory", "ignore saved memory",
		"don't use prior conversations", "do not use prior conversations",
	} {
		if strings.Contains(prompt, cue) {
			return true
		}
	}
	return false
}

func memoryWriteIntent(prompt string) bool {
	for _, cue := range []string{
		"remember to", "please remember this", "please remember that",
		"remember this for", "save this to memory", "store this in memory",
	} {
		if strings.Contains(prompt, cue) {
			return true
		}
	}
	return false
}

func hybridMemoryHitRelevant(hit MemoryDisplayHit) bool {
	score := hit.Score
	return score.Phrase > 0 || score.Lexical >= hybridMemoryMinLexicalScore ||
		score.Semantic >= hybridMemoryMinSemanticScore || score.Total >= hybridMemoryMinTotalScore
}

// selectHybridMemoryHits separates broad search from prompt injection. Direct
// recall accepts the bounded service result; ordinary prompts keep only
// confidently relevant scored hits.
func selectHybridMemoryHits(prompt string, hits []MemoryDisplayHit) []MemoryDisplayHit {
	if IsExplicitMemoryRecall(prompt) {
		return append([]MemoryDisplayHit(nil), hits...)
	}
	selected := make([]MemoryDisplayHit, 0, len(hits))
	for _, hit := range hits {
		if hybridMemoryHitRelevant(hit) {
			selected = append(selected, hit)
		}
	}
	return selected
}

func boundHybridMemoryRetrieval(options MemoryRetrievalOptions, policy MemoryPolicy) MemoryRetrievalOptions {
	maxRecords := minInt(policy.MaxContextRecords, hybridMemoryMaxRecords)
	maxBytes := minInt(policy.MaxContextBytes, hybridMemoryMaxBytes)
	if options.MaxResults == 0 || options.MaxResults > maxRecords {
		options.MaxResults = maxRecords
	}
	if options.ResultLimit > maxRecords {
		options.ResultLimit = maxRecords
	}
	if options.MaxBytes == 0 || options.MaxBytes > maxBytes {
		options.MaxBytes = maxBytes
	}
	return options
}

// NormalizeMemoryRecallQuery removes recall boilerplate and low-information
// words before hybrid prompt-based retrieval. Explicit memory_search calls are
// left untouched because the model or caller has already chosen its query.
func NormalizeMemoryRecallQuery(prompt string) string {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if len(prompt) > MaxMemoryQueryTextLength {
		prompt = prompt[:MaxMemoryQueryTextLength]
	}
	for _, cue := range []string{
		"do you remember", "what did we decide", "what were we working on",
		"what did we discuss", "what did we talk about", "you said earlier",
		"you mentioned earlier", "what do you know about me",
		"remind me what", "what happened last time", "last time", "previously",
		"we decided", "we agreed", "we chose", "continue where we left off",
		"continue where we left", "pick up where we left off", "pick up where we left",
		"based on what you know about me", "based on my preferences",
	} {
		prompt = strings.ReplaceAll(prompt, cue, " ")
	}
	stop := map[string]struct{}{
		"a": {}, "about": {}, "an": {}, "and": {}, "are": {}, "as": {},
		"at": {}, "be": {}, "can": {}, "could": {}, "did": {}, "do": {},
		"explain": {}, "for": {}, "from": {}, "give": {}, "help": {},
		"how": {}, "i": {}, "in": {}, "is": {}, "it": {}, "me": {},
		"my": {}, "of": {}, "on": {}, "or": {}, "our": {}, "please": {},
		"recommend": {}, "show": {}, "tell": {}, "that": {}, "the": {},
		"this": {}, "to": {}, "want": {}, "was": {}, "we": {}, "were": {},
		"what": {}, "when": {}, "which": {}, "with": {}, "would": {}, "you": {},
	}
	tokens := memoryTokens(prompt)
	filtered := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, skip := stop[token]; skip {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		filtered = append(filtered, token)
	}
	if len(filtered) == 0 {
		return strings.TrimSpace(prompt)
	}
	return strings.Join(filtered, " ")
}
