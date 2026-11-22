package main

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

// The `bee memory search --json` contract: a small, stable shape for scripts
// and context bridges that read Bee memory. Every key is always present;
// missing values are empty strings or an empty list.
const (
	memorySearchJSONMaxResults = 20
	memorySearchJSONMaxChars   = 2000
)

type memorySearchJSONResult struct {
	Path        string   `json:"path"`
	Score       float64  `json:"score"`
	Snippet     string   `json:"snippet"`
	Citation    string   `json:"citation"`
	Corpus      string   `json:"corpus"`
	Key         string   `json:"key"`
	Content     string   `json:"content"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	Sensitivity string   `json:"sensitivity"`
	Source      string   `json:"source"`
	UpdateDate  string   `json:"update_date"`
}

type memorySearchJSONDocument struct {
	Results []memorySearchJSONResult `json:"results"`
}

// memorySearchJSON maps search hits to the bridge contract. Redacted hits
// (sensitive records the caller was not allowed to read) are left out
// entirely rather than emitted as empty stubs.
func memorySearchJSON(result bee.MemorySearchResult) memorySearchJSONDocument {
	document := memorySearchJSONDocument{Results: []memorySearchJSONResult{}}
	for _, hit := range result.Hits {
		if len(document.Results) >= memorySearchJSONMaxResults {
			break
		}
		if hit.Record.Redacted {
			continue
		}
		record := hit.Record.Record
		content := boundMemorySearchJSONText(record.Content)
		summary := boundMemorySearchJSONText(record.Summary)
		snippet := summary
		if snippet == "" {
			snippet = content
		}
		updated := ""
		if !record.UpdatedAt.IsZero() {
			updated = record.UpdatedAt.UTC().Format(time.RFC3339)
		}
		document.Results = append(document.Results, memorySearchJSONResult{
			Path:        record.Key,
			Score:       hit.Score.Total,
			Snippet:     snippet,
			Citation:    boundMemorySearchJSONText(record.Source),
			Corpus:      string(record.Namespace),
			Key:         record.Key,
			Content:     content,
			Summary:     summary,
			Tags:        memorySearchJSONTags(record.Tags),
			Sensitivity: string(record.Sensitivity),
			Source:      boundMemorySearchJSONText(record.Source),
			UpdateDate:  updated,
		})
	}
	return document
}

func memorySearchJSONTags(tags map[string]string) []string {
	out := make([]string, 0, len(tags))
	for key, value := range tags {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

func boundMemorySearchJSONText(text string) string {
	if utf8.RuneCountInString(text) <= memorySearchJSONMaxChars {
		return text
	}
	var builder strings.Builder
	count := 0
	for _, r := range text {
		if count == memorySearchJSONMaxChars {
			break
		}
		builder.WriteRune(r)
		count++
	}
	return builder.String()
}

func writeMemorySearchJSON(output io.Writer, result bee.MemorySearchResult) error {
	return json.NewEncoder(output).Encode(memorySearchJSON(result))
}
