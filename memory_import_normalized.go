package bee

import (
	"bufio"
	"bytes"
	"context"
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
	MemoryNormalizedImportFormatVersion = 1
	MaxNormalizedImportRecords          = 100_000
	MaxNormalizedImportBytes            = 256 << 20
	MaxImportReceiptBytes               = 4 << 20
	NormalizedImportActorID             = "bee-m4-import"
)

type NormalizedImportOptions struct {
	NormalizedPath   string
	ReviewReportPath string
	MemoryRoot       string
	AuditID          string
	Reason           string
	Apply            bool
}

type NormalizedImportConflict struct {
	Category string `json:"category"`
	ID       string `json:"id"`
	Reason   string `json:"reason"`
}

type NormalizedImportReport struct {
	FormatVersion         int                        `json:"format_version"`
	BatchID               string                     `json:"batch_id"`
	NormalizedArtifactSHA string                     `json:"normalized_artifact_sha256"`
	ReviewBatchID         string                     `json:"review_batch_id"`
	DryRun                bool                       `json:"dry_run"`
	Valid                 bool                       `json:"valid"`
	Applied               bool                       `json:"applied"`
	RecordsSeen           int                        `json:"records_seen"`
	RecordsReady          int                        `json:"records_ready"`
	RecordsImported       int                        `json:"records_imported"`
	UnsupportedTargets    int                        `json:"unsupported_targets"`
	Conflicts             []NormalizedImportConflict `json:"conflicts,omitempty"`
	Errors                []string                   `json:"errors,omitempty"`
	PreflightMemoryRoot   string                     `json:"memory_root,omitempty"`
	CreatedAt             time.Time                  `json:"created_at"`
	CompletedAt           time.Time                  `json:"completed_at"`
}

// ImportNormalizedMemory performs M4 validation and optionally applies the
// reviewed normalized artifact through LocalMemoryService.Import. Dry-run is
// side-effect free when the target memory root does not already exist.
func ImportNormalizedMemory(ctx context.Context, options NormalizedImportOptions) (NormalizedImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := NormalizedImportReport{FormatVersion: MemoryNormalizedImportFormatVersion, BatchID: "m4-" + fmt.Sprint(time.Now().UTC().UnixNano()), DryRun: !options.Apply, PreflightMemoryRoot: options.MemoryRoot, CreatedAt: time.Now().UTC()}
	if err := validateNormalizedImportOptions(options); err != nil {
		report.Errors = []string{err.Error()}
		report.CompletedAt = time.Now().UTC()
		return report, err
	}
	normalized, normalizedHash, err := LoadNormalizedMemoryCandidates(options.NormalizedPath)
	if err != nil {
		report.Errors = []string{err.Error()}
		report.CompletedAt = time.Now().UTC()
		return report, err
	}
	report.NormalizedArtifactSHA = normalizedHash
	report.RecordsSeen = len(normalized)
	review, err := LoadMemoryReviewReport(options.ReviewReportPath)
	if err != nil {
		report.Errors = []string{err.Error()}
		report.CompletedAt = time.Now().UTC()
		return report, err
	}
	report.ReviewBatchID = review.ReviewBatchID
	if review.ReadyForM4 != len(normalized) || review.InvalidDecisions != 0 || review.OpenConflicts != 0 {
		report.Errors = append(report.Errors, "M3 review report does not authorize this normalized artifact")
	}
	policy := DefaultMemoryPolicy()
	export, conversionErrors := normalizedMemoryExport(normalized, options, policy)
	report.Errors = append(report.Errors, conversionErrors...)
	report.RecordsReady = len(export.Memory)
	if len(report.Errors) == 0 && len(export.Memory) == 0 {
		report.Valid = true
	}
	if len(report.Errors) == 0 {
		conflicts, preflightErrors := preflightNormalizedImport(ctx, options.MemoryRoot, export, policy)
		report.Conflicts = append(report.Conflicts, conflicts...)
		report.Errors = append(report.Errors, preflightErrors...)
		report.Valid = len(report.Errors) == 0 && len(report.Conflicts) == 0
	}
	if report.Valid && options.Apply {
		service, serviceErr := NewLocalMemoryService(options.MemoryRoot, policy)
		if serviceErr != nil {
			report.Errors = append(report.Errors, serviceErr.Error())
			report.Valid = false
		} else {
			result, importErr := service.Import(ctx, MemoryImportRequest{Actor: MemoryActor{Kind: MemoryActorParent, ID: NormalizedImportActorID}, Export: &export, DryRun: false, AuditID: options.AuditID, Reason: options.Reason})
			if importErr != nil {
				report.Errors = append(report.Errors, importErr.Error())
				report.Valid = false
			} else {
				report.Applied = result.Applied
				report.RecordsImported = result.Memory
			}
		}
	}
	if report.Valid && !options.Apply {
		report.RecordsImported = 0
	}
	report.CompletedAt = time.Now().UTC()
	if !report.Valid {
		return report, errors.New("normalized memory import validation failed")
	}
	return report, nil
}

func validateNormalizedImportOptions(options NormalizedImportOptions) error {
	if strings.TrimSpace(options.NormalizedPath) == "" || strings.TrimSpace(options.ReviewReportPath) == "" {
		return errors.New("normalized and review report paths are required")
	}
	if strings.TrimSpace(options.MemoryRoot) == "" || !filepath.IsAbs(options.MemoryRoot) {
		return errors.New("memory root must be an absolute path")
	}
	if err := validateSafeID("memory audit_id", options.AuditID, MaxMemoryAuditIDLength); err != nil {
		return err
	}
	if err := validateText("memory audit reason", options.Reason, MaxMemoryReasonLength, true); err != nil {
		return err
	}
	return nil
}

func LoadNormalizedMemoryCandidates(path string) ([]MemoryNormalizedCandidate, string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errors.New("inspect normalized memory artifact")
	}
	if info.Size() > MaxNormalizedImportBytes {
		return nil, "", errors.New("normalized memory artifact is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", errors.New("read normalized memory artifact")
	}
	defer file.Close()
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hash))
	scanner.Buffer(make([]byte, 4096), MaxMemoryTextLength+1)
	items := make([]MemoryNormalizedCandidate, 0)
	seen := make(map[string]struct{})
	for scanner.Scan() {
		var item MemoryNormalizedCandidate
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&item); err != nil {
			return nil, "", fmt.Errorf("decode normalized memory record: %w", err)
		}
		if err := validateNormalizedCandidate(item); err != nil {
			return nil, "", fmt.Errorf("validate normalized memory record: %w", err)
		}
		if _, exists := seen[item.CandidateID]; exists {
			return nil, "", fmt.Errorf("duplicate normalized candidate %q", item.CandidateID)
		}
		seen[item.CandidateID] = struct{}{}
		items = append(items, item)
		if len(items) > MaxNormalizedImportRecords {
			return nil, "", errors.New("normalized memory artifact exceeds record limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read normalized memory artifact: %w", err)
	}
	return items, hex.EncodeToString(hash.Sum(nil)), nil
}

func LoadMemoryReviewReport(path string) (MemoryReviewReport, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return MemoryReviewReport{}, errors.New("inspect memory review report")
	}
	if info.Size() > MaxImportReceiptBytes {
		return MemoryReviewReport{}, errors.New("memory review report is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return MemoryReviewReport{}, errors.New("read memory review report")
	}
	var report MemoryReviewReport
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return MemoryReviewReport{}, fmt.Errorf("decode memory review report: %w", err)
	}
	if report.FormatVersion != MemoryReviewFormatVersion || report.ReviewBatchID == "" {
		return MemoryReviewReport{}, errors.New("invalid memory review report")
	}
	return report, nil
}

func validateNormalizedCandidate(item MemoryNormalizedCandidate) error {
	if err := validateSafeID("normalized candidate_id", item.CandidateID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateSafeID("normalized decision_id", item.DecisionID, MaxMemoryIDLength); err != nil {
		return err
	}
	if item.Target != MemoryReviewTargetDurableMemory {
		return fmt.Errorf("normalized target %q is not supported by M4 durable import", item.Target)
	}
	if err := validateDurableNamespace(item.Namespace); err != nil {
		return err
	}
	if err := validateSafeKey("normalized key", item.Key, MaxMemoryKeyLength); err != nil {
		return err
	}
	if err := validateText("normalized content", item.Content, MaxMemoryTextLength, true); err != nil {
		return err
	}
	if err := validateText("normalized summary", item.Summary, MaxMemorySummaryLength, false); err != nil {
		return err
	}
	if !item.Sensitivity.valid() || item.Sensitivity == MemorySensitivitySecret {
		return errors.New("normalized sensitivity is invalid or secret")
	}
	if err := DefaultMemoryPolicy().allowsSensitivity(item.Sensitivity); err != nil {
		return err
	}
	if item.Confidence <= 0 || item.Confidence > 1 {
		return errors.New("normalized confidence is out of range")
	}
	if item.EffectiveAt.IsZero() || item.LastConfirmedAt.IsZero() || item.LastConfirmedAt.Before(item.EffectiveAt) {
		return errors.New("normalized timestamps are invalid")
	}
	if !item.ExpiresAt.IsZero() && item.ExpiresAt.Before(item.LastConfirmedAt) {
		return errors.New("normalized expiration precedes confirmation")
	}
	if item.Source.InventoryID == "" || item.CandidateID == "" || item.Source.RootLabel == "" || item.Source.RelativePath == "" || item.Source.FileSHA256 == "" || item.Source.ContentSHA256 == "" || item.Source.LineStart < 1 || item.Source.LineEnd < item.Source.LineStart {
		return errors.New("normalized provenance is incomplete")
	}
	return nil
}

func normalizedMemoryExport(items []MemoryNormalizedCandidate, options NormalizedImportOptions, policy MemoryPolicy) (MemoryExport, []string) {
	export := MemoryExport{FormatVersion: 1}
	errs := make([]string, 0)
	ids := make(map[string]struct{})
	for _, item := range items {
		if item.Target != MemoryReviewTargetDurableMemory {
			errs = append(errs, fmt.Sprintf("%s: target %q is unsupported", item.CandidateID, item.Target))
			continue
		}
		record, err := normalizedToMemoryRecord(item, options)
		if err != nil {
			errs = append(errs, item.CandidateID+": "+err.Error())
			continue
		}
		if _, exists := ids[record.ID]; exists {
			errs = append(errs, "duplicate deterministic record id "+record.ID)
			continue
		}
		ids[record.ID] = struct{}{}
		if err := record.Validate(policy); err != nil {
			errs = append(errs, item.CandidateID+": "+err.Error())
			continue
		}
		export.Memory = append(export.Memory, record)
	}
	sort.Slice(export.Memory, func(i, j int) bool { return export.Memory[i].ID < export.Memory[j].ID })
	return export, errs
}

func normalizedToMemoryRecord(item MemoryNormalizedCandidate, options NormalizedImportOptions) (MemoryRecord, error) {
	hash := sha256.Sum256([]byte("bee-m4-record\x00" + item.CandidateID + "\x00" + item.DecisionID + "\x00" + item.Namespace + "\x00" + item.Key))
	recordID := "m4-" + hex.EncodeToString(hash[:])[:32]
	tags := map[string]string{"m4_status": item.Status, "m4_candidate": item.CandidateID, "m4_decision": item.DecisionID}
	if item.SupersededBy != "" {
		tags["m4_superseded_by_candidate"] = item.SupersededBy
	}
	status := MemoryStatus(item.Status)
	if status == "" {
		status = MemoryStatusActive
	}
	source := compactImportSource(item)
	provenance := &MemoryProvenance{InventoryID: item.Source.InventoryID, CandidateID: item.CandidateID, DecisionID: item.DecisionID, RootLabel: item.Source.RootLabel, RelativePath: item.Source.RelativePath, FileSHA256: item.Source.FileSHA256, ContentSHA256: item.Source.ContentSHA256, LineStart: item.Source.LineStart, LineEnd: item.Source.LineEnd, HeadingPath: append([]string(nil), item.Source.HeadingPath...)}
	record := MemoryRecord{ID: recordID, Namespace: MemoryNamespace(item.Namespace), Key: item.Key, Content: item.Content, Summary: item.Summary, Sensitivity: item.Sensitivity, Tags: tags, Source: source, CreatedAt: item.EffectiveAt.UTC(), UpdatedAt: item.LastConfirmedAt.UTC(), ExpiresAt: item.ExpiresAt.UTC(), Confidence: item.Confidence, Version: 1, CreatedBy: MemoryActor{Kind: MemoryActorParent, ID: NormalizedImportActorID}, Status: status, SupersededBy: item.SupersededBy, Provenance: provenance}
	return record, nil
}

func compactImportSource(item MemoryNormalizedCandidate) string {
	value := fmt.Sprintf("m4:%s:%s:%s:%d-%d", item.CandidateID, item.Source.RootLabel, item.Source.RelativePath, item.Source.LineStart, item.Source.LineEnd)
	if len(value) > MaxMemorySourceLength {
		value = value[:MaxMemorySourceLength]
	}
	return value
}

func preflightNormalizedImport(ctx context.Context, root string, export MemoryExport, policy MemoryPolicy) ([]NormalizedImportConflict, []string) {
	if info, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil || !info.IsDir() {
		return nil, []string{"memory root is unavailable"}
	}
	// Conflict detection is an inventory operation: expired-by-retention
	// records still own their deterministic IDs and namespace/keys.
	policy.Retention = 0
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		return nil, []string{err.Error()}
	}
	current, err := store.ListRecords(ctx, MemoryActor{Kind: MemoryActorParent, ID: NormalizedImportActorID}, false)
	if err != nil {
		return nil, []string{err.Error()}
	}
	byID := make(map[string]MemoryRecord, len(current))
	byKey := make(map[string]MemoryRecord, len(current))
	for _, record := range current {
		byID[record.ID] = record
		byKey[string(record.Namespace)+"\x00"+record.Key] = record
	}
	conflicts := make([]NormalizedImportConflict, 0)
	for _, record := range export.Memory {
		if _, ok := byID[record.ID]; ok {
			conflicts = append(conflicts, NormalizedImportConflict{Category: "memory", ID: record.ID, Reason: "deterministic record ID already exists"})
			continue
		}
		if existing, ok := byKey[string(record.Namespace)+"\x00"+record.Key]; ok {
			conflicts = append(conflicts, NormalizedImportConflict{Category: "memory", ID: record.ID, Reason: "namespace/key already exists as " + existing.ID})
		}
	}
	return conflicts, nil
}

// WriteNormalizedImportReceipt writes a new 0600 M4 plan/receipt artifact.
func WriteNormalizedImportReceipt(path string, report NormalizedImportReport) error {
	if !filepath.IsAbs(path) {
		return errors.New("import receipt path must be absolute")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("refusing to overwrite import receipt")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect import receipt")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
