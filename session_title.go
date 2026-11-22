package bee

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Automatic session titles (G8). After a session's first completed turn,
// an untitled session gets a short title from the child model: one bounded,
// tool-free call, made at most once per session. A title set any other way
// (the owner, a job) is never touched, and a failure leaves the session
// untitled. A channel's placeholder title ("Telegram · Nov 16", marked
// TitleSourceProvisional) is replaced, keeping its label: "Telegram · Disk
// usage on beta". `bee sessions title-backfill` titles older sessions the
// same way.

const (
	TitleSourceAuto        = "auto"
	TitleSourceAutoFailed  = "auto_failed"
	TitleSourceProvisional = "provisional"

	maxAutoTitleBytes      = 60
	maxAutoTitleWords      = 12
	maxAutoTitleInputBytes = 1500
	autoTitleTimeoutSecs   = 60
	autoTitleMaxTokens     = 48
	// provisionalTitleSeparator splits a placeholder's label from its date.
	provisionalTitleSeparator = " · "
)

const autoTitleSystemPrompt = `Write a title for the conversation that starts with the exchange below.
Two to six words, in the language of the conversation, naming its subject.
Reply with the title only: no quotes, no trailing punctuation, no emoji, no prefix like "Title:".`

// AutoTitleEnabled reports whether automatic session titles are on.
func (config AssistantConfig) AutoTitleEnabled() bool {
	return config.AutoTitleSessions == nil || *config.AutoTitleSessions
}

// provisionalTitleSource is the TitleSource for a new session: provisional
// when the creator marked a non-empty title as a placeholder.
func provisionalTitleSource(title string, provisional bool) string {
	if provisional && strings.TrimSpace(title) != "" {
		return TitleSourceProvisional
	}
	return ""
}

// autoTitleWanted reports whether Bee may still title the session itself:
// untitled and never tried, or holding a channel placeholder.
func autoTitleWanted(metadata SessionMetadata) bool {
	if metadata.TitleSource == TitleSourceProvisional {
		return true
	}
	return strings.TrimSpace(metadata.Title) == "" && metadata.TitleSource == ""
}

// autoTitleAvailable reports whether this application can make the call:
// titles on, and a child profile distinct from the main one (a
// single-provider install would spend main-model time on it).
func (app *Application) autoTitleAvailable() bool {
	return app != nil && app.Config.Assistant.AutoTitleEnabled() && app.ChildProviderProfile != "" && !sameProviderConfig(app.ChildConfigOrMain().Provider, app.Config.Provider)
}

// maybeAutoTitle titles session from this turn's exchange when it has no
// title and Bee has never tried before. It calls the child model directly
// (like recon_review), so it does not depend on the specialist service. It
// never fails the run.
func (app *Application) maybeAutoTitle(ctx context.Context, session *Session, prompt, assistant string) {
	if session == nil || !app.autoTitleAvailable() || !autoTitleWanted(session.Metadata) {
		return
	}
	title, ok, attempted := app.generateSessionTitle(ctx, prompt, assistant)
	if !attempted {
		return
	}
	_ = applyAutoTitle(session, title, ok, time.Time{})
}

// generateSessionTitle makes the one child-model call. attempted is false
// when nothing was learned (no input, child busy, or ctx cancelled), so the
// caller should try again later rather than record a failure.
func (app *Application) generateSessionTitle(ctx context.Context, prompt, assistant string) (title string, ok, attempted bool) {
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(assistant) == "" {
		return "", false, false
	}
	provider, err := app.childProvider()
	if err != nil || provider == nil {
		return "", false, false
	}
	model := strings.TrimSpace(app.ChildConfigOrMain().Provider.Model)
	if model == "" {
		return "", false, false
	}
	callCtx, cancel := context.WithTimeout(ctx, autoTitleTimeoutSecs*time.Second)
	defer cancel()
	var lease *LocalChildLease
	if app.ChildBroker != nil {
		if lease, err = app.ChildBroker.Acquire(callCtx, LocalChildPurposeSessionTitle, LocalChildPriorityBackground); err != nil {
			// No call was made (the child is busy or the run ended): try next turn.
			return "", false, false
		}
	}
	input := "USER:\n" + truncateUTF8(strings.TrimSpace(prompt), maxAutoTitleInputBytes) + "\n\nASSISTANT:\n" + truncateUTF8(strings.TrimSpace(assistant), maxAutoTitleInputBytes)
	response, err := provider.Complete(callCtx, Request{
		Model:           model,
		Messages:        []Message{{Role: RoleSystem, Content: autoTitleSystemPrompt}, {Role: RoleUser, Content: input}},
		MaxTokens:       autoTitleMaxTokens,
		DisableThinking: true,
	})
	if lease != nil {
		lease.ReleaseError(err)
	}
	if err != nil && ctx.Err() != nil {
		// The run itself was cancelled; nothing was learned, try next turn.
		return "", false, false
	}
	if err != nil {
		return "", false, true
	}
	title, ok = cleanAutoTitle(response.Message.Content)
	return title, ok, true
}

// applyAutoTitle records the outcome. A placeholder keeps its label and,
// on failure, its title. at stamps the metadata event (zero means now).
func applyAutoTitle(session *Session, title string, ok bool, at time.Time) error {
	metadata := session.Metadata
	if ok {
		if metadata.TitleSource == TitleSourceProvisional {
			if label, _, found := strings.Cut(metadata.Title, provisionalTitleSeparator); found && strings.TrimSpace(label) != "" {
				title = strings.TrimSpace(label) + provisionalTitleSeparator + title
			}
		}
		metadata.Title = title
		metadata.TitleSource = TitleSourceAuto
	} else {
		metadata.TitleSource = TitleSourceAutoFailed
	}
	return session.replaceMetadataAt(metadata, at)
}

// cleanAutoTitle turns a model reply into a bounded one-line title, or
// rejects it.
func cleanAutoTitle(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if line, _, found := strings.Cut(text, "\n"); found {
		text = strings.TrimSpace(line)
	}
	for {
		before := text
		text = strings.Trim(text, " \t\"'`*_#“”‘’«»")
		text = strings.TrimRight(text, ".!?:;,")
		lower := strings.ToLower(text)
		for _, prefix := range []string{"title:", "title -"} {
			if strings.HasPrefix(lower, prefix) {
				text = text[len(prefix):]
				break
			}
		}
		if text == before {
			break
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	var cleaned strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F || r == 0x200D {
			continue
		}
		cleaned.WriteRune(r)
	}
	text = strings.TrimSpace(redactString(cleaned.String()))
	if text == "" || !utf8.ValidString(text) || len(strings.Fields(text)) > maxAutoTitleWords || strings.Contains(text, "[REDACTED]") {
		return "", false
	}
	if len(text) > maxAutoTitleBytes {
		cut := truncateUTF8(text, maxAutoTitleBytes)
		if space := strings.LastIndexByte(cut, ' '); space > maxAutoTitleBytes/2 {
			cut = cut[:space]
		}
		text = strings.TrimRight(cut, " ,;:-")
	}
	return text, true
}

// TitleBackfillItem is one session's backfill outcome.
type TitleBackfillItem struct {
	ID SessionID `json:"id"`
	// Outcome is "would_title" (dry run), "titled", "failed" (the model's
	// reply was unusable; recorded so it isn't retried), "busy" (a run or
	// the child held it; try again later), "skipped" (titled meanwhile) or
	// "no_exchange" (no completed user/assistant pair).
	Outcome string `json:"outcome"`
	Title   string `json:"title,omitempty"`
}

// TitleBackfillReport summarizes a backfill.
type TitleBackfillReport struct {
	Applied    bool                `json:"applied"`
	Candidates int                 `json:"candidates"`
	Items      []TitleBackfillItem `json:"items"`
}

// BackfillSessionTitles titles sessions that predate automatic titles (and
// Telegram placeholders), from each session's first exchange. A dry run lists
// the candidates without calling the model. With apply it makes one child
// call per session, holding the session's turn lease so it never races a
// run, and stamps the title at the session's last event time so the listing
// order doesn't change. limit <= 0 means no limit; progress, if set, sees
// each item as it is decided.
func (app *Application) BackfillSessionTitles(ctx context.Context, apply bool, limit int, progress func(TitleBackfillItem)) (TitleBackfillReport, error) {
	report := TitleBackfillReport{Applied: apply}
	if app == nil || app.Store == nil {
		return report, errors.New("a session store is required")
	}
	if apply && !app.autoTitleAvailable() {
		return report, errors.New("automatic titles need assistant.auto_title_sessions on and a child profile distinct from the main one")
	}
	summaries, err := app.Store.List()
	if err != nil {
		return report, err
	}
	record := func(item TitleBackfillItem) {
		report.Items = append(report.Items, item)
		if progress != nil {
			progress(item)
		}
	}
	for _, summary := range summaries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if limit > 0 && report.Candidates >= limit {
			break
		}
		session, err := app.Store.Load(summary.ID)
		if err != nil || !autoTitleWanted(session.Metadata) {
			continue
		}
		prompt, assistant := firstSessionExchange(session.EventsSnapshot())
		if prompt == "" || assistant == "" {
			continue
		}
		report.Candidates++
		if !apply {
			record(TitleBackfillItem{ID: summary.ID, Outcome: "would_title"})
			continue
		}
		record(app.backfillOneTitle(ctx, summary.ID))
	}
	return report, nil
}

func (app *Application) backfillOneTitle(ctx context.Context, id SessionID) TitleBackfillItem {
	item := TitleBackfillItem{ID: id}
	lease, err := app.Store.acquireTurnLease(id)
	if err != nil {
		item.Outcome = "busy"
		return item
	}
	defer lease.Close()
	// Reload under the lease: the session may have changed since listing.
	session, err := app.Store.Load(id)
	if err != nil || !autoTitleWanted(session.Metadata) {
		item.Outcome = "skipped"
		return item
	}
	events := session.EventsSnapshot()
	prompt, assistant := firstSessionExchange(events)
	if prompt == "" || assistant == "" {
		item.Outcome = "no_exchange"
		return item
	}
	title, ok, attempted := app.generateSessionTitle(ctx, prompt, assistant)
	if !attempted {
		item.Outcome = "busy"
		return item
	}
	var at time.Time
	if len(events) > 0 {
		at = events[len(events)-1].At
	}
	if err := applyAutoTitle(session, title, ok, at); err != nil {
		item.Outcome = "failed"
		return item
	}
	item.Title = session.Metadata.Title
	if ok {
		item.Outcome = "titled"
	} else {
		item.Outcome = "failed"
	}
	return item
}

// firstSessionExchange returns the first user prompt and the first
// non-empty assistant reply after it.
func firstSessionExchange(events []Event) (prompt, assistant string) {
	messages, err := MessagesFromEvents(events)
	if err != nil {
		return "", ""
	}
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		switch {
		case prompt == "" && message.Role == RoleUser && content != "":
			prompt = content
		case prompt != "" && message.Role == RoleAssistant && content != "":
			return prompt, content
		}
	}
	return "", ""
}
