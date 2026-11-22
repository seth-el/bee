package main

import "strings"

// beeSubcommands are the first-argument words run() dispatches before flag
// parsing. Anything else is treated as a prompt, so keep this in sync.
var beeSubcommands = []string{
	"version", "code", "memory", "config", "skill", "plugin", "jobs", "inbox", "sessions",
	"providers", "init", "doctor", "harness", "health", "readiness", "maintenance", "child", "help",
}

// likelySubcommandTypo reports whether args look like a mistyped subcommand
// rather than a prompt: one or two bare lowercase words whose first word is
// within a small edit distance of a real subcommand. Without this, a typo
// such as `bee sesions list` is sent to the model as a prompt and creates a
// durable session. Real prompts are usually quoted or longer; `bee -- ...`
// always sends the words as a prompt.
func likelySubcommandTypo(args []string) (string, bool) {
	if len(args) == 0 || len(args) > 2 {
		return "", false
	}
	for _, arg := range args {
		if !bareLowercaseWord(arg) {
			return "", false
		}
	}
	word := args[0]
	if len(word) < 3 {
		return "", false
	}
	limit := 1
	if len(word) > 5 {
		limit = 2
	}
	best, bestDistance := "", limit+1
	for _, command := range beeSubcommands {
		if command == word {
			return "", false
		}
		if distance := editDistance(word, command); distance < bestDistance {
			best, bestDistance = command, distance
		}
	}
	return best, best != ""
}

func bareLowercaseWord(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(value, "-")
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
