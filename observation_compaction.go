package bee

import "strings"

// DeterministicObservationPreprocess removes only exact safe repetition and
// blank/separator boilerplate. It preserves error-like lines, exit codes,
// identifiers, and bounded head/tail context. Structured/lossless callers must
// bypass this helper before handing their observation to a model.
func DeterministicObservationPreprocess(observation string) string {
	if observation == "" {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(observation, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if index == 0 || index == len(lines)-1 || len(kept) == 0 || kept[len(kept)-1] != "" {
				kept = append(kept, "")
			}
			continue
		}
		if safeObservationBoilerplate(trimmed) && (trimmed == "---" || trimmed == "___" || trimmed == "...") {
			continue
		}
		if safeObservationBoilerplate(trimmed) && containsTrimmedLine(kept, trimmed) {
			// Only the explicit safe-boilerplate allowlist may be deduplicated.
			// Ordinary repeated events remain loss-preserving.
			continue
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && kept[0] == "" {
		kept = kept[1:]
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\n")
}

func containsTrimmedLine(lines []string, target string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}

func safeObservationBoilerplate(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "---" || lower == "___" || lower == "..." || lower == "stdout:" || lower == "stderr:" {
		return true
	}
	return strings.HasPrefix(lower, "progress:") || strings.HasPrefix(lower, "status: waiting") || strings.HasPrefix(lower, "heartbeat:")
}

func observationImportantLine(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{"error", "fatal", "failed", "panic", "exception", "exit code", "exit status", "warning", "uuid", "trace", "id:", "code:"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// DeterministicObservationFallback keeps a stable, JSON-shaped summary while
// retaining high-signal lines before ordinary facts. It is intentionally
// independent of provider availability.
func DeterministicObservationFallbackPreservingSignals(observation string, maxBytes int) string {
	if maxBytes <= 1 {
		return ""
	}
	processed := DeterministicObservationPreprocess(observation)
	lines := strings.Split(processed, "\n")
	important := make([]string, 0, len(lines))
	ordinary := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if observationImportantLine(line) {
			important = append(important, line)
		} else {
			ordinary = append(ordinary, line)
		}
	}
	if len(lines) > 0 {
		head := lines
		if len(head) > 4 {
			head = head[:4]
		}
		tail := lines
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		ordinary = append(ordinary, head...)
		ordinary = append(ordinary, tail...)
	}
	return marshalBoundedObservationSummary(ObservationSummary{Errors: dedupeStrings(important), Facts: dedupeStrings(ordinary)}, maxBytes)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
