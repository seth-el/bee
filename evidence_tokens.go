package bee

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	evidenceURLTokenPattern    = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://[^\s"'<>]+|//[A-Za-z0-9._-]+(?::[0-9]+)?(?:/[^\s"'<>]*)?)`)
	evidencePathTokenPattern   = regexp.MustCompile(`(?:^|[\s"'(<\[])(?P<token>(?:\.\.?[\\/][^\s"'<>]+|~[\\/][^\s"'<>]+|\\\\[A-Za-z0-9._-]+\\[^\s"'<>]+|[A-Za-z]:[\\/][^\s"'<>]+|/[A-Za-z0-9._~+\-]+(?:/[A-Za-z0-9._~+\-]+)+))`)
	evidenceSchemeTokenPattern = regexp.MustCompile(`(?i)(?:^|[\s"'(<\[])((?:file|ssh|ftp|mailto|data|javascript|ws|wss|urn):[^\s"'<>]+|[a-z][a-z0-9+.-]*:[\\/][^\s"'<>]+)`)
)

func sensitiveEvidenceTokens(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var tokens []string
	add := func(token string) {
		token = strings.Trim(token, "()[]{}<>,.;!?'")
		if token == "" {
			return
		}
		if _, ok := seen[token]; ok {
			return
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	for _, match := range evidenceURLTokenPattern.FindAllString(value, -1) {
		add(match)
	}
	for _, match := range evidencePathTokenPattern.FindAllStringSubmatch(value, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	for _, match := range evidenceSchemeTokenPattern.FindAllStringSubmatch(value, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	return tokens
}

func validateSensitiveEvidenceTokens(value, supplied string, requireGrounding bool) error {
	if evidenceSecretLike.MatchString(value) || looksLikeSecret(value) {
		return fmt.Errorf("text contains secret-like output")
	}
	for _, token := range sensitiveEvidenceTokens(value) {
		if requireGrounding {
			if !strings.Contains(supplied, token) {
				return fmt.Errorf("text contains an ungrounded path or URL: %q", token)
			}
			continue
		}
		return fmt.Errorf("text contains a path or URL: %q", token)
	}
	return nil
}

func validateOpaqueMemoryID(id string, max int) error {
	if err := validateSafeID("memory rerank candidate id", id, max); err != nil {
		return err
	}
	if err := validateSensitiveEvidenceTokens(id, "", false); err != nil {
		return err
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, "/\\:") {
		return fmt.Errorf("memory rerank candidate id has path-like syntax")
	}
	return nil
}
