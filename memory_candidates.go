package bee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	MemoryCandidateFormatVersion    = 1
	DefaultCandidateMaxFiles        = 2_000
	DefaultCandidateMaxSections     = 5_000
	DefaultCandidateMaxSectionBytes = 32 << 10
	DefaultCandidateMaxTotalBytes   = 64 << 20
	MaxCandidateMaxFiles            = 100_000
	MaxCandidateMaxSections         = 100_000
	MaxCandidateSectionBytes        = 4 << 20
	MaxCandidateTotalBytes          = 1 << 30
)

type MemoryCandidateStatus string

const (
	MemoryCandidateCandidate      MemoryCandidateStatus = "candidate"
	MemoryCandidateReviewRequired MemoryCandidateStatus = "review_required"
	MemoryCandidateRejected       MemoryCandidateStatus = "rejected"
	MemoryCandidateDuplicate      MemoryCandidateStatus = "duplicate"
)

type MemoryCandidateCategory string

const (
	CandidateCategoryProjectDecision MemoryCandidateCategory = "project_decision"
	CandidateCategoryPreference      MemoryCandidateCategory = "preference"
	CandidateCategoryIdentity        MemoryCandidateCategory = "identity"
	CandidateCategoryPerson          MemoryCandidateCategory = "person_or_pet"
	CandidateCategoryInfrastructure  MemoryCandidateCategory = "infrastructure"
	CandidateCategoryTodo            MemoryCandidateCategory = "todo"
	CandidateCategoryHistorical      MemoryCandidateCategory = "historical"
	CandidateCategoryGeneral         MemoryCandidateCategory = "general"
)

type MemoryCandidateSource struct {
	InventoryID   string   `json:"inventory_id"`
	RootLabel     string   `json:"root_label"`
	RelativePath  string   `json:"relative_path"`
	FileSHA256    string   `json:"file_sha256"`
	ContentSHA256 string   `json:"content_sha256"`
	LineStart     int      `json:"line_start"`
	LineEnd       int      `json:"line_end"`
	HeadingPath   []string `json:"heading_path,omitempty"`
}

type MemoryCandidate struct {
	CandidateID           string                  `json:"candidate_id"`
	Status                MemoryCandidateStatus   `json:"status"`
	Category              MemoryCandidateCategory `json:"category"`
	NamespaceSuggestion   string                  `json:"namespace_suggestion,omitempty"`
	KeySuggestion         string                  `json:"key_suggestion,omitempty"`
	SummarySuggestion     string                  `json:"summary_suggestion,omitempty"`
	Content               string                  `json:"content,omitempty"`
	SensitivitySuggestion MemorySensitivity       `json:"sensitivity_suggestion"`
	ConfidenceSuggestion  float64                 `json:"confidence_suggestion"`
	Flags                 []string                `json:"flags,omitempty"`
	DuplicateOf           string                  `json:"duplicate_of,omitempty"`
	Reason                string                  `json:"reason,omitempty"`
	Source                MemoryCandidateSource   `json:"source"`
}

type MemoryCandidateOptions struct {
	InventoryPath   string
	SourceTypes     []string
	IncludeArchives bool
	MaxFiles        int
	MaxSections     int
	MaxSectionBytes int64
	MaxTotalBytes   int64
}

type MemoryCandidateReport struct {
	FormatVersion     int       `json:"format_version"`
	ExtractionBatchID string    `json:"extraction_batch_id"`
	InventoryID       string    `json:"inventory_id"`
	CreatedAt         time.Time `json:"created_at"`
	FilesSelected     int       `json:"files_selected"`
	FilesExtracted    int       `json:"files_extracted"`
	FilesStale        int       `json:"files_stale"`
	FilesUnsupported  int       `json:"files_unsupported"`
	FilesRejected     int       `json:"files_rejected"`
	SectionsExtracted int       `json:"sections_extracted"`
	CandidatesEmitted int       `json:"candidates_emitted"`
	ReviewRequired    int       `json:"review_required"`
	Rejected          int       `json:"rejected"`
	Duplicates        int       `json:"duplicates"`
	BytesEmitted      int64     `json:"bytes_emitted"`
	Truncated         bool      `json:"truncated"`
	Diagnostics       []string  `json:"diagnostics,omitempty"`
}

type candidateSection struct {
	start       int
	end         int
	headingPath []string
	text        string
}

var candidateHeadingPattern = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)\s*$`)
var candidateVolatilePattern = regexp.MustCompile(`(?i)\b(192\.168\.|10\.\d+\.|172\.(1[6-9]|2\d|3[01])\.|localhost|127\.0\.0\.1|port\s*[:=]?\s*\d{2,5}|ip\s*address|endpoint|host\s*[:=])`)
var candidateTodoPattern = regexp.MustCompile(`(?i)\b(todo|next step|follow[- ]?up|backlog|should do|need to|resume phrase|wake phrase)\b`)
var candidateHistoricalPattern = regexp.MustCompile(`(?i)\b(historical|formerly|previously|superseded|replaced|no longer|stale|deprecated|archive|old checkpoint)\b`)
var candidatePreferencePattern = regexp.MustCompile(`(?i)\b(prefer|preference|likes?|loves?|hates?|wants?|doesn't want|communication style|favorite)\b`)
var candidateDecisionPattern = regexp.MustCompile(`(?i)\b(decided|decision|agreed|implemented|must use|should use|architecture|sequencing|roadmap|boundary)\b`)
var candidatePersonPattern = regexp.MustCompile(`(?i)\b(wife|husband|cat|dog|pet|family|parent|mother|father)\b`)

// ExtractMemoryCandidates reads only files listed as included in an M1
// manifest, rechecks their hashes, and emits bounded review candidates. It
// never writes Bee memory or modifies a source root.
func ExtractMemoryCandidates(options MemoryCandidateOptions) ([]MemoryCandidate, MemoryCandidateReport, error) {
	manifest, err := LoadMemoryInventory(options.InventoryPath)
	if err != nil {
		return nil, MemoryCandidateReport{}, err
	}
	options, err = normalizeCandidateOptions(options)
	if err != nil {
		return nil, MemoryCandidateReport{}, err
	}
	allowedTypes := candidateAllowedTypes(options)
	report := MemoryCandidateReport{
		FormatVersion:     MemoryCandidateFormatVersion,
		ExtractionBatchID: fmt.Sprintf("memory-candidates-%d", time.Now().UTC().UnixNano()),
		InventoryID:       manifest.InventoryID,
		CreatedAt:         time.Now().UTC(),
	}
	candidates := make([]MemoryCandidate, 0)
	seenContent := make(map[string]string)
	for _, file := range manifest.Files {
		if report.FilesSelected >= options.MaxFiles || len(candidates) >= options.MaxSections {
			report.Truncated = true
			break
		}
		if file.Status != "included" || !allowedTypes[file.SourceType] {
			continue
		}
		report.FilesSelected++
		if !isCandidateTextPath(file.RelativePath) {
			report.FilesUnsupported++
			continue
		}
		root, ok := inventoryRootByLabel(manifest.Roots, file.RootLabel)
		if !ok {
			report.Diagnostics = appendCandidateDiagnostic(report.Diagnostics, file.RelativePath+": inventory root not found")
			report.FilesStale++
			continue
		}
		path, pathErr := resolveInventoryFilePath(root, file.RelativePath)
		if pathErr != nil {
			report.Diagnostics = appendCandidateDiagnostic(report.Diagnostics, file.RelativePath+": "+pathErr.Error())
			report.FilesStale++
			continue
		}
		data, readErr := readCandidateSource(path, DefaultCandidateMaxSectionBytes*128)
		if readErr != nil {
			report.Diagnostics = appendCandidateDiagnostic(report.Diagnostics, file.RelativePath+": cannot read source")
			report.FilesRejected++
			continue
		}
		fileHash := sha256.Sum256(data)
		currentHash := hex.EncodeToString(fileHash[:])
		if currentHash != file.SHA256 {
			report.FilesStale++
			report.Diagnostics = appendCandidateDiagnostic(report.Diagnostics, file.RelativePath+": source hash changed since inventory")
			continue
		}
		report.FilesExtracted++
		sections := splitCandidateSections(data)
		report.SectionsExtracted += len(sections)
		for _, section := range sections {
			if len(candidates) >= options.MaxSections {
				report.Truncated = true
				break
			}
			candidate := makeCandidate(manifest, file, section, seenContent)
			if int64(len(candidate.Content)) > options.MaxSectionBytes {
				candidate.Content = ""
				candidate.Status = MemoryCandidateRejected
				candidate.Reason = "section exceeds max_section_bytes"
				candidate.Flags = append(candidate.Flags, "oversized_section")
			}
			if candidate.Content != "" && int64(report.BytesEmitted+int64(len(candidate.Content))) > options.MaxTotalBytes {
				candidate.Content = ""
				candidate.Status = MemoryCandidateRejected
				candidate.Reason = "candidate byte budget exceeded"
				candidate.Flags = append(candidate.Flags, "candidate_byte_budget")
				report.Truncated = true
			}
			candidates = append(candidates, candidate)
			countCandidate(&report, candidate)
		}
		if report.Truncated {
			break
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CandidateID < candidates[j].CandidateID })
	report.Diagnostics = uniqueStrings(report.Diagnostics)
	return candidates, report, nil
}

func LoadMemoryInventory(path string) (MemoryInventoryManifest, error) {
	if strings.TrimSpace(path) == "" {
		return MemoryInventoryManifest{}, errors.New("memory inventory path is required")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return MemoryInventoryManifest{}, errors.New("inspect memory inventory manifest")
	}
	if info.Size() > 128<<20 {
		return MemoryInventoryManifest{}, errors.New("memory inventory manifest is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return MemoryInventoryManifest{}, errors.New("read memory inventory manifest")
	}
	var manifest MemoryInventoryManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return MemoryInventoryManifest{}, fmt.Errorf("parse memory inventory manifest: %w", err)
	}
	if manifest.FormatVersion != MemoryInventoryFormatVersion || manifest.InventoryID == "" || len(manifest.Roots) == 0 {
		return MemoryInventoryManifest{}, errors.New("invalid memory inventory manifest")
	}
	return manifest, nil
}

func normalizeCandidateOptions(options MemoryCandidateOptions) (MemoryCandidateOptions, error) {
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultCandidateMaxFiles
	}
	if options.MaxSections == 0 {
		options.MaxSections = DefaultCandidateMaxSections
	}
	if options.MaxSectionBytes == 0 {
		options.MaxSectionBytes = DefaultCandidateMaxSectionBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = DefaultCandidateMaxTotalBytes
	}
	if options.MaxFiles < 1 || options.MaxFiles > MaxCandidateMaxFiles {
		return MemoryCandidateOptions{}, fmt.Errorf("candidate max files must be between 1 and %d", MaxCandidateMaxFiles)
	}
	if options.MaxSections < 1 || options.MaxSections > MaxCandidateMaxSections {
		return MemoryCandidateOptions{}, fmt.Errorf("candidate max sections must be between 1 and %d", MaxCandidateMaxSections)
	}
	if options.MaxSectionBytes < 1 || options.MaxSectionBytes > MaxCandidateSectionBytes {
		return MemoryCandidateOptions{}, fmt.Errorf("candidate max section bytes must be between 1 and %d", MaxCandidateSectionBytes)
	}
	if options.MaxTotalBytes < 1 || options.MaxTotalBytes > MaxCandidateTotalBytes {
		return MemoryCandidateOptions{}, fmt.Errorf("candidate max total bytes must be between 1 and %d", MaxCandidateTotalBytes)
	}
	return options, nil
}

// DefaultCandidateSourceTypes is the ground-truth-only default for candidate
// extraction. curated_long_term and curated_topic are derived layers.
var DefaultCandidateSourceTypes = []string{"daily_note"}

func candidateAllowedTypes(options MemoryCandidateOptions) map[string]bool {
	if len(options.SourceTypes) == 0 {
		// Derived layers (MEMORY.md, the curated memory/topics/ layer that the
		// dreaming pass maintains in place) are excluded by default so a rebuild
		// from dailies and transcripts never absorbs a previous dream's
		// conclusions: dreams never feed dreams. Importing them from another
		// assistant's export remains an explicit --source-types choice.
		options.SourceTypes = append([]string(nil), DefaultCandidateSourceTypes...)
		if options.IncludeArchives {
			options.SourceTypes = append(options.SourceTypes, "archive")
		}
	}
	result := make(map[string]bool, len(options.SourceTypes))
	for _, value := range options.SourceTypes {
		result[strings.TrimSpace(value)] = true
	}
	return result
}

func inventoryRootByLabel(roots []MemoryInventoryRoot, label string) (MemoryInventoryRoot, bool) {
	for _, root := range roots {
		if root.Label == label {
			return root, true
		}
	}
	return MemoryInventoryRoot{}, false
}

func resolveInventoryFilePath(root MemoryInventoryRoot, relative string) (string, error) {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", errors.New("unsafe inventory relative path")
	}
	rootInfo, err := os.Stat(root.Path)
	if err != nil {
		return "", errors.New("inventory root is unavailable")
	}
	path := root.Path
	if rootInfo.IsDir() {
		path = filepath.Join(root.Path, filepath.FromSlash(relative))
	}
	path = filepath.Clean(path)
	if rootInfo.IsDir() {
		base, _ := filepath.Abs(root.Path)
		abs, _ := filepath.Abs(path)
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return "", errors.New("inventory path escapes root")
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("inventory source is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("inventory source is a symlink")
	}
	return path, nil
}

func readCandidateSource(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return nil, errors.New("source exceeds candidate read bound")
	}
	return data, nil
}

func isCandidateTextPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown" || ext == ".txt"
}

func splitCandidateSections(data []byte) []candidateSection {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type heading struct {
		line, level int
		title       string
	}
	headings := make([]heading, 0)
	for index, line := range lines {
		match := candidateHeadingPattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) == 3 {
			headings = append(headings, heading{line: index + 1, level: len(match[1]), title: strings.TrimSpace(strings.TrimRight(match[2], "# "))})
		}
	}
	sections := make([]candidateSection, 0)
	if len(headings) > 0 {
		for index, current := range headings {
			end := len(lines)
			if index+1 < len(headings) {
				end = headings[index+1].line - 1
			}
			contentLines := lines[current.line-1 : end]
			for len(contentLines) > 0 && strings.TrimSpace(contentLines[len(contentLines)-1]) == "" {
				contentLines = contentLines[:len(contentLines)-1]
			}
			content := strings.TrimSpace(strings.Join(contentLines, "\n"))
			if content == "" {
				continue
			}
			sectionEnd := current.line + len(contentLines) - 1

			path := make([]string, 0, current.level)
			for _, previous := range headings[:index+1] {
				if previous.line > current.line {
					break
				}
				if previous.level < current.level {
					path = append(path, previous.title)
				}
			}
			path = append(path, current.title)
			sections = append(sections, candidateSection{start: current.line, end: sectionEnd, headingPath: path, text: content})
		}
		return sections
	}
	start := 0
	for start < len(lines) {
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
		if start >= len(lines) {
			break
		}
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		content := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		if content != "" {
			sections = append(sections, candidateSection{start: start + 1, end: end, text: content})
		}
		start = end
	}
	return sections
}

func makeCandidate(manifest MemoryInventoryManifest, file MemoryInventoryFile, section candidateSection, seen map[string]string) MemoryCandidate {
	hash := sha256.Sum256([]byte(normalizeCandidateText(section.text)))
	contentHash := hex.EncodeToString(hash[:])
	source := MemoryCandidateSource{InventoryID: manifest.InventoryID, RootLabel: file.RootLabel, RelativePath: file.RelativePath, FileSHA256: file.SHA256, ContentSHA256: contentHash, LineStart: section.start, LineEnd: section.end, HeadingPath: append([]string(nil), section.headingPath...)}
	idHash := sha256.Sum256([]byte(file.RootLabel + "\x00" + file.RelativePath + "\x00" + fmt.Sprint(section.start) + "\x00" + fmt.Sprint(section.end) + "\x00" + contentHash))
	candidate := MemoryCandidate{CandidateID: "candidate-" + hex.EncodeToString(idHash[:])[:24], Status: MemoryCandidateCandidate, Category: classifyCandidate(section.headingPath, section.text), Content: section.text, Source: source, SensitivitySuggestion: MemorySensitivityPersonal, ConfidenceSuggestion: 0.65}
	candidate.SummarySuggestion = candidateSummary(section.headingPath, section.text)
	candidate.NamespaceSuggestion, candidate.KeySuggestion = candidateLocation(file, section.headingPath, candidate.Category)
	candidate.Flags = candidateFlags(section.headingPath, section.text, candidate.Category)
	if candidate.Category == CandidateCategoryInfrastructure || candidate.Category == CandidateCategoryHistorical || len(candidate.Flags) > 0 {
		candidate.Status = MemoryCandidateReviewRequired
		candidate.ConfidenceSuggestion = 0.45
	}
	if inventoryLooksSecret([]byte(section.text)) {
		candidate.Status = MemoryCandidateRejected
		candidate.Content = ""
		candidate.Reason = "secret-shaped section"
		candidate.Flags = append(candidate.Flags, "secret_shaped")
		return candidate
	}
	if previous, exists := seen[contentHash]; exists {
		candidate.Status = MemoryCandidateDuplicate
		candidate.DuplicateOf = previous
		candidate.Content = ""
		candidate.Reason = "exact normalized-content duplicate"
	} else {
		seen[contentHash] = candidate.CandidateID
	}
	return candidate
}

func normalizeCandidateText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	content := make([]string, 0, len(lines))
	for _, line := range lines {
		if candidateHeadingPattern.MatchString(strings.TrimSpace(line)) {
			continue
		}
		content = append(content, line)
	}
	return strings.Join(strings.Fields(strings.Join(content, " ")), " ")
}

func classifyCandidate(headings []string, text string) MemoryCandidateCategory {
	context := strings.ToLower(strings.Join(append(append([]string{}, headings...), text), " "))
	switch {
	case candidateTodoPattern.MatchString(context):
		return CandidateCategoryTodo
	case candidateHistoricalPattern.MatchString(context):
		return CandidateCategoryHistorical
	case candidateVolatilePattern.MatchString(context):
		return CandidateCategoryInfrastructure
	case candidatePreferencePattern.MatchString(context):
		return CandidateCategoryPreference
	case candidatePersonPattern.MatchString(context):
		return CandidateCategoryPerson
	case candidateDecisionPattern.MatchString(context):
		return CandidateCategoryProjectDecision
	default:
		return CandidateCategoryGeneral
	}
}

func candidateFlags(headings []string, text string, category MemoryCandidateCategory) []string {
	context := strings.ToLower(strings.Join(append(append([]string{}, headings...), text), " "))
	flags := make([]string, 0, 3)
	if candidateVolatilePattern.MatchString(context) {
		flags = append(flags, "volatile_fact")
	}
	if candidateHistoricalPattern.MatchString(context) {
		flags = append(flags, "possible_supersession_or_history")
	}
	if category == CandidateCategoryTodo {
		flags = append(flags, "requires_currentness_review")
	}
	if strings.Contains(context, "?") || strings.Contains(context, "uncertain") || strings.Contains(context, "maybe") {
		flags = append(flags, "uncertain_language")
	}
	return flags
}

func candidateSummary(headings []string, text string) string {
	if len(headings) > 0 && strings.TrimSpace(headings[len(headings)-1]) != "" {
		return truncateCandidateText(headings[len(headings)-1], 512)
	}
	return truncateCandidateText(strings.TrimSpace(strings.Split(text, "\n")[0]), 512)
}

func candidateLocation(file MemoryInventoryFile, headings []string, category MemoryCandidateCategory) (string, string) {
	name := strings.TrimSuffix(filepath.Base(file.RelativePath), filepath.Ext(file.RelativePath))
	name = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", "-"), " ", "-"))
	name = strings.Trim(name, "-._")
	if name == "" {
		name = "source"
	}
	namespace := "imported/" + strings.ToLower(strings.ReplaceAll(file.SourceType, "_", "-"))
	key := name + "/" + strings.ToLower(string(category))
	if len(headings) > 0 {
		key += "/" + strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(headings[len(headings)-1], " ", "-"), "/", "-"))
	}
	return namespace, truncateCandidateText(key, 128)
}

func truncateCandidateText(value string, max int) string {
	if len(value) > max {
		return value[:max]
	}
	return value
}

func countCandidate(report *MemoryCandidateReport, candidate MemoryCandidate) {
	report.CandidatesEmitted++
	report.BytesEmitted += int64(len(candidate.Content))
	switch candidate.Status {
	case MemoryCandidateReviewRequired:
		report.ReviewRequired++
	case MemoryCandidateRejected:
		report.Rejected++
	case MemoryCandidateDuplicate:
		report.Duplicates++
	}
}

func appendCandidateDiagnostic(values []string, value string) []string {
	if len(value) > 512 {
		value = value[:512]
	}
	if len(values) >= 256 {
		return values
	}
	return append(values, value)
}

// WriteMemoryCandidates writes JSONL candidates to a new 0600 file and a
// report beside it. It refuses overwrites and never writes durable memory.
func WriteMemoryCandidates(candidatePath, reportPath string, candidates []MemoryCandidate, report MemoryCandidateReport) error {
	if !filepath.IsAbs(candidatePath) || !filepath.IsAbs(reportPath) {
		return errors.New("candidate and report paths must be absolute")
	}
	if candidatePath == reportPath {
		return errors.New("candidate and report paths must differ")
	}
	for _, path := range []string{candidatePath, reportPath} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("refusing to overwrite existing candidate artifact")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect candidate artifact path")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create candidate output directory: %w", err)
		}
	}
	candidateFile, err := os.OpenFile(candidatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create candidate JSONL: %w", err)
	}
	encoder := json.NewEncoder(candidateFile)
	for _, candidate := range candidates {
		if err := encoder.Encode(candidate); err != nil {
			candidateFile.Close()
			return fmt.Errorf("write candidate JSONL: %w", err)
		}
	}
	if err := candidateFile.Chmod(0o600); err != nil {
		candidateFile.Close()
		return err
	}
	if err := candidateFile.Close(); err != nil {
		return err
	}
	reportData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode candidate report: %w", err)
	}
	reportData = append(reportData, '\n')
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		return fmt.Errorf("write candidate report: %w", err)
	}
	return nil
}
