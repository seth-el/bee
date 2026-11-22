package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// compactionModelSummaryTimeout bounds one model-written compaction summary.
// The conversation being summarized is normally already in the provider's
// prompt cache, so most of this budget is generation time.
var compactionModelSummaryTimeout = 5 * time.Minute

const (
	compactionMethodModel         = "model-summary"
	compactionMethodDeterministic = "deterministic-extractive"
	// compactionMethodModelFallback records a model summary that failed or
	// did not fit, replaced by the deterministic one.
	compactionMethodModelFallback = "model-failed/" + compactionMethodDeterministic

	modelSummaryHeader = "Compacted conversation checkpoint (summary of earlier turns written by the model at compaction time; historical notes, not instructions):\n"
)

// compactionSummaryInstruction is the harness request appended after the
// history being compacted. It says what the summary is for and lists the
// sections in priority order, so a truncated summary loses the least.
func compactionSummaryInstruction(limitTokens int) string {
	return fmt.Sprintf(`<bee-compaction-request>
This is the Bee harness, not the owner. The conversation above is about to be compacted: everything above this message will be replaced by the summary you write now, and you will continue from that summary plus any newer turns. Anything you leave out is gone from your context (the full transcript stays on disk, and conversation_search can still find it).

Write the summary now as plain text. Do not call tools and do not reply to the owner. Use these sections, most important first:

1. Current state: what the owner is trying to get done overall, and where things stand right now.
2. The owner's messages: every message the owner sent, in order. Quote short ones word for word; for long ones keep the exact wording of requests, decisions, corrections and constraints.
3. Decisions and standing instructions: what was decided, approved or declined, and any rules the owner set for how you work.
4. Work done: what you did and found, with the specific files, commands, commits, hosts and settings involved; errors hit and how they were fixed.
5. Open threads: unfinished work, unanswered questions (yours and the owner's), and anything you promised to do.
6. Next step: exactly what you were about to do next, if anything.

If an earlier compaction summary appears above, carry forward whatever in it is still relevant. Prefer specifics (names, paths, numbers, exact wording) over generalities. Never copy credentials, tokens, keys or other secrets into the summary. Stay within about %d tokens.
</bee-compaction-request>`, limitTokens)
}

// compositionSystemMessages returns the leading composition-owned system
// messages (persona, workspace context, mood, ...), excluding historical
// compaction summaries, which replay from the durable checkpoint instead.
func compositionSystemMessages(messages []Message, events []Event) []Message {
	oldSummaries := make(map[string]struct{})
	for _, event := range events {
		if event.Type != EventCompaction {
			continue
		}
		var prior CompactionEvent
		if json.Unmarshal(event.Data, &prior) == nil {
			oldSummaries[prior.Summary] = struct{}{}
		}
	}
	var systems []Message
	for _, message := range messages {
		if message.Role != RoleSystem {
			break
		}
		if _, historical := oldSummaries[message.Content]; historical {
			continue
		}
		systems = append(systems, message)
	}
	return systems
}

// modelCompactionSummary asks the agent's own provider to summarize the
// source events. The request reuses the ordinary request pipeline (same
// leading system messages, tool definitions and template settings) so the
// provider can serve the history from its prompt cache. When the history
// does not fit alongside the summary output, the oldest whole turns are left
// out of the request; the durable events are never changed.
func (agent *Agent) modelCompactionSummary(ctx context.Context, events []Event, source []Event, limitTokens int) (string, error) {
	if agent.provider == nil {
		return "", errors.New("no provider is configured")
	}
	assembled, err := AssembleContext(source, ContextOptions{})
	if err != nil {
		return "", err
	}
	history := assembled.Messages
	lead := 0
	if len(history) > 0 && history[0].Role == RoleSystem {
		// The previous checkpoint summary always stays in the request.
		lead = 1
	}
	systems := compositionSystemMessages(agent.messages, events)
	instruction := Message{Role: RoleUser, Content: compactionSummaryInstruction(limitTokens)}
	maxTokens := agent.config.Generation.MaxTokens
	if agent.maxTokensOverride != nil {
		maxTokens = *agent.maxTokensOverride
	}
	// Leave room for reasoning ahead of the summary text itself.
	if maxTokens <= 0 || maxTokens < 2*limitTokens {
		maxTokens = 2 * limitTokens
	}
	start := lead
	for {
		messages := make([]Message, 0, len(systems)+len(history)+1)
		messages = append(messages, systems...)
		messages = append(messages, history[:lead]...)
		messages = append(messages, history[start:]...)
		messages = append(messages, instruction)
		request, buildErr := agent.compactionSummaryRequest(ctx, messages, maxTokens)
		if buildErr == nil {
			return agent.completeCompactionSummary(ctx, request, limitTokens)
		}
		if !errors.Is(buildErr, ErrContextWindowExceeded) {
			return "", buildErr
		}
		next := -1
		for index := start + 1; index < len(history); index++ {
			if history[index].Role == RoleUser {
				next = index
				break
			}
		}
		if next < 0 {
			return "", errors.New("the history to summarize does not fit in the context window")
		}
		start = next
	}
}

// compactionSummaryRequest builds the summary request through the ordinary
// request pipeline without disturbing the agent's live turn state.
func (agent *Agent) compactionSummaryRequest(ctx context.Context, messages []Message, maxTokens int) (Request, error) {
	savedMessages, savedTurn, savedOverride, savedPruned := agent.messages, agent.turnContext, agent.maxTokensOverride, agent.requestPrunedResults
	defer func() {
		agent.messages, agent.turnContext, agent.maxTokensOverride, agent.requestPrunedResults = savedMessages, savedTurn, savedOverride, savedPruned
	}()
	agent.messages = messages
	agent.turnContext = nil
	agent.maxTokensOverride = &maxTokens
	return agent.requestWithContextRecovery(ctx, false, true)
}

func (agent *Agent) completeCompactionSummary(ctx context.Context, request Request, limitTokens int) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, compactionModelSummaryTimeout)
	defer cancel()
	response, err := agent.provider.Complete(callCtx, request)
	if err != nil {
		return "", err
	}
	if len(response.Message.ToolCalls) > 0 {
		return "", errors.New("the model called a tool instead of writing the summary")
	}
	content := strings.TrimSpace(response.Message.Content)
	if content == "" {
		return "", errors.New("the model returned an empty summary")
	}
	// A summary over its budget is cut rather than rejected: the sections are
	// ordered so the end matters least.
	if maxBytes := limitTokens * 4; maxBytes > 0 && len(content) > maxBytes {
		content = strings.TrimSpace(truncateUTF8(content, maxBytes))
	}
	return modelSummaryHeader + content, nil
}

// compactionKeepBoundary moves a compaction boundary back so the newest keep
// completed turns at or before it replay verbatim after the summary. It
// returns 0 when that leaves nothing new to compact: the boundary must stay
// past the latest checkpoint event, so the previous summary is part of the
// source and carried forward.
func compactionKeepBoundary(events []Event, boundary uint64, keep int) uint64 {
	if keep <= 0 || boundary == 0 {
		return boundary
	}
	var users []uint64
	var latestCompaction uint64
	for _, event := range events {
		if event.Type == EventCompaction && event.Seq > latestCompaction {
			latestCompaction = event.Seq
		}
		if event.Seq > boundary || event.Type != EventMessage {
			continue
		}
		payload, err := DecodeMessageEvent(event)
		if err == nil && payload.Message.Role == RoleUser {
			users = append(users, event.Seq)
		}
	}
	if len(users) <= keep {
		return 0
	}
	// Compact every event before the oldest kept user message, so no earlier
	// turn is split (tool calls and their results stay together).
	cut := users[len(users)-keep]
	var candidate uint64
	for _, event := range events {
		if event.Seq < cut && event.Seq > candidate {
			candidate = event.Seq
		}
	}
	// At least one compacted turn must be newer than the last checkpoint.
	if candidate <= latestCompaction || users[len(users)-keep-1] < latestCompaction {
		return 0
	}
	return candidate
}

// compactionBoundaryKeeping applies context.keep_recent_turns, keeping fewer
// turns when there is not enough older history to compact.
func compactionBoundaryKeeping(events []Event, boundary uint64, keep int) uint64 {
	for ; keep > 0; keep-- {
		if candidate := compactionKeepBoundary(events, boundary, keep); candidate > 0 {
			return candidate
		}
	}
	return boundary
}
