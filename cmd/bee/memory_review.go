package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/seth-el/bee"
)

func runMemoryReview(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("memory review command is required: list, decide, or normalize")
	}
	switch args[0] {
	case "list":
		return memoryReviewList(args[1:], output, errorOutput)
	case "decide":
		return memoryReviewDecide(args[1:], output, errorOutput)
	case "normalize":
		return memoryReviewNormalize(args[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown memory review command %q", args[0])
	}
}

type memoryReviewListResult struct {
	CandidateCount int                            `json:"candidate_count"`
	DecisionCount  int                            `json:"decision_count"`
	Unreviewed     int                            `json:"unreviewed"`
	ByAction       map[bee.MemoryReviewAction]int `json:"by_action,omitempty"`
	Candidates     []memoryReviewListCandidate    `json:"candidates"`
}

type memoryReviewListCandidate struct {
	CandidateID string                      `json:"candidate_id"`
	Status      bee.MemoryCandidateStatus   `json:"status"`
	Category    bee.MemoryCandidateCategory `json:"category"`
	Source      bee.MemoryCandidateSource   `json:"source"`
	Action      bee.MemoryReviewAction      `json:"action,omitempty"`
	Reviewer    string                      `json:"reviewer,omitempty"`
	Reason      string                      `json:"reason,omitempty"`
}

func memoryReviewList(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory review list", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	candidatePath := flags.String("candidates", "", "M2 candidate JSONL path")
	decisionPath := flags.String("decisions", "", "review decision JSONL path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory review list does not accept positional arguments")
	}
	if *candidatePath == "" {
		return errors.New("review list requires --candidates")
	}
	candidates, _, err := bee.LoadMemoryCandidates(*candidatePath)
	if err != nil {
		return err
	}
	decisions, err := bee.LoadMemoryReviewDecisions(*decisionPath)
	if err != nil {
		return err
	}
	byCandidate := make(map[string]bee.MemoryReviewDecision, len(decisions))
	byAction := make(map[bee.MemoryReviewAction]int)
	for _, decision := range decisions {
		byCandidate[decision.CandidateID] = decision
		byAction[decision.Action]++
	}
	result := memoryReviewListResult{CandidateCount: len(candidates), DecisionCount: len(decisions), ByAction: byAction, Candidates: make([]memoryReviewListCandidate, 0, len(candidates))}
	for _, candidate := range candidates {
		item := memoryReviewListCandidate{CandidateID: candidate.CandidateID, Status: candidate.Status, Category: candidate.Category, Source: candidate.Source}
		if decision, ok := byCandidate[candidate.CandidateID]; ok {
			item.Action, item.Reviewer, item.Reason = decision.Action, decision.Reviewer, decision.Reason
		} else {
			result.Unreviewed++
		}
		result.Candidates = append(result.Candidates, item)
	}
	return json.NewEncoder(output).Encode(result)
}

func memoryReviewDecide(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory review decide", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	path := flags.String("decisions", "", "absolute append-only decision JSONL path")
	candidateID := flags.String("candidate", "", "candidate ID")
	action := flags.String("action", "", "accept, reject, defer, mark_historical, mark_superseded, replace, or merge")
	reviewer := flags.String("reviewer", "", "reviewer ID")
	reason := flags.String("reason", "", "bounded review reason")
	target := flags.String("target", "", "durable_memory, user_profile, or identity")
	replacement := flags.String("replacement-candidate", "", "replacement/superseding candidate ID")
	namespace := flags.String("namespace", "", "normalized durable namespace")
	key := flags.String("key", "", "normalized memory key")
	summary := flags.String("summary", "", "normalized summary")
	content := flags.String("content", "", "explicit normalized content; omit to use candidate content")
	sensitivity := flags.String("sensitivity", "", "public, personal, sensitive, or secret")
	confidence := flags.Float64("confidence", 0, "normalized confidence between 0 and 1")
	effective := flags.String("effective-at", "", "RFC3339 effective timestamp")
	confirmed := flags.String("last-confirmed-at", "", "RFC3339 last-confirmed timestamp")
	expires := flags.String("expires-at", "", "optional RFC3339 expiration timestamp")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory review decide does not accept positional arguments")
	}
	if *path == "" || *candidateID == "" || *action == "" || *reviewer == "" || *reason == "" {
		return errors.New("review decide requires --decisions, --candidate, --action, --reviewer, and --reason")
	}
	parseTime := func(value, name string) (time.Time, error) {
		if value == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("--%s must be RFC3339", name)
		}
		return parsed.UTC(), nil
	}
	effectiveAt, err := parseTime(*effective, "effective-at")
	if err != nil {
		return err
	}
	confirmedAt, err := parseTime(*confirmed, "last-confirmed-at")
	if err != nil {
		return err
	}
	expiresAt, err := parseTime(*expires, "expires-at")
	if err != nil {
		return err
	}
	decision := bee.MemoryReviewDecision{DecisionID: fmt.Sprintf("review-%d", time.Now().UTC().UnixNano()), CandidateID: *candidateID, Action: bee.MemoryReviewAction(*action), Reviewer: *reviewer, Reason: *reason, ReviewedAt: time.Now().UTC(), ReplacementCandidateID: *replacement, Target: bee.MemoryReviewTarget(*target), Namespace: *namespace, Key: *key, Summary: *summary, Content: *content, Sensitivity: bee.MemorySensitivity(*sensitivity), Confidence: *confidence, EffectiveAt: effectiveAt, LastConfirmedAt: confirmedAt, ExpiresAt: expiresAt}
	if err := bee.AppendMemoryReviewDecision(*path, decision); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(decision)
}

func memoryReviewNormalize(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory review normalize", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	candidates := flags.String("candidates", "", "M2 candidate JSONL path")
	decisions := flags.String("decisions", "", "review decision JSONL path; missing means no candidates approved")
	normalized := flags.String("output", "", "absolute normalized JSONL output path")
	reportPath := flags.String("report", "", "absolute review report output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory review normalize does not accept positional arguments")
	}
	if *candidates == "" || *normalized == "" || *reportPath == "" {
		return errors.New("review normalize requires --candidates, --output, and --report")
	}
	items, report, err := bee.NormalizeMemoryReview(*candidates, *decisions)
	if err != nil {
		return err
	}
	if err := bee.WriteMemoryReviewArtifacts(*normalized, *reportPath, items, report); err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if report.InvalidDecisions > 0 || report.OpenConflicts > 0 {
		return errors.New("memory review normalization has unresolved validation/conflict issues")
	}
	return nil
}
