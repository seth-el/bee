package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seth-el/bee"
)

const cliMemoryActorID = "bee-cli-parent"

type memoryReadPolicyOptions struct {
	includeSensitive bool
	retentionDays    *int
}

func (options memoryReadPolicyOptions) policy() (bee.MemoryPolicy, error) {
	policy := bee.DefaultMemoryPolicy()
	if options.includeSensitive {
		policy.AllowSensitiveMemory = true
	}
	if options.retentionDays != nil {
		days := int64(*options.retentionDays)
		if days > math.MaxInt64/int64(24*time.Hour) {
			return bee.MemoryPolicy{}, errors.New("--retention-days is too large")
		}
		policy.Retention = time.Duration(days) * 24 * time.Hour
	}
	return policy, nil
}

// parseMemoryReadPolicyFlags removes the read-only policy overrides before the
// command-specific flag set parses its arguments. Mutating commands never call
// this helper, so these flags remain unavailable to import/export paths.
func parseMemoryReadPolicyFlags(command string, args []string) (memoryReadPolicyOptions, []string, error) {
	if command != "list" && command != "show" && command != "search" {
		return memoryReadPolicyOptions{}, args, nil
	}
	var options memoryReadPolicyOptions
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--include-sensitive":
			options.includeSensitive = true
		case strings.HasPrefix(arg, "--include-sensitive="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--include-sensitive="))
			if err != nil {
				return memoryReadPolicyOptions{}, nil, errors.New("--include-sensitive must be true or false")
			}
			options.includeSensitive = value
		case arg == "--retention-days":
			if index+1 >= len(args) {
				return memoryReadPolicyOptions{}, nil, errors.New("--retention-days requires a nonnegative integer")
			}
			index++
			value, err := parseMemoryRetentionDays(args[index])
			if err != nil {
				return memoryReadPolicyOptions{}, nil, err
			}
			options.retentionDays = &value
		case strings.HasPrefix(arg, "--retention-days="):
			value, err := parseMemoryRetentionDays(strings.TrimPrefix(arg, "--retention-days="))
			if err != nil {
				return memoryReadPolicyOptions{}, nil, err
			}
			options.retentionDays = &value
		default:
			remaining = append(remaining, arg)
		}
	}
	return options, remaining, nil
}

func parseMemoryRetentionDays(value string) (int, error) {
	days, err := strconv.ParseInt(value, 10, 64)
	if err != nil || days < 0 || days > int64(int(^uint(0)>>1)) {
		return 0, errors.New("--retention-days must be a nonnegative integer")
	}
	return int(days), nil
}

func runMemory(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("memory command is required: list, show, set-status, search, doctor, mood-sweep, proposals, export, import, or source-archive")
	}
	if args[0] == "source-archive" {
		return runMemorySourceArchive(args[1:], output, errorOutput)
	}
	memoryRoot, rest, err := memoryRootFlag(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("memory command is required: list, show, set-status, search, doctor, mood-sweep, proposals, export, import, or source-archive")
	}
	if rest[0] == "mood-sweep" {
		return memoryMoodSweep(memoryRoot, rest[1:], output, errorOutput)
	}
	readOptions, commandArgs, err := parseMemoryReadPolicyFlags(rest[0], rest[1:])
	if err != nil {
		return err
	}
	policy, err := readOptions.policy()
	if err != nil {
		return err
	}
	// Read commands may inspect stores containing older non-secret sensitive
	// records, but the request-level IncludeSensitive flag remains the actual
	// exposure gate. Mutating commands retain the default write policy.
	if rest[0] == "list" || rest[0] == "show" || rest[0] == "search" || rest[0] == "doctor" || rest[0] == "proposals" {
		policy.AllowSensitiveMemory = true
	}
	if rest[0] == "doctor" {
		policy.Retention = 0
	}
	service, err := bee.NewLocalMemoryService(memoryRoot, policy)
	if err != nil {
		return err
	}
	actor := bee.MemoryActor{Kind: bee.MemoryActorParent, ID: cliMemoryActorID}
	switch rest[0] {
	case "list":
		return memoryList(service, actor, commandArgs, readOptions.includeSensitive, output, errorOutput)
	case "show":
		return memoryShow(service, actor, commandArgs, readOptions.includeSensitive, output, errorOutput)
	case "set-status":
		return memorySetStatus(service, actor, commandArgs, output, errorOutput)
	case "search":
		return memorySearch(service, actor, memoryRoot, commandArgs, readOptions.includeSensitive, output, errorOutput)
	case "doctor":
		return memoryDoctor(service, actor, memoryRoot, rest[1:], output, errorOutput)
	case "proposals":
		return memoryProposals(service, actor, rest[1:], output, errorOutput)
	case "export":
		return memoryExport(service, actor, rest[1:], output, errorOutput)
	case "import":
		return memoryImport(service, actor, rest[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown memory command %q", rest[0])
	}
}

func memoryMoodSweep(root string, args []string, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory mood-sweep", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory mood-sweep does not accept positional arguments")
	}
	manager, err := bee.NewMoodManager(root)
	if err != nil {
		return err
	}
	report, err := manager.Consolidate(context.Background(), time.Now().UTC())
	if err != nil {
		return err
	}
	return encodeValue(output, report)
}

type memoryDoctorReport struct {
	Root                string                   `json:"root"`
	EventLogPresent     bool                     `json:"event_log_present"`
	Events              int                      `json:"events"`
	Records             int                      `json:"records"`
	Active              int                      `json:"active"`
	Historical          int                      `json:"historical"`
	Superseded          int                      `json:"superseded"`
	Tombstones          int                      `json:"tombstones"`
	PendingProposals    int                      `json:"pending_proposals"`
	SourceFiles         int                      `json:"source_files"`
	SourceBytes         int64                    `json:"source_bytes"`
	Semantic            bee.SemanticHealthStatus `json:"semantic"`
	Warnings            []string                 `json:"warnings,omitempty"`
	SourceScanTruncated bool                     `json:"source_scan_truncated,omitempty"`
}

func memoryDoctor(service *bee.LocalMemoryService, actor bee.MemoryActor, root string, args []string, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory doctor", errorOutput)
	semanticIndex := flags.String("semantic-index", "", "semantic index root (default: ~/.bee/state/memory-semantic)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory doctor does not accept positional arguments")
	}
	store, ok := service.Memory.(*bee.LocalMemoryStore)
	if !ok {
		return errors.New("memory doctor requires the local memory store")
	}
	root = store.Root()
	diagnostics, err := store.ReplayDiagnostics()
	if err != nil {
		return err
	}
	records, err := store.ListRecords(context.Background(), actor, true)
	if err != nil {
		return err
	}
	pending, err := service.ListProposals(context.Background(), bee.ListProposalRequest{Actor: actor, IncludeSensitive: true, Bounds: bee.MemoryDisplayBounds{}})
	if err != nil {
		return err
	}
	report := memoryDoctorReport{Root: root, Events: diagnostics.Events, Records: len(records), Tombstones: diagnostics.Tombstones, PendingProposals: pending.Total}
	if _, err := os.Stat(filepath.Join(root, "events.jsonl")); err == nil {
		report.EventLogPresent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, record := range records {
		switch record.Status {
		case bee.MemoryStatusHistorical:
			report.Historical++
		case bee.MemoryStatusSuperseded:
			report.Superseded++
		default:
			report.Active++
		}
	}
	const maxSourceFiles = 100_000
	activeFiles := map[string]struct{}{"events.jsonl": {}, "proposals.jsonl": {}, "proposal_reviews.jsonl": {}, "identity.json": {}, "manifest.json": {}, "vectors.jsonl": {}}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if _, active := activeFiles[entry.Name()]; active || filepath.Base(filepath.Dir(path)) == "profiles" && filepath.Ext(path) == ".json" {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".markdown", ".txt", ".ndjson":
		default:
			return nil
		}
		if report.SourceFiles >= maxSourceFiles {
			report.SourceScanTruncated = true
			return filepath.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		report.SourceFiles++
		report.SourceBytes += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	indexRoot := strings.TrimSpace(*semanticIndex)
	if indexRoot == "" {
		indexRoot, err = bee.DefaultBeeSemanticIndexRoot()
	} else {
		indexRoot, err = bee.ResolveBeeHomePath(indexRoot)
	}
	if err != nil {
		return err
	}
	if manifest, verifyErr := bee.VerifySemanticIndex(root, indexRoot); verifyErr == nil {
		code := bee.SemanticHealthReady
		if manifest.RecordCount == 0 {
			code = bee.SemanticHealthEmpty
		}
		report.Semantic = bee.SemanticHealthStatus{Code: code, Enabled: true, IndexRoot: indexRoot, Model: manifest.Model, RecordCount: manifest.RecordCount}
	} else if _, statusErr := bee.SemanticIndexStatus(indexRoot); statusErr == nil {
		report.Semantic = bee.SemanticHealthStatus{Code: bee.SemanticHealthStale, Enabled: true, IndexRoot: indexRoot}
	} else if _, statErr := os.Lstat(indexRoot); statErr == nil {
		report.Semantic = bee.SemanticHealthStatus{Code: bee.SemanticHealthInvalid, Enabled: true, IndexRoot: indexRoot}
	} else {
		report.Semantic = bee.SemanticHealthStatus{Code: bee.SemanticHealthMissing, IndexRoot: indexRoot}
	}
	if report.SourceFiles > 0 && report.Records == 0 {
		report.Warnings = append(report.Warnings, "source corpus is present but the active durable-memory store is empty; source files are not searched until reviewed and imported")
	}
	if report.PendingProposals > 0 {
		report.Warnings = append(report.Warnings, "pending memory proposals require explicit review")
	}
	if report.Semantic.Code == bee.SemanticHealthStale || report.Semantic.Code == bee.SemanticHealthInvalid {
		report.Warnings = append(report.Warnings, "semantic index is stale or invalid; lexical retrieval remains available")
	}
	return encodeValue(output, report)
}

func memoryProposals(service bee.MemoryService, actor bee.MemoryActor, args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("memory proposals command is required: list, show, approve, or reject")
	}
	switch args[0] {
	case "list":
		flags := newFlagSet("bee memory proposals list", errorOutput)
		includeSensitive := flags.Bool("include-sensitive", false, "include sensitive proposal content")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("memory proposals list does not accept positional arguments")
		}
		result, err := service.ListProposals(context.Background(), bee.ListProposalRequest{Actor: actor, IncludeSensitive: *includeSensitive, Bounds: bee.MemoryDisplayBounds{}})
		if err != nil {
			return err
		}
		return encodeValue(output, result)
	case "show":
		flags := newFlagSet("bee memory proposals show", errorOutput)
		id := flags.String("id", "", "proposal ID")
		includeSensitive := flags.Bool("include-sensitive", false, "include sensitive proposal content")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" || flags.NArg() != 0 {
			return errors.New("memory proposals show requires --id and no positional arguments")
		}
		result, err := service.ShowProposal(context.Background(), bee.ShowProposalRequest{Actor: actor, ProposalID: *id, IncludeSensitive: *includeSensitive, Bounds: bee.MemoryDisplayBounds{}})
		if err != nil {
			return err
		}
		return encodeValue(output, result)
	case "approve", "reject":
		flags := newFlagSet("bee memory proposals "+args[0], errorOutput)
		id := flags.String("id", "", "proposal ID")
		auditID := flags.String("audit-id", "", "explicit audit ID")
		reason := flags.String("reason", "", "explicit review reason")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" || *auditID == "" || *reason == "" || flags.NArg() != 0 {
			return fmt.Errorf("memory proposals %s requires --id, --audit-id, --reason, and no positional arguments", args[0])
		}
		if args[0] == "approve" {
			record, err := service.ApproveProposal(context.Background(), bee.ApproveProposalRequest{Actor: actor, ProposalID: *id, AuditID: *auditID, Reason: *reason})
			if err != nil {
				return err
			}
			return encodeValue(output, map[string]any{"approved": true, "proposal_id": *id, "record_id": record.ID})
		}
		if err := service.RejectProposal(context.Background(), bee.RejectProposalRequest{Actor: actor, ProposalID: *id, AuditID: *auditID, Reason: *reason}); err != nil {
			return err
		}
		return encodeValue(output, map[string]any{"rejected": true, "proposal_id": *id})
	default:
		return fmt.Errorf("unknown memory proposals command %q", args[0])
	}
}

func memoryRootFlag(args []string) (string, []string, error) {
	root := ""
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--memory-root" {
			if index+1 >= len(args) {
				return "", nil, errors.New("--memory-root requires a path")
			}
			root = args[index+1]
			index++
			continue
		}
		remaining = append(remaining, args[index])
	}
	if root == "" {
		if os.Getenv("BEE_HOME") != "" || os.Getenv("XDG_DATA_HOME") == "" {
			if _, err := bee.EnsureBeeLayout(); err != nil {
				return "", nil, err
			}
		}
		var err error
		root, err = bee.DefaultBeeMemoryRoot()
		if err != nil {
			return "", nil, err
		}
	}
	return root, remaining, nil
}

func memoryList(service bee.MemoryService, actor bee.MemoryActor, args []string, includeSensitive bool, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory list", errorOutput)
	namespace := flags.String("namespace", string(bee.MemoryNamespaceDurableMemory), "memory namespace")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory list does not accept positional arguments")
	}
	result, err := service.List(context.Background(), bee.ListMemoryRequest{Actor: actor, Namespace: bee.MemoryNamespace(*namespace), IncludeSensitive: includeSensitive, Bounds: bee.MemoryDisplayBounds{}})
	if err != nil {
		return err
	}
	return encodeValue(output, result)
}

func memoryShow(service bee.MemoryService, actor bee.MemoryActor, args []string, includeSensitive bool, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory show", errorOutput)
	id := flags.String("id", "", "memory record ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" || flags.NArg() != 0 {
		return errors.New("memory show requires --id and no positional arguments")
	}
	result, err := service.Show(context.Background(), bee.ShowMemoryRequest{Actor: actor, RecordID: *id, IncludeSensitive: includeSensitive, Bounds: bee.MemoryDisplayBounds{}})
	if err != nil {
		return err
	}
	return encodeValue(output, result)
}

// memorySetStatus marks records historical (true once, no longer describing
// the present) or active again. Historical records leave automatic recall and
// default searches but are never deleted.
func memorySetStatus(service bee.MemoryService, actor bee.MemoryActor, args []string, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory set-status", errorOutput)
	ids := flags.String("id", "", "memory record ID, or several separated by commas")
	status := flags.String("status", "", "historical or active")
	reason := flags.String("reason", "", "why the status is changing (recorded in the event log)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *ids == "" || *reason == "" || flags.NArg() != 0 {
		return errors.New("memory set-status requires --id, --status, --reason and no positional arguments")
	}
	if *status != string(bee.MemoryStatusHistorical) && *status != string(bee.MemoryStatusActive) {
		return errors.New("memory set-status --status must be historical or active")
	}
	editor, ok := service.(bee.MemoryFieldEditor)
	if !ok {
		return errors.New("memory service does not support status edits")
	}
	type outcome struct {
		RecordID string `json:"record_id"`
		Key      string `json:"key,omitempty"`
		Status   string `json:"status,omitempty"`
		Changed  bool   `json:"changed"`
		Error    string `json:"error,omitempty"`
	}
	var outcomes []outcome
	failed := 0
	for _, id := range strings.Split(*ids, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		result := outcome{RecordID: id}
		shown, err := service.Show(context.Background(), bee.ShowMemoryRequest{Actor: actor, RecordID: id, Bounds: bee.MemoryDisplayBounds{}})
		if err == nil {
			current := shown.Record
			result.Key = current.Key
			currentStatus := current.Status
			if currentStatus == "" {
				currentStatus = bee.MemoryStatusActive
			}
			result.Status = string(currentStatus)
			if string(currentStatus) != *status {
				var edited bee.MemoryRecord
				edited, err = editor.EditFields(context.Background(), bee.EditMemoryFieldsRequest{Actor: actor, RecordID: id, ExpectedVersion: current.Version, Fields: map[string]any{"status": *status}, Mode: bee.MemoryWriteExplicit, AuditID: fmt.Sprintf("memory-set-status-%d", time.Now().UnixNano()), Reason: *reason})
				if err == nil {
					result.Status = string(edited.Status)
					result.Changed = true
				}
			}
		}
		if err != nil {
			result.Error = err.Error()
			failed++
		}
		outcomes = append(outcomes, result)
	}
	if err := encodeValue(output, outcomes); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("memory set-status: %d of %d records failed", failed, len(outcomes))
	}
	return nil
}

func memorySearch(service bee.MemoryService, actor bee.MemoryActor, memoryRoot string, args []string, includeSensitive bool, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory search", errorOutput)
	query := flags.String("query", "", "bounded lexical memory query")
	asOf := flags.String("as-of", "", "RFC3339 point-in-time bound")
	since := flags.String("since", "", "RFC3339 inclusive updated-at lower bound")
	before := flags.String("before", "", "RFC3339 exclusive updated-at upper bound")
	currentOnly := flags.Bool("current-only", false, "return only active, non-superseded memory")
	semantic := flags.Bool("semantic", false, "enable optional local semantic retrieval")
	semanticEndpoint := flags.String("semantic-endpoint", bee.DefaultSemanticEndpoint, "local OpenAI-compatible embeddings endpoint")
	semanticModel := flags.String("semantic-model", bee.DefaultSemanticModel, "local embedding model")
	semanticIndex := flags.String("semantic-index", "", "semantic index root (default: ~/.bee/state/memory-semantic)")
	jsonOutput := flags.Bool("json", false, "emit the {\"results\": [...]} context-bridge contract instead of the full search result")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *query == "" || flags.NArg() != 0 {
		return errors.New("memory search requires --query and no positional arguments")
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
	asOfTime, err := parseTime(*asOf, "as-of")
	if err != nil {
		return err
	}
	sinceTime, err := parseTime(*since, "since")
	if err != nil {
		return err
	}
	beforeTime, err := parseTime(*before, "before")
	if err != nil {
		return err
	}
	options := bee.MemoryRetrievalOptions{SemanticEnabled: *semantic}
	if *semantic {
		indexRoot := *semanticIndex
		if indexRoot == "" {
			indexRoot, err = bee.DefaultBeeSemanticIndexRoot()
			if err != nil {
				return err
			}
		}
		options = (bee.SemanticConfig{Enabled: true, Endpoint: *semanticEndpoint, Model: *semanticModel, IndexRoot: indexRoot, TimeoutSeconds: 15}).RetrievalOptions(memoryRoot)
	}
	result, err := service.Search(context.Background(), bee.SearchMemoryRequest{Query: bee.MemoryQuery{Actor: actor, Text: *query, AsOf: asOfTime, Since: sinceTime, Before: beforeTime, CurrentOnly: *currentOnly, IncludeSensitive: includeSensitive}, Options: options, Bounds: bee.MemoryDisplayBounds{}})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeMemorySearchJSON(output, result)
	}
	return encodeValue(output, result)
}

func memoryExport(service bee.MemoryService, actor bee.MemoryActor, args []string, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory export", errorOutput)
	destination := flags.String("destination", "", "destination JSON export path")
	auditID := flags.String("audit-id", "", "explicit audit ID")
	reason := flags.String("reason", "", "explicit export reason")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *destination == "" || *auditID == "" || *reason == "" || flags.NArg() != 0 {
		return errors.New("memory export requires --destination, --audit-id, --reason, and no positional arguments")
	}
	export, err := service.Export(context.Background(), bee.MemoryExportRequest{Actor: actor, AuditID: *auditID, Reason: *reason})
	if err != nil {
		return err
	}
	data, err := bee.MemoryExportJSON(export)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*destination, data, 0600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "exported memory to %s\n", *destination)
	return err
}

func memoryImport(service bee.MemoryService, actor bee.MemoryActor, args []string, output, errorOutput io.Writer) error {
	flags := newFlagSet("bee memory import", errorOutput)
	source := flags.String("source", "", "JSON memory export source path")
	apply := flags.Bool("apply", false, "apply the validated import; default is dry-run")
	auditID := flags.String("audit-id", "", "explicit audit ID")
	reason := flags.String("reason", "", "explicit import reason")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" || *auditID == "" || *reason == "" || flags.NArg() != 0 {
		return errors.New("memory import requires --source, --audit-id, --reason, and no positional arguments")
	}
	info, err := os.Stat(*source)
	if err != nil {
		return err
	}
	if info.Size() > bee.MaxMemoryExportBytes {
		return errors.New("memory import source exceeds bounded size")
	}
	payload, err := os.ReadFile(*source)
	if err != nil {
		return err
	}
	result, err := service.Import(context.Background(), bee.MemoryImportRequest{Actor: actor, Payload: payload, DryRun: !*apply, AuditID: *auditID, Reason: *reason})
	if err != nil {
		return err
	}
	return encodeValue(output, result)
}
