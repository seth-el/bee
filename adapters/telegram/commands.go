package telegram

import (
	"context"
	"errors"
	"strings"

	"github.com/seth-el/bee"
)

// BotCommand is one entry in Telegram's "/" command menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// ChatCommands are the slash commands Bee answers in Telegram, in menu
// order. They run through the same core command handling as the TUI.
// VoiceChatCommands are added when voice is enabled.
var VoiceChatCommands = []BotCommand{
	{"speak", "Ask, and hear the answer as a voice note"},
	{"say", "Read some text aloud"},
}

var ChatCommands = []BotCommand{
	{"status", "Bee and session status"},
	{"new", "Start a fresh conversation"},
	{"resume", "List conversations, or /resume <id> to switch"},
	{"continue", "Resume the latest saved checkpoint"},
	{"stop", "Stop the reply that is running"},
	{"steer", "Stop the running reply and send a correction"},
	{"inbox", "Unread job notifications (/inbox ack to clear)"},
	{"network", "This chat's network access: on or off"},
	{"plan", "Plan mode: on, off, status, reset, complete"},
	{"tasks", "Current plan tasks"},
	{"compact", "Compact this conversation's context"},
	{"tool_limit", "Per-turn tool-call limit: on or off"},
	{"subagents", "Child model capacity"},
	{"help", "This list"},
}

// UnsupportedChatCommands are core commands that do not work in Telegram:
// display toggles only affect the TUI, and media retrieval is a web-gateway
// feature. /stop, /kill, /cancel and /steer never reach the router: the
// poller acts on them immediately (see Poller.interrupt).
var UnsupportedChatCommands = []bee.CommandName{
	bee.CommandTools, bee.CommandThinking, bee.CommandVideo, bee.CommandImage,
}

// ChatCommandMenu is the "/" menu for this configuration.
func ChatCommandMenu(voice bool) []BotCommand {
	menu := append([]BotCommand(nil), ChatCommands...)
	if voice {
		help := menu[len(menu)-1]
		menu = append(append(menu[:len(menu)-1], VoiceChatCommands...), help)
	}
	return menu
}

// ChatCommandHelp is the /help reply in Telegram.
func ChatCommandHelp(voice bool) string {
	menu := ChatCommandMenu(voice)
	lines := make([]string, 0, len(menu)+3)
	lines = append(lines, "Commands:")
	for _, command := range menu {
		lines = append(lines, "/"+command.Command+" — "+command.Description)
	}
	lines = append(lines, "Conversations are shared with the TUI and other Bee clients: /resume lists them all.")
	if voice {
		lines = append(lines, "Send a voice note and Bee answers in text and voice.")
	}
	return strings.Join(lines, "\n")
}

// UnsupportedChatCommandsFor adds the voice commands when voice is off.
func UnsupportedChatCommandsFor(voice bool) []bee.CommandName {
	list := append([]bee.CommandName(nil), UnsupportedChatCommands...)
	if !voice {
		list = append(list, bee.CommandSpeak, bee.CommandSay)
	}
	return list
}

// NormalizeChatCommand maps Telegram's command spelling to Bee's. Telegram
// command names cannot contain "-", so the menu uses /tool_limit.
func NormalizeChatCommand(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/tool_limit") {
		rest := strings.TrimPrefix(trimmed, "/tool_limit")
		if rest == "" || rest[0] == ' ' || rest[0] == '@' || rest[0] == '\n' {
			return "/tool-limit" + rest
		}
	}
	return text
}

type setMyCommandsRequest struct {
	Commands []BotCommand `json:"commands"`
}

// SetMyCommands publishes the bot's "/" command menu.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	if len(commands) == 0 || len(commands) > 100 {
		return errors.New("telegram setMyCommands request is invalid")
	}
	var response struct {
		OK     bool `json:"ok"`
		Result bool `json:"result"`
	}
	if err := c.call(ctx, "setMyCommands", setMyCommandsRequest{Commands: commands}, &response); err != nil {
		return err
	}
	if !response.Result {
		return errors.New("telegram setMyCommands was not accepted")
	}
	return nil
}
