package telegramcompat

import "testing"

func TestVoiceCommandParsing(t *testing.T) {
	for input, want := range map[string][2]string{
		"/say hello there":          {"say", "hello there"},
		"/speak@bee_bot  what's up": {"speak", "what's up"},
		"/speak":                    {"speak", ""},
		"/status":                   {"", ""},
		"say hi":                    {"", ""},
	} {
		name, args := voiceCommand(input)
		if name != want[0] || args != want[1] {
			t.Fatalf("%q = %q %q", input, name, args)
		}
	}
}
