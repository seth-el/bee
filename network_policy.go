package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Per-session network switch (G11). The default never changes: sessions have
// network. The owner can turn it off for one session with /network off
// (TUI, HTTP /api/commands, Telegram); the model has no tool to change it.
// While it is off:
//   - new sandboxed processes (exec, run_command local, terminal) start
//     with --unshare-net, and a process that can't be sandboxed is refused;
//   - host_exec, remote run_command, repo_clone, the web/browser/media tools,
//     MCP and extension tools refuse at call time (they stay in the tool list,
//     keeping the prompt-cache prefix stable);
//   - terminals this session opened with network are closed when it turns
//     off, and terminals that still have network are off-limits;
//   - LSP diagnostics are skipped.
// Owner-configured lifecycle hooks are unchanged, and calls to Bee's own
// model servers are not affected. The setting is recorded as a
// network_policy event, so resume keeps it and the transcript shows when it
// changed. Turning it back on applies to new processes.

// EventNetworkPolicy records an owner's /network change.
const EventNetworkPolicy EventType = "network_policy"

// NetworkPolicyEvent is the bounded record of one change.
type NetworkPolicyEvent struct {
	Enabled bool   `json:"enabled"`
	Actor   string `json:"actor"`
}

// ErrNetworkOff is returned by anything refused because the session's
// network is off.
var ErrNetworkOff = errors.New("network access is off for this session (the owner can run /network on)")

type networkOffKey struct{}

// withNetworkOff marks ctx as belonging to a session whose network is off.
func withNetworkOff(ctx context.Context, off bool) context.Context {
	if !off {
		return ctx
	}
	return context.WithValue(ctx, networkOffKey{}, true)
}

// NetworkOffFromContext reports whether the calling session's network is off.
func NetworkOffFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	off, _ := ctx.Value(networkOffKey{}).(bool)
	return off
}

// latestSessionNetwork returns the session's setting: enabled unless the
// latest network_policy event turned it off.
func latestSessionNetwork(events []Event) bool {
	enabled := true
	for _, event := range events {
		if event.Type != EventNetworkPolicy {
			continue
		}
		payload, err := decodeEventPayload[NetworkPolicyEvent](event, EventNetworkPolicy)
		if err == nil {
			enabled = payload.Enabled
		}
	}
	return enabled
}

// SessionNetworkEnabled reports a session's setting ("" means the next new
// session, which honors a pending /network off).
func (app *Application) SessionNetworkEnabled(id SessionID) (bool, error) {
	if app == nil {
		return true, errors.New("application is required")
	}
	if strings.TrimSpace(string(id)) == "" || app.Store == nil {
		app.mu.Lock()
		defer app.mu.Unlock()
		if app.pendingNetworkOff {
			return false, nil
		}
		return true, nil
	}
	session, err := app.Store.Load(id)
	if err != nil {
		return true, err
	}
	return latestSessionNetwork(session.EventsSnapshot()), nil
}

// SetSessionNetwork changes a session's setting. Turning it off closes the
// terminals this session opened while it had network and reports how many.
// Without a session yet it applies to the next one (like /plan).
func (app *Application) SetSessionNetwork(ctx context.Context, id SessionID, enabled bool) (changed bool, closedTerminals int, err error) {
	if app == nil {
		return false, 0, errors.New("application is required")
	}
	if strings.TrimSpace(string(id)) == "" || app.Store == nil {
		app.mu.Lock()
		changed = app.pendingNetworkOff == enabled
		app.pendingNetworkOff = !enabled
		app.mu.Unlock()
		return changed, 0, nil
	}
	session, err := app.Store.Load(id)
	if err != nil {
		return false, 0, err
	}
	current := latestSessionNetwork(session.EventsSnapshot())
	if current != enabled {
		event, err := newEvent(session.ID, EventNetworkPolicy, NetworkPolicyEvent{Enabled: enabled, Actor: "owner"})
		if err != nil {
			return false, 0, err
		}
		if err := session.Append(event); err != nil {
			return false, 0, err
		}
		changed = true
	}
	if !enabled && app.Terminal != nil {
		closedTerminals = app.Terminal.CloseNetworkedForSession(ctx, id)
	}
	return changed, closedTerminals, nil
}

// prepareSessionNetwork returns whether the run's session has network,
// applying a pending pre-session /network off to a new session once.
func (app *Application) prepareSessionNetwork(session *Session) (bool, error) {
	if app == nil {
		return true, nil
	}
	app.mu.Lock()
	pending := app.pendingNetworkOff
	app.mu.Unlock()
	if session == nil {
		return !pending, nil
	}
	enabled := latestSessionNetwork(session.EventsSnapshot())
	if pending && enabled {
		event, err := newEvent(session.ID, EventNetworkPolicy, NetworkPolicyEvent{Enabled: false, Actor: "owner"})
		if err != nil {
			return true, err
		}
		if err := session.Append(event); err != nil {
			return true, err
		}
		enabled = false
	}
	if pending {
		app.mu.Lock()
		app.pendingNetworkOff = false
		app.mu.Unlock()
	}
	return enabled, nil
}

// networkToolNames always reach the network.
var networkToolNames = map[string]bool{
	HostExecToolName:      true,
	"web_fetch":           true,
	"web_search":          true,
	BrowserToolName:       true,
	WebResearchToolName:   true,
	"media_search":        true,
	MediaRetrieveToolName: true,
	"repo_clone":          true,
}

// toolNeedsNetwork reports whether a call can't run with the session's
// network off. Sandboxed shell tools are not listed: they run with the
// network namespace removed instead.
func toolNeedsNetwork(tool Tool, name string, args json.RawMessage) bool {
	if networkToolNames[name] {
		return true
	}
	switch tool.(type) {
	case mcpTool, *mcpTool, extensionTool, *extensionTool:
		return true
	}
	if name == "run_command" {
		var input struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(args, &input) == nil && input.Target == string(CommandTargetSSH) {
			return true
		}
	}
	return false
}

// commandNetwork handles /network, /network on and /network off.
func commandNetwork(ctx context.Context, commandContext CommandContext, args string) CommandResult {
	app := commandContext.Application
	if app == nil {
		return commandError("/network requires an assistant")
	}
	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "":
		enabled, err := app.SessionNetworkEnabled(commandContext.SessionID)
		if err != nil {
			return commandError("network status unavailable: " + boundedCommandText(err.Error()))
		}
		state := networkStateText(enabled)
		if commandContext.SessionID == "" && !enabled {
			state = "off for the next session"
		}
		return CommandResult{Handled: true, Name: CommandNetwork, Kind: commandKindNetwork, Text: "network: " + state}
	case "on", "off":
	default:
		return commandError("usage: /network [on|off]")
	}
	if commandContext.RunActive {
		return commandError("/network can't change while a response is running; try again when it finishes")
	}
	enabled := arg == "on"
	changed, closed, err := app.SetSessionNetwork(ctx, commandContext.SessionID, enabled)
	if err != nil {
		return commandError("network change failed: " + boundedCommandText(err.Error()))
	}
	var text string
	switch {
	case !changed && enabled:
		text = "network is already on"
	case !changed:
		text = "network is already off"
	case enabled:
		text = "network on: new commands and tools can reach the network again"
	default:
		text = "network off: new commands run without network, and web, remote and MCP tools are refused"
	}
	if commandContext.SessionID == "" && changed {
		text += " (applies to the next session)"
	}
	if closed > 0 {
		text += fmt.Sprintf("; closed %d terminal%s that had network", closed, pluralSuffix(closed))
	}
	return CommandResult{Handled: true, Name: CommandNetwork, Kind: commandKindNetwork, Text: text}
}

func networkStateText(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
