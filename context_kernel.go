package bee

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrContextWindowExceeded = errors.New("request exceeds configured context window")

const prunedToolResultMarker = "[older tool result omitted from request projection; durable transcript retained]"

type RequestContextUsage struct {
	WindowTokens       int    `json:"window_tokens"`
	WindowKnown        bool   `json:"window_known"`
	SystemTokens       int    `json:"system_tokens"`
	HistoryTokens      int    `json:"history_tokens"`
	ToolResultTokens   int    `json:"tool_result_tokens"`
	AttachmentTokens   int    `json:"attachment_tokens"`
	ToolSchemaTokens   int    `json:"tool_schema_tokens"`
	ReservedOutput     int    `json:"reserved_output_tokens"`
	ReservedRecovery   int    `json:"reserved_recovery_tokens"`
	PromptBudget       int    `json:"prompt_budget"`
	PromptUsed         int    `json:"prompt_used"`
	PromptRemaining    int    `json:"prompt_remaining"`
	OverBudget         bool   `json:"over_budget"`
	PrunedToolResults  int    `json:"pruned_tool_results"`
	PrefixStableTokens int    `json:"prefix_stable_tokens"`
	PrefixStableBytes  int    `json:"prefix_stable_bytes,omitempty"`
	PrefixChanged      bool   `json:"prefix_changed"`
	PrefixChangeReason string `json:"prefix_change_reason,omitempty"`
	CacheEpoch         uint64 `json:"cache_epoch,omitempty"`
	// Prefix component hashes are omitted from status and gateway JSON. Durable
	// sessions retain them only in the content-free cache_state event so a fresh
	// Agent can compare its first request with the preceding run.
	PrefixFingerprint  string `json:"-"`
	PrefixModelHash    string `json:"-"`
	PrefixTemplateHash string `json:"-"`
	PrefixToolsHash    string `json:"-"`
	PrefixHistoryHash  string `json:"-"`
}

func estimateToolDefinitions(definitions []ToolDefinition) int {
	data, err := json.Marshal(requestTools(definitions))
	if err != nil {
		return 0
	}
	return (len(data) + 3) / 4
}

func classifyRequestUsage(messages []Message, definitions []ToolDefinition, window, output, reserve int) RequestContextUsage {
	usage := RequestContextUsage{WindowTokens: window, WindowKnown: window > 0, ReservedOutput: output, ReservedRecovery: reserve, ToolSchemaTokens: estimateToolDefinitions(definitions)}
	for _, message := range messages {
		estimate := EstimateMessageSize(message)
		switch {
		case message.Role == RoleSystem:
			usage.SystemTokens += estimate
		case message.Role == RoleTool:
			usage.ToolResultTokens += estimate
		case len(message.Attachments) > 0:
			usage.AttachmentTokens += estimate + estimateAttachmentPayloadTokens(message.Attachments)
		default:
			usage.HistoryTokens += estimate
		}
	}
	usage.PromptUsed = usage.SystemTokens + usage.HistoryTokens + usage.ToolResultTokens + usage.AttachmentTokens + usage.ToolSchemaTokens
	if window > 0 {
		usage.PromptBudget = window - output - reserve
		if usage.PromptBudget < 0 {
			usage.PromptBudget = 0
		}
		usage.PromptRemaining = usage.PromptBudget - usage.PromptUsed
		if usage.PromptRemaining < 0 {
			usage.PromptRemaining = 0
		}
		usage.OverBudget = usage.PromptUsed > usage.PromptBudget
	}
	return usage
}

func estimateAttachmentPayloadTokens(attachments []AttachmentRef) int {
	total := 0
	for _, attachment := range attachments {
		if !isImageAttachmentRef(attachment) {
			continue
		}
		estimate := 2048
		if attachment.SizeBytes > 0 {
			estimate = 1024 + int((attachment.SizeBytes+8191)/8192)
			if estimate > 8192 {
				estimate = 8192
			}
		}
		total += estimate
	}
	return total
}

// projectToolResults bounds old observations without rewriting durable state.
// Tool messages remain paired with their assistant calls, preserving provider
// protocol validity and a byte-stable prefix until pressure requires pruning.
func projectToolResults(messages []Message, perResultTokens, targetTokens int) ([]Message, int) {
	projected := append([]Message(nil), messages...)
	pruned := 0
	currentTurn := len(projected)
	for index := len(projected) - 1; index >= 0; index-- {
		if projected[index].Role == RoleUser {
			currentTurn = index
			break
		}
	}
	// The newest canonical plan result is the append-only replacement for the
	// old mutable leading snapshot. Preserve it until compaction emits a
	// durable <bee-plan-state> checkpoint.
	protectedPlanResult := -1
	for index := currentTurn - 1; index >= 0; index-- {
		message := projected[index]
		if message.Role == RoleTool && (message.Name == PlanToolName || message.Name == PlanStatusToolName || message.Name == PlanUpdateToolName) && strings.Contains(message.Content, `"fingerprint":"`) {
			protectedPlanResult = index
			break
		}
	}
	for index := 0; index < currentTurn; index++ {
		message := &projected[index]
		if index == protectedPlanResult || message.Role != RoleTool || message.Content == prunedToolResultMarker {
			continue
		}
		if perResultTokens > 0 && EstimateMessageSize(*message) > perResultTokens {
			message.Content = prunedToolResultMarker
			pruned++
		}
	}
	// Never mutate additional historical observations merely to hit a target
	// budget. Once a result has appeared in a provider prefix, changing it
	// destroys append-only cache continuity. The caller may instead perform a
	// durable compaction, which creates an explicit new cache epoch.
	_ = targetTokens
	return projected, pruned
}

func stablePrefixFingerprint(messages []Message, definitions []ToolDefinition) (string, int, []byte) {
	// Everything preceding the most recent user message is expected to remain
	// byte-identical as a session grows. Current-turn context follows that user.
	boundary := len(messages)
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == RoleUser {
			boundary = index
			break
		}
	}
	var data []byte
	toolData, _ := json.Marshal(requestTools(definitions))
	data = append(data, toolData...)
	data = append(data, '\n')
	for _, message := range requestMessages(messages[:boundary]) {
		encoded, _ := json.Marshal(message)
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), (len(data) + 3) / 4, bytes.Clone(data)
}

type cachePrefixComponents struct {
	Model    string
	Template string
	Tools    string
	History  string
}

func fingerprintJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func stableRequestPrefixComponents(request Request) cachePrefixComponents {
	boundary := len(request.Messages)
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == RoleUser {
			boundary = index
			break
		}
	}
	return cachePrefixComponents{
		Model:    fingerprintJSON(request.Model),
		Template: fingerprintJSON(chatTemplateKwargs(request)),
		Tools:    fingerprintJSON(requestTools(request.Tools)),
		History:  fingerprintJSON(requestMessages(request.Messages[:boundary])),
	}
}

func stableRequestPrefixFingerprint(request Request) (string, int, []byte) {
	// Only prompt-rendering settings belong ahead of the tool/message prefix.
	// Sampling, output bounds, and tool_choice constrain generation without
	// changing the reusable prompt tokens on OpenAI-compatible providers.
	shape := struct {
		Model              string         `json:"model"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}{
		Model:              request.Model,
		ChatTemplateKwargs: chatTemplateKwargs(request),
	}
	shapeData, _ := json.Marshal(shape)
	_, _, stable := stablePrefixFingerprint(request.Messages, request.Tools)
	data := append(shapeData, '\n')
	data = append(data, stable...)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), (len(data) + 3) / 4, bytes.Clone(data)
}

func (agent *Agent) compactContext(ctx context.Context, automatic bool) error {
	return agent.compactContextKeeping(ctx, automatic, agent.config.Context.KeepRecentTurns)
}

// compactContextKeeping compacts everything before the current turn except
// the newest keep completed turns (fewer when there is not enough history).
func (agent *Agent) compactContextKeeping(ctx context.Context, automatic bool, keep int) error {
	if agent == nil || agent.session == nil {
		return errors.New("context compaction requires a durable session")
	}
	events := agent.session.EventsSnapshot()
	through := compactionBoundaryKeeping(events, compactionBoundaryBeforeCurrentTurn(events), keep)
	if through == 0 {
		return errors.New("no completed earlier turn is available for compaction")
	}
	prefix := ""
	if automatic {
		prefix = "overflow-recovery/"
	}
	return agent.compactContextThrough(ctx, events, through, prefix)
}

// compactCompletedHistory is used before a new user message is committed. It
// compacts only through the latest complete assistant answer, so admission can
// recover space without first creating an unmatched durable user turn.
func (agent *Agent) compactCompletedHistory(ctx context.Context, automatic bool) error {
	return agent.compactCompletedHistoryKeeping(ctx, automatic, agent.config.Context.KeepRecentTurns)
}

func (agent *Agent) compactCompletedHistoryKeeping(ctx context.Context, automatic bool, keep int) error {
	if agent == nil || agent.session == nil {
		return errors.New("context compaction requires a durable session")
	}
	events := agent.session.EventsSnapshot()
	through := compactionBoundaryKeeping(events, completedHistoryBoundary(events), keep)
	if through == 0 {
		return errors.New("no completed earlier turn is available for compaction")
	}
	prefix := ""
	if automatic {
		prefix = "overflow-recovery/"
	}
	return agent.compactContextThrough(ctx, events, through, prefix)
}

func completedHistoryBoundary(events []Event) uint64 {
	var latestUser uint64
	var boundary uint64
	for _, event := range events {
		if event.Type != EventMessage {
			continue
		}
		payload, err := DecodeMessageEvent(event)
		if err != nil {
			continue
		}
		switch payload.Message.Role {
		case RoleUser:
			latestUser = event.Seq
			boundary = 0
		case RoleAssistant:
			if event.Seq > latestUser && !payload.Message.Incomplete && len(payload.Message.ToolCalls) == 0 {
				boundary = event.Seq
			}
		}
	}
	return boundary
}

func (agent *Agent) compactCompletedToolSegment(ctx context.Context) error {
	if agent == nil || agent.session == nil {
		return errors.New("segment compaction requires a durable session")
	}
	events := agent.session.EventsSnapshot()
	if len(events) == 0 {
		return errors.New("no completed segment is available for compaction")
	}
	through := events[len(events)-1].Seq
	return agent.compactContextThrough(ctx, events, through, "segment-boundary/")
}

func (agent *Agent) compactContextThrough(ctx context.Context, events []Event, through uint64, methodPrefix string) error {
	var source []Event
	for _, event := range events {
		if event.Seq <= through {
			source = append(source, event)
		}
	}
	items, err := contextItemsFromEvents(source)
	if err != nil {
		return err
	}
	// Clock controls are point-in-time tail state, not historical content.
	// Exclude them from both deterministic and configured summaries so dropping
	// pre-compaction time_sync events cannot preserve a stale clock indirectly.
	compactionItems := make([]ContextItem, 0, len(items))
	for _, item := range items {
		if item.Source != ContextSourceTimeSync {
			compactionItems = append(compactionItems, item)
		}
	}
	items = compactionItems
	limit := agent.config.Context.SummaryTokens
	if limit <= 0 {
		limit = DefaultContextSummaryTokens
	}
	var summary ContextItem
	method := compactionMethodDeterministic
	switch {
	case agent.contextBase.Summarizer != nil:
		summary, err = agent.contextBase.Summarizer.Summarize(ctx, items, limit)
		method = "configured-summarizer"
	case agent.config.Context.ModelSummary:
		// A failed model summary falls back to the deterministic one, so
		// overflow recovery never depends on the model call succeeding. The
		// method records the fallback; the failure text is not persisted.
		text, modelErr := agent.modelCompactionSummary(ctx, events, source, limit)
		if modelErr == nil {
			method = compactionMethodModel
		} else {
			if ctx.Err() != nil {
				return fmt.Errorf("compact context: %w", ctx.Err())
			}
			text = deterministicContextSummary(items, limit)
			method = compactionMethodModelFallback
		}
		summary = ContextItem{Source: ContextSourceSummary, Message: Message{Role: RoleSystem, Content: text}}
	default:
		summary = ContextItem{Source: ContextSourceSummary, Message: Message{Role: RoleSystem, Content: deterministicContextSummary(items, limit)}}
	}
	if err != nil {
		return fmt.Errorf("compact context: %w", err)
	}
	if strings.TrimSpace(summary.Message.Content) == "" {
		return errors.New("context compaction produced an empty summary")
	}
	sourceData, _ := json.Marshal(source)
	sourceHash := sha256.Sum256(sourceData)
	summaryHash := sha256.Sum256([]byte(summary.Message.Content))
	compaction := CompactionEvent{Through: through, Summary: summary.Message.Content, SourceSHA256: hex.EncodeToString(sourceHash[:]), SummarySHA256: hex.EncodeToString(summaryHash[:]), Provider: agent.config.Provider.Type, Profile: agent.config.ProviderProfile, Model: agent.config.Provider.Model, Method: method, CreatedAt: time.Now().UTC()}
	compaction.Method = methodPrefix + method
	if err := agent.session.CompactWithProvenance(compaction); err != nil {
		return err
	}
	result, err := AssembleContext(agent.session.EventsSnapshot(), ContextOptions{})
	if err != nil {
		return err
	}
	// Retain composition-owned leading system instructions and replace only the
	// replayed transcript projection.
	systems := compositionSystemMessages(agent.messages, events)
	agent.messages = append(systems, result.Messages...)
	// Compaction is an explicit cache discontinuity. Re-establish the current
	// control and plan state after the checkpoint by appending durable records;
	// never move mutable state back into the leading system prefix.
	return agent.appendCurrentPlanCheckpoint()
}

func compactionBoundaryBeforeCurrentTurn(events []Event) uint64 {
	var currentUser uint64
	for _, event := range events {
		if event.Type != EventMessage {
			continue
		}
		payload, err := DecodeMessageEvent(event)
		if err == nil && payload.Message.Role == RoleUser {
			currentUser = event.Seq
		}
	}
	var boundary uint64
	for _, event := range events {
		if event.Seq >= currentUser || event.Type != EventMessage {
			continue
		}
		if event.Seq > boundary {
			boundary = event.Seq
		}
	}
	if boundary == 0 {
		return 0
	}
	// Never split an assistant call from its tool results.
	for boundary > 0 {
		var candidate *Event
		for index := range events {
			if events[index].Seq == boundary {
				candidate = &events[index]
				break
			}
		}
		if candidate == nil || candidate.Type != EventToolCall {
			break
		}
		boundary--
	}
	return boundary
}

func deterministicContextSummary(items []ContextItem, maxTokens int) string {
	maxBytes := maxTokens * 4
	if maxBytes <= 0 {
		maxBytes = DefaultContextSummaryTokens * 4
	}
	const header = "Compacted conversation checkpoint (untrusted historical summary):\n"
	var lines []string
	total := len(header)
	for _, item := range items {
		if item.Message.Role == RoleTool {
			continue
		}
		content := strings.TrimSpace(item.Message.Content)
		if content == "" {
			continue
		}
		line := fmt.Sprintf("- %s: %s\n", item.Message.Role, content)
		lines = append(lines, line)
		total += len(line)
	}
	if total <= maxBytes {
		return strings.TrimSpace(header + strings.Join(lines, ""))
	}
	// Over budget: keep the start (how the conversation began, including any
	// earlier checkpoint) and, with twice the room, the end (what was most
	// recently going on), and mark what was left out between them.
	const markerReserve = 96
	room := maxBytes - len(header) - markerReserve
	if room < 0 {
		room = 0
	}
	headRoom := room / 3
	tailRoom := room - headRoom
	var head []string
	used := 0
	next := 0
	for ; next < len(lines); next++ {
		line := lines[next]
		if used+len(line) > headRoom {
			if remaining := headRoom - used; remaining > 32 {
				head = append(head, strings.TrimRight(truncateUTF8(line, remaining-1), "\n")+"\n")
				used = headRoom
				next++
			}
			break
		}
		head = append(head, line)
		used += len(line)
	}
	var tail []string
	used = 0
	last := len(lines) - 1
	for ; last >= next; last-- {
		line := lines[last]
		if used+len(line) > tailRoom {
			if remaining := tailRoom - used; remaining > 32 {
				tail = append(tail, strings.TrimRight(truncateUTF8(line, remaining-1), "\n")+"\n")
				last--
			}
			break
		}
		tail = append(tail, line)
		used += len(line)
	}
	var builder strings.Builder
	builder.WriteString(header)
	for _, line := range head {
		builder.WriteString(line)
	}
	if omitted := last - next + 1; omitted > 0 {
		fmt.Fprintf(&builder, "- [%d messages from the middle of the conversation omitted]\n", omitted)
	}
	for index := len(tail) - 1; index >= 0; index-- {
		builder.WriteString(tail[index])
	}
	return strings.TrimSpace(builder.String())
}

// CompactContext is the explicit/manual compaction surface.
func (agent *Agent) CompactContext(ctx context.Context) error {
	return agent.compactContext(ctx, false)
}

// VerifyCompaction proves that a checkpoint still matches the retained source
// prefix and its stored summary. Legacy checkpoints without hashes are
// reported as unverifiable rather than treated as corrupt.
func VerifyCompaction(events []Event, checkpoint Event) error {
	if checkpoint.Type != EventCompaction {
		return errors.New("event is not a compaction checkpoint")
	}
	var payload CompactionEvent
	if err := json.Unmarshal(checkpoint.Data, &payload); err != nil {
		return err
	}
	if payload.SourceSHA256 == "" || payload.SummarySHA256 == "" {
		return errors.New("legacy compaction checkpoint has no provenance hashes")
	}
	var source []Event
	for _, event := range events {
		if event.Seq <= payload.Through {
			source = append(source, event)
		}
	}
	data, _ := json.Marshal(source)
	sourceHash := sha256.Sum256(data)
	summaryHash := sha256.Sum256([]byte(payload.Summary))
	if hex.EncodeToString(sourceHash[:]) != payload.SourceSHA256 {
		return errors.New("compaction source hash mismatch")
	}
	if hex.EncodeToString(summaryHash[:]) != payload.SummarySHA256 {
		return errors.New("compaction summary hash mismatch")
	}
	return nil
}
