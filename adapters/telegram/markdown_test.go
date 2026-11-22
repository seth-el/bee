package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

func TestRenderTelegramHTML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain escaped", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		{"bold italic strike", "**bold** *it* _also_ ~~gone~~", "<b>bold</b> <i>it</i> <i>also</i> <s>gone</s>"},
		{"bold italic nested", "***both***", "<b><i>both</i></b>"},
		{"inline code escapes", "run `a<b && rm *x*`", "run <code>a&lt;b &amp;&amp; rm *x*</code>"},
		{"double backtick code", "``a ` b``", "<code>a ` b</code>"},
		{"snake case literal", "use my_var_name and 2*3*4", "use my_var_name and 2*3*4"},
		{"unmatched literal", "**not closed and * star", "**not closed and * star"},
		{"spaced star literal", "a * b * c", "a * b * c"},
		{"link", "see [the docs](https://example.com/a?b=1&c=2)", `see <a href="https://example.com/a?b=1&amp;c=2">the docs</a>`},
		{"unsafe link literal", "[x](javascript:alert(1))", "[x](javascript:alert(1))"},
		{"image as link", "![chart](https://example.com/c.png)", `<a href="https://example.com/c.png">chart</a>`},
		{"heading", "## Setup *now*", "<b>Setup <i>now</i></b>"},
		{"heading with bold", "# **Title**", "<b>Title</b>"},
		{"bullets and tasks", "- one\n  * two\n- [x] done\n- [ ] todo", "• one\n  • two\n☑ done\n☐ todo"},
		{"numbered kept", "1. first\n2. **second**", "1. first\n2. <b>second</b>"},
		{"rule", "above\n\n---\n\nbelow", "above\n\n──────────\n\nbelow"},
		{"escapes", `\*not italic\* and \_x\_`, "*not italic* and _x_"},
		{"spoiler", "||hidden||", "<tg-spoiler>hidden</tg-spoiler>"},
		{"shell or", "a || b", "a || b"},
		{"quote", "> quoted **bold**\n> second", "<blockquote>quoted <b>bold</b>\nsecond</blockquote>"},
		{"code block lang", "```go\nfunc main() { if a < b {} }\n```", `<pre><code class="language-go">func main() { if a &lt; b {} }</code></pre>`},
		{"code block plain", "~~~\n**not bold**\n~~~", "<pre>**not bold**</pre>"},
		{"code block bad lang", "```<script>\nx\n```", "<pre>x</pre>"},
		{"unclosed fence", "```sh\necho hi", `<pre><code class="language-sh">echo hi</code></pre>`},
		{"html smuggling", "<b>x</b> <a href=\"javascript:1\">y</a>", "&lt;b&gt;x&lt;/b&gt; &lt;a href=&quot;javascript:1&quot;&gt;y&lt;/a&gt;"},
		{"narrow table grid", "| Name | Size |\n|---|---:|\n| **a** | 1 |\n| [bee](https://x.y) | 200 |", "<pre>Name │ Size\n─────┼─────\na    │ 1\nbee  │ 200</pre>"},
		{"wide table records", "| Host | Role | Address |\n|---|---|---|\n| alpha | runs bee and the Telegram adapter | 203.0.113.10 |\n| beta | `whisper` + TTS | 203.0.113.29 |",
			"<b>alpha</b>\nRole: runs bee and the Telegram adapter\nAddress: 203.0.113.10\n\n<b>beta</b>\nRole: <code>whisper</code> + TTS\nAddress: 203.0.113.29"},
		{"emoji table records", "| Check | OK |\n|---|---|\n| tests | ✅ |\n| vet | ✅ |", "<b>tests</b> — ✅\n<b>vet</b> — ✅"},
		{"cjk table records", "| a | b | c |\n|---|---|---|\n| 名前 | x | y |", "<b>名前</b>\nb: x\nc: y"},
		{"escaped and code pipes", "| Op | Meaning |\n|---|---|\n| `a \\| b` | or |\n| x \\| y | lit |", "<pre>Op    │ Meaning\n──────┼────────\na | b │ or\nx | y │ lit</pre>"},
		{"empty cells and bold title", "| Name | A | B |\n|---|---|---|\n| **one** |  | 2 |\n|  | 3 |  |", "<pre>Name │ A │ B\n─────┼───┼──\none  │   │ 2\n     │ 3 │</pre>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderTelegramHTML(tc.in); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestFormatMarkdownTableWidthOption(t *testing.T) {
	table := "| Name | Size |\n|---|---|\n| alpha | 1 |"
	if got := FormatMarkdown(table, MarkdownOptions{TableWidth: 8}); len(got) != 1 || got[0].HTML != "<b>alpha</b> — 1" {
		t.Fatalf("narrow width = %#v", got)
	}
	if got := FormatMarkdown(table, MarkdownOptions{}); len(got) != 1 || !strings.HasPrefix(got[0].HTML, "<pre>") {
		t.Fatalf("default width = %#v", got)
	}
	var rows strings.Builder
	rows.WriteString("| Host | Role | Notes |\n|---|---|---|\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&rows, "| host-%03d | some role text here | a fairly long note about host %d |\n", i, i)
	}
	chunks := FormatMarkdown(rows.String(), MarkdownOptions{})
	if len(chunks) < 2 {
		t.Fatalf("long record table should split, got %d", len(chunks))
	}
	for index, chunk := range chunks {
		if len(chunk.HTML) > MaxTelegramTextBytes || strings.Count(chunk.HTML, "<b>") != strings.Count(chunk.HTML, "</b>") {
			t.Fatalf("chunk %d bad", index)
		}
	}
}

func TestMarkdownChunksSplitLongCodeBlocksAndProse(t *testing.T) {
	var code strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&code, "line %03d: x := a < b && c\n", i)
	}
	prose := strings.Repeat("word **bold** ", 600)
	text := "Intro paragraph.\n\n```go\n" + code.String() + "```\n\n" + prose + "\n\n" + strings.Repeat("界", 3000)
	chunks := MarkdownChunks(text, MaxTelegramTextBytes)
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	codeChunks := 0
	for index, chunk := range chunks {
		if len(chunk.HTML) > MaxTelegramTextBytes || len(chunk.Plain) > MaxTelegramTextBytes {
			t.Fatalf("chunk %d too large: html %d plain %d", index, len(chunk.HTML), len(chunk.Plain))
		}
		if !utf8.ValidString(chunk.HTML) || !utf8.ValidString(chunk.Plain) || strings.TrimSpace(chunk.HTML) == "" {
			t.Fatalf("chunk %d invalid or empty", index)
		}
		if strings.Count(chunk.HTML, "<pre") != strings.Count(chunk.HTML, "</pre>") {
			t.Fatalf("chunk %d has unbalanced pre: %q", index, chunk.HTML[:80])
		}
		if strings.Count(chunk.HTML, "<b>") != strings.Count(chunk.HTML, "</b>") {
			t.Fatalf("chunk %d has unbalanced bold", index)
		}
		if strings.Contains(chunk.HTML, "language-go") {
			codeChunks++
		}
	}
	if codeChunks < 2 {
		t.Fatalf("long code block should span chunks, got %d", codeChunks)
	}
	if got := MarkdownChunks("  \n\n ", MaxTelegramTextBytes); len(got) != 0 {
		t.Fatalf("blank text chunks = %#v", got)
	}
}

func TestChannelSendsHTMLAndFallsBackToPlainOnParseError(t *testing.T) {
	config := testConfig(t)
	var mu sync.Mutex
	var requests []SendMessageRequest
	rejectHTML := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requests = append(requests, request)
		reject := rejectHTML && request.ParseMode == "HTML"
		mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	channel, err := NewTelegramChannel(client, config, ChannelOptions{Markdown: true, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	message := bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-n100123", ReplyTo: "tg-msg-p5", Text: "Run **this**:\n\n```sh\nls -la\n```"}
	if err := channel.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(requests) != 2 || requests[0].ParseMode != "HTML" || requests[0].Text != "Run <b>this</b>:\n\n<pre><code class=\"language-sh\">ls -la</code></pre>" {
		t.Fatalf("first request = %#v", requests)
	}
	if requests[1].ParseMode != "" || requests[1].Text != "Run **this**:\n\n```sh\nls -la\n```" || requests[1].ReplyToMessageID != 5 {
		t.Fatalf("fallback request = %#v", requests[1])
	}
	requests, rejectHTML = nil, false
	mu.Unlock()
	if err := channel.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].ParseMode != "HTML" {
		t.Fatalf("accepted HTML should send once: %#v", requests)
	}
}
