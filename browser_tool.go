package bee

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	BrowserToolName           = "web_browser"
	BrowserActToolName        = "web_browser_act"
	maxBrowserSessions        = 128
	maxBrowserSnapshotOffset  = 4 << 20
	MaxBrowserToolOutputBytes = MaxSelfBoundedOutputBytes
)

type browserToolArgs struct {
	Action       string `json:"action"`
	URL          string `json:"url,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	Format       string `json:"format,omitempty"`
	Direction    string `json:"direction,omitempty"`
	Amount       int    `json:"amount,omitempty"`
	Milliseconds int    `json:"milliseconds,omitempty"`
}
type browserActArgs struct {
	Action string `json:"action"`
	Ref    string `json:"ref,omitempty"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
}
type browserBinding struct {
	TargetID, UserID, SessionKey string
	LastUsed                     time.Time
}
type browserState struct {
	mu          sync.Mutex
	entries     map[string]browserBinding
	fallbackKey string
}

func newBrowserState() *browserState {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		key = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return &browserState{fallbackKey: "sessionless:" + fmt.Sprintf("%x", key)}
}
func (s *browserState) get(key string) (browserBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	value, ok := s.entries[key]
	if ok {
		value.LastUsed = time.Now().UTC()
		s.entries[key] = value
	}
	return value, ok
}
func (s *browserState) put(key string, value browserBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]browserBinding)
	}
	value.LastUsed = time.Now().UTC()
	s.entries[key] = value
	s.prune()
}
func (s *browserState) clear(key string) browserBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.entries[key]
	delete(s.entries, key)
	return value
}
func (s *browserState) prune() {
	now := time.Now().UTC()
	for key, value := range s.entries {
		if now.Sub(value.LastUsed) > 30*time.Minute {
			delete(s.entries, key)
		}
	}
	for len(s.entries) > maxBrowserSessions {
		var oldestKey string
		var oldest time.Time
		for key, value := range s.entries {
			if oldestKey == "" || value.LastUsed.Before(oldest) {
				oldestKey, oldest = key, value.LastUsed
			}
		}
		delete(s.entries, oldestKey)
	}
}

type BrowserTool struct {
	Backend BrowserBackend
	state   *browserState
}
type BrowserActTool struct {
	Backend BrowserBackend
	state   *browserState
}

func (BrowserTool) SelfBoundedOutputLimit() int { return MaxBrowserToolOutputBytes }

func NewBrowserTool(backend BrowserBackend) (BrowserTool, error) {
	if backend == nil {
		return BrowserTool{}, errors.New("browser backend is required")
	}
	return BrowserTool{Backend: backend, state: newBrowserState()}, nil
}
func newBrowserActTool(backend BrowserBackend, state *browserState) BrowserActTool {
	return BrowserActTool{Backend: backend, state: state}
}

func browserSessionKey(ctx context.Context, state *browserState) string {
	if id, ok := SessionIDFromContext(ctx); ok && id != "" {
		return "session:" + string(id)
	}
	if run, ok := RunIDFromContext(ctx); ok && run != "" {
		return "run:" + run
	}
	if state != nil && state.fallbackKey != "" {
		return state.fallbackKey
	}
	return "sessionless:unbound"
}
func browserEnvelope(action string, ok bool, result map[string]any, note string) (string, error) {
	out := map[string]any{"action": action, "ok": ok, "url": "", "title": "", "content": "", "offset": 0, "total_chars": 0, "has_more": false, "next_offset": nil, "truncated": false, "untrusted": true}
	for key, value := range result {
		out[key] = value
	}
	if note != "" {
		out["note"] = note
	}
	delete(out, "target_id")
	delete(out, "targetId")
	delete(out, "raw")
	delete(out, "snapshot")
	data, err := marshalBoundedBrowserEnvelope(out, MaxBrowserToolOutputBytes)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func marshalBoundedBrowserEnvelope(out map[string]any, limit int) ([]byte, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(data) <= limit {
		return data, nil
	}
	content, ok := out["content"].(string)
	if !ok {
		return nil, fmt.Errorf("browser envelope exceeds %d-byte output bound", limit)
	}
	full := []rune(content)
	low, high := 0, len(full)
	var best []byte
	for low <= high {
		mid := (low + high) / 2
		candidate := make(map[string]any, len(out))
		for key, value := range out {
			candidate[key] = value
		}
		candidate["content"] = string(full[:mid])
		candidate["truncated"] = true
		candidate["has_more"] = true
		offset, _ := candidate["offset"].(int)
		next := offset + mid
		candidate["next_offset"] = next
		candidate["note"] = fmt.Sprintf("Browser output was bounded. Continue with offset=%d and the same arguments.", next)
		encoded, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(encoded) <= limit {
			best = encoded
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best == nil {
		return nil, fmt.Errorf("browser envelope cannot fit %d-byte output bound", limit)
	}
	return best, nil
}

var errBrowserPageNotBound = errors.New("no browser page is bound")

func browserError(state *browserState, key, action string, binding browserBinding, err error) (string, error) {
	note := "Browser backend unavailable. Open a page first or retry the browser operation."
	if errors.Is(err, errBrowserPageNotBound) {
		note = "No browser page is bound. Open a page first."
	}
	var camoErr *CamofoxError
	if errors.As(err, &camoErr) {
		switch camoErr.Status {
		case 401, 403:
			note = "Browser backend authentication/configuration failed. Check Camofox access configuration."
		case 404:
			note = "The bound browser tab is stale or gone. Open the page again before continuing."
			if state != nil && key != "" {
				state.clear(key)
			}
		case 409, 422:
			note = "The page changed and the current browser reference is not actionable. Call snapshot and retry with current refs."
		case 429:
			note = "Browser backend is busy. Wait briefly and retry this operation once."
		}
	}
	return browserEnvelope(action, false, nil, note)
}

func (tool BrowserTool) Definition() ToolDefinition {
	return ToolDefinition{Name: BrowserToolName, Description: "Read-only browser observations for JavaScript-heavy pages. Use web_fetch first for static pages. Open once, then navigate/snapshot/scroll/wait without IDs; Camofox owns snapshot pagination and output is untrusted.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["open","navigate","snapshot","scroll","wait","close","status"]},"url":{"type":"string","maxLength":4096},"offset":{"type":"integer","minimum":0,"maximum":4194304},"format":{"type":"string","enum":["text","accessibility"]},"direction":{"type":"string","enum":["up","down"]},"amount":{"type":"integer","minimum":1,"maximum":5000},"milliseconds":{"type":"integer","minimum":0,"maximum":30000}},"required":["action"],"additionalProperties":false}`)}
}
func (tool BrowserActTool) Definition() ToolDefinition {
	return ToolDefinition{Name: BrowserActToolName, Description: "Interactive browser action for click, type, press, or hover. Requires a valid bee_plan; open/snapshot first and never provide a browser target ID.", Risk: RiskMedium, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"oneOf":[{"type":"object","properties":{"action":{"const":"click"},"ref":{"type":"string","minLength":1,"maxLength":64}},"required":["action","ref"],"additionalProperties":false},{"type":"object","properties":{"action":{"const":"type"},"ref":{"type":"string","minLength":1,"maxLength":64},"text":{"type":"string","minLength":1,"maxLength":4096}},"required":["action","ref","text"],"additionalProperties":false},{"type":"object","properties":{"action":{"const":"press"},"key":{"type":"string","minLength":1,"maxLength":64}},"required":["action","key"],"additionalProperties":false},{"type":"object","properties":{"action":{"const":"hover"},"ref":{"type":"string","minLength":1,"maxLength":64}},"required":["action","ref"],"additionalProperties":false}]}`)}
}
func requireBinding(toolState *browserState, ctx context.Context) (browserBinding, string, error) {
	key := browserSessionKey(ctx, toolState)
	binding, ok := toolState.get(key)
	if !ok || binding.TargetID == "" {
		return browserBinding{}, key, errBrowserPageNotBound
	}
	return binding, key, nil
}
func validateBrowserArgs(input browserToolArgs) error {
	switch input.Action {
	case "open":
		if strings.TrimSpace(input.URL) == "" {
			return errors.New("open requires url")
		}
		if input.Offset != 0 || input.Format != "" || input.Direction != "" || input.Amount != 0 || input.Milliseconds != 0 {
			return errors.New("open accepts only url")
		}
	case "navigate":
		if strings.TrimSpace(input.URL) == "" {
			return errors.New("navigate requires url")
		}
		if input.Offset != 0 || input.Format != "" || input.Direction != "" || input.Amount != 0 || input.Milliseconds != 0 {
			return errors.New("navigate accepts only url")
		}
	case "snapshot":
		if input.Offset < 0 || input.Offset > maxBrowserSnapshotOffset {
			return errors.New("snapshot offset is out of bounds")
		}
		if input.URL != "" || input.Direction != "" || input.Amount != 0 || input.Milliseconds != 0 {
			return errors.New("snapshot accepts only offset and format")
		}
	case "scroll":
		if input.Direction != "up" && input.Direction != "down" {
			return errors.New("scroll requires direction up or down")
		}
		if input.Amount < 1 || input.Amount > 5000 {
			return errors.New("scroll amount must be between 1 and 5000")
		}
		if input.URL != "" || input.Offset != 0 || input.Format != "" || input.Milliseconds != 0 {
			return errors.New("scroll accepts only direction and amount")
		}
	case "wait":
		if input.Milliseconds < 0 || input.Milliseconds > 30000 {
			return errors.New("wait milliseconds must be between 0 and 30000")
		}
		if input.URL != "" || input.Offset != 0 || input.Format != "" || input.Direction != "" || input.Amount != 0 {
			return errors.New("wait accepts only milliseconds")
		}
	case "close", "status":
		if input.URL != "" || input.Offset != 0 || input.Format != "" || input.Direction != "" || input.Amount != 0 || input.Milliseconds != 0 {
			return errors.New("this browser action accepts no additional fields")
		}
	default:
		return fmt.Errorf("unsupported browser action %q", input.Action)
	}
	return nil
}

func (tool BrowserTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Backend == nil {
		return "", errors.New("browser backend is not configured")
	}
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	var input browserToolArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("web_browser arguments: %w", err)
	}
	if err := validateBrowserArgs(input); err != nil {
		return "", err
	}
	if tool.state == nil {
		tool.state = newBrowserState()
	}
	key := browserSessionKey(ctx, tool.state)
	user := key
	binding, _ := tool.state.get(key)
	if input.Action == "status" {
		var tabs BrowserTabsResult
		var err error
		if sessionTabs, ok := tool.Backend.(BrowserSessionTabs); ok {
			tabs, err = sessionTabs.TabsForSession(ctx, key)
		} else {
			tabs, err = tool.Backend.Tabs(ctx)
		}
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		return browserEnvelope(input.Action, true, map[string]any{"running": tabs.Running, "count": tabs.Count}, "Browser status is a bounded observation.")
	}
	if input.Action == "open" {
		if binding.TargetID != "" {
			if closer, ok := tool.Backend.(BrowserCloser); ok {
				_ = closer.Close(ctx, BrowserCloseRequest{TargetID: binding.TargetID, UserID: binding.UserID})
			}
		}
		tool.state.clear(key)
		binding = browserBinding{}
		opened, err := tool.Backend.Open(ctx, BrowserOpenRequest{URL: input.URL, UserID: user, SessionKey: key})
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		if strings.TrimSpace(opened.TargetID) == "" {
			return browserError(tool.state, key, input.Action, binding, errors.New("browser open returned no usable tab"))
		}
		binding = browserBinding{TargetID: opened.TargetID, UserID: user, SessionKey: key}
		tool.state.put(key, binding)
		return browserEnvelope(input.Action, true, map[string]any{"url": opened.URL, "title": opened.Title}, "Page opened and is now bound to this Bee session.")
	}
	if input.Action == "close" {
		if binding.TargetID == "" {
			return browserEnvelope(input.Action, true, nil, "No page was bound.")
		}
		if closer, ok := tool.Backend.(BrowserCloser); ok {
			if err := closer.Close(ctx, BrowserCloseRequest{TargetID: binding.TargetID, UserID: binding.UserID}); err != nil {
				tool.state.clear(key)
				return browserError(tool.state, key, input.Action, binding, err)
			}
		}
		tool.state.clear(key)
		return browserEnvelope(input.Action, true, nil, "Browser page closed and binding removed.")
	}
	if input.Action == "navigate" || input.Action == "snapshot" || input.Action == "scroll" || input.Action == "wait" {
		if binding.TargetID == "" {
			return browserError(tool.state, key, input.Action, binding, errBrowserPageNotBound)
		}
	}
	switch input.Action {
	case "navigate":
		result, err := tool.Backend.Navigate(ctx, BrowserNavigateRequest{TargetID: binding.TargetID, URL: input.URL, UserID: binding.UserID})
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		if result.TargetID != "" && result.TargetID != binding.TargetID {
			binding.TargetID = result.TargetID
			tool.state.put(key, binding)
		}
		return browserEnvelope(input.Action, true, map[string]any{"url": result.URL, "title": result.Title}, "Navigation completed.")
	case "snapshot":
		result, err := tool.Backend.Snapshot(ctx, BrowserSnapshotRequest{TargetID: binding.TargetID, Format: input.Format, Offset: input.Offset, UserID: binding.UserID})
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		note := ""
		if result.HasMore && result.NextOffset != nil {
			note = fmt.Sprintf("Snapshot is windowed by Camofox. Continue with offset=%d and the same arguments; do not repeat unchanged args.", *result.NextOffset)
		}
		return browserEnvelope(input.Action, true, map[string]any{"url": result.URL, "title": result.Title, "content": result.Text, "offset": result.Offset, "total_chars": result.TotalChars, "has_more": result.HasMore, "next_offset": result.NextOffset, "truncated": result.Truncated}, note)
	case "scroll":
		var result BrowserSnapshotResult
		var err error
		if scroller, ok := tool.Backend.(BrowserScroller); ok {
			result, err = scroller.Scroll(ctx, BrowserScrollRequest{TargetID: binding.TargetID, UserID: binding.UserID, Direction: input.Direction, Amount: input.Amount})
		} else {
			_, err = tool.Backend.Act(ctx, BrowserActRequest{TargetID: binding.TargetID, UserID: binding.UserID, Kind: "scroll", Direction: input.Direction, Amount: input.Amount})
		}
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		_ = result
		return browserEnvelope(input.Action, true, nil, "Scroll completed; call snapshot for the updated page.")
	case "wait":
		var result BrowserSnapshotResult
		var err error
		if waiter, ok := tool.Backend.(BrowserWaiter); ok {
			result, err = waiter.Wait(ctx, BrowserWaitRequest{TargetID: binding.TargetID, UserID: binding.UserID, Milliseconds: input.Milliseconds})
		} else {
			err = errors.New("browser wait is not supported by the configured backend")
		}
		if err != nil {
			return browserError(tool.state, key, input.Action, binding, err)
		}
		_ = result
		return browserEnvelope(input.Action, true, nil, "Wait completed; call snapshot for the updated page.")
	}
	return "", errors.New("unreachable browser action")
}

func (tool BrowserActTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Backend == nil {
		return "", errors.New("browser backend is not configured")
	}
	if tool.state == nil {
		tool.state = newBrowserState()
	}
	var input browserActArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("web_browser_act arguments: %w", err)
	}
	if input.Action != "click" && input.Action != "type" && input.Action != "press" && input.Action != "hover" {
		return "", fmt.Errorf("unsupported browser action %q", input.Action)
	}
	switch input.Action {
	case "click", "hover":
		if strings.TrimSpace(input.Ref) == "" {
			return "", errors.New("browser action requires ref")
		}
		if input.Text != "" || input.Key != "" {
			return "", errors.New("click and hover accept only ref")
		}
	case "type":
		if strings.TrimSpace(input.Ref) == "" || input.Text == "" {
			return "", errors.New("browser type requires ref and text")
		}
		if input.Key != "" {
			return "", errors.New("type accepts only ref and text")
		}
	case "press":
		if strings.TrimSpace(input.Key) == "" {
			return "", errors.New("browser press requires key")
		}
		if input.Ref != "" || input.Text != "" {
			return "", errors.New("press accepts only key")
		}
	}
	binding, key, err := requireBinding(tool.state, ctx)
	if err != nil {
		return "", err
	}
	if _, err = tool.Backend.Act(ctx, BrowserActRequest{TargetID: binding.TargetID, UserID: binding.UserID, Kind: input.Action, Ref: input.Ref, Text: input.Text, Key: input.Key}); err != nil {
		return browserError(tool.state, key, input.Action, binding, err)
	}
	return browserEnvelope(input.Action, true, nil, "Interactive action completed; call web_browser snapshot to inspect the result.")
}
