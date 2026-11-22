package bee

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MemoryReviewFormatVersion = 1
	DefaultReviewMaxBytes     = 256 << 20
	MaxReviewBytes            = 1 << 30
	MaxReviewDecisions        = 100_000
	MaxReviewReasonBytes      = 4 << 10
)

type MemoryReviewAction string

const (
	MemoryReviewAccept     MemoryReviewAction = "accept"
	MemoryReviewReject     MemoryReviewAction = "reject"
	MemoryReviewDefer      MemoryReviewAction = "defer"
	MemoryReviewHistorical MemoryReviewAction = "mark_historical"
	MemoryReviewSuperseded MemoryReviewAction = "mark_superseded"
	MemoryReviewReplace    MemoryReviewAction = "replace"
	MemoryReviewMerge      MemoryReviewAction = "merge"
)

type MemoryReviewTarget string

const (
	MemoryReviewTargetDurableMemory MemoryReviewTarget = "durable_memory"
	MemoryReviewTargetUserProfile   MemoryReviewTarget = "user_profile"
	MemoryReviewTargetIdentity      MemoryReviewTarget = "identity"
)

type MemoryReviewDecision struct {
	DecisionID             string             `json:"decision_id"`
	CandidateID            string             `json:"candidate_id"`
	Action                 MemoryReviewAction `json:"action"`
	Reviewer               string             `json:"reviewer"`
	Reason                 string             `json:"reason"`
	ReviewedAt             time.Time          `json:"reviewed_at"`
	ReplacementCandidateID string             `json:"replacement_candidate_id,omitempty"`
	Target                 MemoryReviewTarget `json:"target,omitempty"`
	Namespace              string             `json:"namespace,omitempty"`
	Key                    string             `json:"key,omitempty"`
	Summary                string             `json:"summary,omitempty"`
	Content                string             `json:"content,omitempty"`
	Sensitivity            MemorySensitivity  `json:"sensitivity,omitempty"`
	Confidence             float64            `json:"confidence,omitempty"`
	EffectiveAt            time.Time          `json:"effective_at,omitempty"`
	LastConfirmedAt        time.Time          `json:"last_confirmed_at,omitempty"`
	ExpiresAt              time.Time          `json:"expires_at,omitempty"`
	Status                 string             `json:"status,omitempty"`
}

type MemoryNormalizedCandidate struct {
	CandidateID     string                `json:"candidate_id"`
	DecisionID      string                `json:"decision_id"`
	Target          MemoryReviewTarget    `json:"target"`
	Namespace       string                `json:"namespace,omitempty"`
	Key             string                `json:"key,omitempty"`
	Summary         string                `json:"summary"`
	Content         string                `json:"content"`
	Sensitivity     MemorySensitivity     `json:"sensitivity"`
	Confidence      float64               `json:"confidence"`
	Status          string                `json:"status"`
	EffectiveAt     time.Time             `json:"effective_at"`
	LastConfirmedAt time.Time             `json:"last_confirmed_at"`
	ExpiresAt       time.Time             `json:"expires_at,omitempty"`
	SupersededBy    string                `json:"superseded_by,omitempty"`
	Source          MemoryCandidateSource `json:"source"`
}

type MemoryReviewReport struct {
	FormatVersion        int      `json:"format_version"`
	ReviewBatchID        string   `json:"review_batch_id"`
	CandidateArtifactSHA string   `json:"candidate_artifact_sha256"`
	CandidateCount       int      `json:"candidate_count"`
	DecisionCount        int      `json:"decision_count"`
	Unreviewed           int      `json:"unreviewed"`
	Accepted             int      `json:"accepted"`
	Rejected             int      `json:"rejected"`
	Deferred             int      `json:"deferred"`
	Historical           int      `json:"historical"`
	Superseded           int      `json:"superseded"`
	ReadyForM4           int      `json:"ready_for_m4"`
	InvalidDecisions     int      `json:"invalid_decisions"`
	OpenConflicts        int      `json:"open_conflicts"`
	DuplicateFinalKeys   int      `json:"duplicate_final_keys"`
	StaleOrMissing       int      `json:"stale_or_missing"`
	Diagnostics          []string `json:"diagnostics,omitempty"`
}

// NormalizeMemoryReview validates M2 candidates and an append-only decision
// log, returning only explicit, fully normalized records ready for M4. It
// never writes the durable memory store.
func NormalizeMemoryReview(candidatePath, decisionPath string) ([]MemoryNormalizedCandidate, MemoryReviewReport, error) {
	candidates, candidateHash, err := LoadMemoryCandidates(candidatePath)
	if err != nil {
		return nil, MemoryReviewReport{}, err
	}
	decisions, err := LoadMemoryReviewDecisions(decisionPath)
	if err != nil {
		return nil, MemoryReviewReport{}, err
	}
	report := MemoryReviewReport{
		FormatVersion:        MemoryReviewFormatVersion,
		ReviewBatchID:        fmt.Sprintf("memory-review-%d", time.Now().UTC().UnixNano()),
		CandidateArtifactSHA: candidateHash,
		CandidateCount:       len(candidates),
		DecisionCount:        len(decisions),
	}
	byID := make(map[string]MemoryCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.CandidateID] = candidate
	}
	byDecision := make(map[string]MemoryReviewDecision, len(decisions))
	for _, decision := range decisions {
		byDecision[decision.CandidateID] = decision
	}
	for _, candidate := range candidates {
		decision, ok := byDecision[candidate.CandidateID]
		if !ok {
			report.Unreviewed++
			continue
		}
		if err := validateReviewDecision(decision, candidate); err != nil {
			report.InvalidDecisions++
			report.Diagnostics = appendReviewDiagnostic(report.Diagnostics, candidate.CandidateID+": "+err.Error())
			continue
		}
		switch decision.Action {
		case MemoryReviewReject:
			report.Rejected++
		case MemoryReviewDefer:
			report.Deferred++
		case MemoryReviewHistorical:
			report.Historical++
		case MemoryReviewSuperseded:
			report.Superseded++
		case MemoryReviewAccept, MemoryReviewReplace, MemoryReviewMerge:
			report.Accepted++
		}
		if normalized, normalizeErr := normalizeReviewedCandidate(candidate, decision, byID); normalizeErr != nil {
			report.InvalidDecisions++
			report.Diagnostics = appendReviewDiagnostic(report.Diagnostics, candidate.CandidateID+": "+normalizeErr.Error())
		} else if normalized != nil {
			report.ReadyForM4++
		}
	}
	// Rebuild normalized values in a second pass so final-key collision checks
	// cover the complete reviewed set and are deterministic.
	ready := make([]MemoryNormalizedCandidate, 0, report.ReadyForM4)
	for _, candidate := range candidates {
		decision, ok := byDecision[candidate.CandidateID]
		if !ok || !decisionIsNormalizing(decision.Action) {
			continue
		}
		normalized, normalizeErr := normalizeReviewedCandidate(candidate, decision, byID)
		if normalizeErr != nil || normalized == nil {
			continue
		}
		ready = append(ready, *normalized)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].CandidateID < ready[j].CandidateID })
	keyOwners := make(map[string]string)
	filtered := ready[:0]
	for _, normalized := range ready {
		if normalized.Target == MemoryReviewTargetDurableMemory || normalized.Target == MemoryReviewTargetUserProfile {
			key := string(normalized.Target) + "\x00" + normalized.Namespace + "\x00" + normalized.Key
			if previous, exists := keyOwners[key]; exists {
				previousItem := findNormalizedByCandidateID(filtered, previous)
				if previousItem == nil || previousItem.SupersededBy == "" {
					report.DuplicateFinalKeys++
					report.OpenConflicts++
					report.Diagnostics = appendReviewDiagnostic(report.Diagnostics, normalized.CandidateID+": final key conflicts with "+previous)
					continue
				}
			}
			keyOwners[key] = normalized.CandidateID
		}
		filtered = append(filtered, normalized)
	}
	report.ReadyForM4 = len(filtered)
	report.Diagnostics = uniqueStrings(report.Diagnostics)
	return filtered, report, nil
}

func LoadMemoryCandidates(path string) ([]MemoryCandidate, string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, "", errors.New("memory candidates path is required")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errors.New("inspect memory candidates")
	}
	if info.Size() > DefaultReviewMaxBytes {
		return nil, "", errors.New("memory candidates are too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", errors.New("read memory candidates")
	}
	defer file.Close()
	hash := sha256.New()
	reader := io.TeeReader(io.LimitReader(file, DefaultReviewMaxBytes+1), hash)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	candidates := make([]MemoryCandidate, 0)
	seen := make(map[string]struct{})
	for scanner.Scan() {
		var candidate MemoryCandidate
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&candidate); err != nil {
			return nil, "", fmt.Errorf("decode memory candidate: %w", err)
		}
		if err := validateMemoryCandidate(candidate); err != nil {
			// M2 candidates are review evidence, not trusted durable records.
			// Preserve provenance but redact unsafe source text so one rejected
			// candidate cannot abort an otherwise reviewable batch.
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "too large") {
				candidate.Content = ""
				candidate.Status = MemoryCandidateRejected
				candidate.Reason = "candidate rejected during M3 validation"
			} else {
				return nil, "", fmt.Errorf("validate memory candidate: %w", err)
			}
		}
		if _, exists := seen[candidate.CandidateID]; exists {
			return nil, "", fmt.Errorf("duplicate memory candidate %q", candidate.CandidateID)
		}
		seen[candidate.CandidateID] = struct{}{}
		candidates = append(candidates, candidate)
		if len(candidates) > MaxReviewDecisions {
			return nil, "", errors.New("memory candidates exceed review limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read memory candidates: %w", err)
	}
	return candidates, hex.EncodeToString(hash.Sum(nil)), nil
}

func LoadMemoryReviewDecisions(path string) ([]MemoryReviewDecision, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("inspect memory review decisions")
	}
	if info.Size() > DefaultReviewMaxBytes {
		return nil, errors.New("memory review decisions are too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("read memory review decisions")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	decisions := make([]MemoryReviewDecision, 0)
	seenDecision := make(map[string]struct{})
	seenCandidate := make(map[string]struct{})
	for scanner.Scan() {
		var decision MemoryReviewDecision
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&decision); err != nil {
			return nil, fmt.Errorf("decode memory review decision: %w", err)
		}
		if err := validateReviewDecisionShape(decision); err != nil {
			return nil, fmt.Errorf("validate memory review decision: %w", err)
		}
		if _, exists := seenDecision[decision.DecisionID]; exists {
			return nil, fmt.Errorf("duplicate memory decision %q", decision.DecisionID)
		}
		if _, exists := seenCandidate[decision.CandidateID]; exists {
			return nil, fmt.Errorf("multiple decisions for candidate %q; append-only correction is not implemented", decision.CandidateID)
		}
		seenDecision[decision.DecisionID] = struct{}{}
		seenCandidate[decision.CandidateID] = struct{}{}
		decisions = append(decisions, decision)
		if len(decisions) > MaxReviewDecisions {
			return nil, errors.New("memory review decisions exceed review limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read memory review decisions: %w", err)
	}
	return decisions, nil
}

func validateMemoryCandidate(candidate MemoryCandidate) error {
	if err := validateSafeID("candidate id", candidate.CandidateID, MaxMemoryIDLength); err != nil {
		return err
	}
	if candidate.Status == "" || candidate.Category == "" {
		return errors.New("candidate status and category are required")
	}
	if candidate.Source.InventoryID == "" || candidate.Source.RootLabel == "" || candidate.Source.RelativePath == "" || candidate.Source.FileSHA256 == "" || candidate.Source.LineStart < 1 || candidate.Source.LineEnd < candidate.Source.LineStart {
		return errors.New("candidate provenance is incomplete")
	}
	if candidate.ConfidenceSuggestion < 0 || candidate.ConfidenceSuggestion > 1 {
		return errors.New("candidate confidence is out of range")
	}
	if len(candidate.Content) > MaxMemoryTextLength {
		return errors.New("candidate content is too large")
	}
	if candidate.Content != "" && looksLikeSecret(candidate.Content) {
		return errors.New("candidate content looks like secret material")
	}
	return nil
}

func validateReviewDecisionShape(decision MemoryReviewDecision) error {
	if err := validateSafeID("decision id", decision.DecisionID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateSafeID("decision candidate id", decision.CandidateID, MaxMemoryIDLength); err != nil {
		return err
	}
	if !validReviewAction(decision.Action) {
		return fmt.Errorf("review action %q is invalid", decision.Action)
	}
	if err := validateSafeID("reviewer", decision.Reviewer, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateText("review reason", decision.Reason, MaxReviewReasonBytes, true); err != nil {
		return err
	}
	if decision.ReviewedAt.IsZero() {
		return errors.New("reviewed_at is required")
	}
	return nil
}

func validateReviewDecision(decision MemoryReviewDecision, candidate MemoryCandidate) error {
	if err := validateReviewDecisionShape(decision); err != nil {
		return err
	}
	if decision.CandidateID != candidate.CandidateID {
		return errors.New("decision candidate identity mismatch")
	}
	if decision.Action == MemoryReviewSuperseded && decision.ReplacementCandidateID == "" {
		return errors.New("superseded decision requires replacement_candidate_id")
	}
	if decision.Action == MemoryReviewMerge && decision.ReplacementCandidateID == "" {
		return errors.New("merge decision requires replacement_candidate_id")
	}
	return nil
}

func validReviewAction(action MemoryReviewAction) bool {
	switch action {
	case MemoryReviewAccept, MemoryReviewReject, MemoryReviewDefer, MemoryReviewHistorical, MemoryReviewSuperseded, MemoryReviewReplace, MemoryReviewMerge:
		return true
	}
	return false
}

func decisionIsNormalizing(action MemoryReviewAction) bool {
	return action == MemoryReviewAccept || action == MemoryReviewHistorical || action == MemoryReviewSuperseded || action == MemoryReviewReplace || action == MemoryReviewMerge
}

func normalizeReviewedCandidate(candidate MemoryCandidate, decision MemoryReviewDecision, byID map[string]MemoryCandidate) (*MemoryNormalizedCandidate, error) {
	if !decisionIsNormalizing(decision.Action) {
		return nil, nil
	}
	if candidate.Content == "" {
		return nil, errors.New("normalizing decision requires candidate content; duplicate candidates must target their canonical candidate")
	}
	target := decision.Target
	if target == "" {
		return nil, errors.New("normalizing decision requires explicit target")
	}
	if target != MemoryReviewTargetDurableMemory && target != MemoryReviewTargetUserProfile && target != MemoryReviewTargetIdentity {
		return nil, fmt.Errorf("review target %q is invalid", target)
	}
	namespace := decision.Namespace
	key := decision.Key
	if target != MemoryReviewTargetIdentity {
		if namespace == "" {
			namespace = candidate.NamespaceSuggestion
		}
		if key == "" {
			key = candidate.KeySuggestion
		}
		if err := validateDurableNamespace(namespace); err != nil {
			return nil, fmt.Errorf("normalized namespace: %w", err)
		}
		if err := validateSafeKey("normalized key", key, MaxMemoryKeyLength); err != nil {
			return nil, err
		}
	}
	content := decision.Content
	if content == "" {
		content = candidate.Content
	}
	if err := validateText("normalized content", content, MaxMemoryTextLength, true); err != nil {
		return nil, err
	}
	summary := decision.Summary
	if summary == "" {
		summary = candidate.SummarySuggestion
	}
	if err := validateText("normalized summary", summary, MaxMemorySummaryLength, false); err != nil {
		return nil, err
	}
	sensitivity := decision.Sensitivity
	if sensitivity == "" {
		sensitivity = candidate.SensitivitySuggestion
	}
	if sensitivity == MemorySensitivityUnspecified {
		sensitivity = MemorySensitivityPersonal
	}
	if !sensitivity.valid() || sensitivity == MemorySensitivitySecret {
		return nil, errors.New("normalized sensitivity is invalid or secret")
	}
	confidence := decision.Confidence
	if confidence == 0 {
		confidence = candidate.ConfidenceSuggestion
	}
	if confidence <= 0 || confidence > 1 {
		return nil, errors.New("normalized confidence must be between 0 and 1")
	}
	if decision.EffectiveAt.IsZero() {
		return nil, errors.New("effective_at is required for normalized memory")
	}
	lastConfirmed := decision.LastConfirmedAt
	if lastConfirmed.IsZero() {
		lastConfirmed = decision.ReviewedAt
	}
	if lastConfirmed.Before(decision.EffectiveAt) {
		return nil, errors.New("last_confirmed_at must not precede effective_at")
	}
	status := "active"
	if decision.Action == MemoryReviewHistorical {
		status = "historical"
	}
	if decision.Action == MemoryReviewSuperseded || decision.Action == MemoryReviewMerge {
		status = "superseded"
	}
	if decision.ExpiresAt.Before(lastConfirmed) && !decision.ExpiresAt.IsZero() {
		return nil, errors.New("expires_at must not precede last_confirmed_at")
	}
	normalized := &MemoryNormalizedCandidate{CandidateID: candidate.CandidateID, DecisionID: decision.DecisionID, Target: target, Namespace: namespace, Key: key, Summary: summary, Content: content, Sensitivity: sensitivity, Confidence: confidence, Status: status, EffectiveAt: decision.EffectiveAt.UTC(), LastConfirmedAt: lastConfirmed.UTC(), ExpiresAt: decision.ExpiresAt.UTC(), Source: candidate.Source}
	if decision.ReplacementCandidateID != "" {
		if _, exists := byID[decision.ReplacementCandidateID]; !exists {
			return nil, errors.New("replacement candidate does not exist")
		}
		normalized.SupersededBy = decision.ReplacementCandidateID
	}
	return normalized, nil
}

// AppendMemoryReviewDecision appends one explicit decision to a new/private
// decision log. It refuses duplicate decision or candidate IDs.
func AppendMemoryReviewDecision(path string, decision MemoryReviewDecision) error {
	if !filepath.IsAbs(path) {
		return errors.New("review decision path must be absolute")
	}
	if err := validateReviewDecisionShape(decision); err != nil {
		return err
	}
	decisions, err := LoadMemoryReviewDecisions(path)
	if err != nil {
		return err
	}
	for _, previous := range decisions {
		if previous.DecisionID == decision.DecisionID || previous.CandidateID == decision.CandidateID {
			return errors.New("review decision already exists for this decision or candidate")
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return json.NewEncoder(file).Encode(decision)
}

func WriteMemoryReviewArtifacts(normalizedPath, reportPath string, normalized []MemoryNormalizedCandidate, report MemoryReviewReport) error {
	if !filepath.IsAbs(normalizedPath) || !filepath.IsAbs(reportPath) || normalizedPath == reportPath {
		return errors.New("normalized and report paths must be distinct absolute paths")
	}
	for _, path := range []string{normalizedPath, reportPath} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("refusing to overwrite memory review artifact")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect memory review artifact")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(normalizedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	for _, item := range normalized {
		if err := json.NewEncoder(file).Encode(item); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(reportPath, data, 0o600)
}

func findNormalizedByCandidateID(values []MemoryNormalizedCandidate, candidateID string) *MemoryNormalizedCandidate {
	for index := range values {
		if values[index].CandidateID == candidateID {
			return &values[index]
		}
	}
	return nil
}

func appendReviewDiagnostic(values []string, value string) []string {
	if len(value) > 512 {
		value = value[:512]
	}
	if len(values) < 256 {
		return append(values, value)
	}
	return values
}
