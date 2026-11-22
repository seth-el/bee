package bee

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultMoodFileName is the single application-wide affective continuity
	// file below the configured memory root. It is never session-scoped.
	DefaultMoodFileName = "mood.md"
	// MaxMoodFileBytes keeps model-facing prefix state small and bounded.
	MaxMoodFileBytes = 64 << 10
	// MaxMoodDeltaBytes bounds one prose movement before it enters prefix state.
	MaxMoodDeltaBytes = 1024
	// MaxMoodEntries bounds carried movements in one baseline.
	MaxMoodEntries = 64
	// MaxMoodSnapshotEntries and MaxMoodSnapshotBytes bound what a session's
	// prefix shows of the baseline: the newest movements only. The file keeps
	// every carried entry; mood is a hint about current register, and older
	// movements belong to the daily notes, not to every prompt.
	MaxMoodSnapshotEntries = 10
	MaxMoodSnapshotBytes   = 4096
	// MaxMoodLongEntries bounds how many movements keep the long half-life.
	// Beyond the newest ones, a "long" movement decays as "medium", so the
	// baseline cannot fill up with entries that never fade.
	MaxMoodLongEntries = 8
	// MaxMoodDeltaSourceBytes bounds each append-only source-note read.
	MaxMoodDeltaSourceBytes = 256 << 10
	// MaxMoodDeltaSources bounds the number of daily/delta notes inspected.
	MaxMoodDeltaSources = 1024
	// MaxMoodDirectoryEntries bounds directory enumeration before filtering.
	MaxMoodDirectoryEntries = 4096
)

const (
	moodManagedStart = "<!-- bee:mood-managed:v1 begin -->"
	moodManagedEnd   = "<!-- bee:mood-managed:v1 end -->"
	moodStatePrefix  = "<!-- bee:mood-state:v1 "
	moodStateSuffix  = " -->"
	moodDeltaPrefix  = "<!-- bee:mood-delta "
	moodDeltaClose   = "<!-- /bee:mood-delta -->"
)

type MoodDecay string

const (
	MoodDecayFleeting MoodDecay = "fleeting"
	MoodDecayShort    MoodDecay = "short"
	MoodDecayMedium   MoodDecay = "medium"
	MoodDecayLong     MoodDecay = "long"
)

func (decay MoodDecay) valid() bool {
	switch decay {
	case MoodDecayFleeting, MoodDecayShort, MoodDecayMedium, MoodDecayLong:
		return true
	default:
		return false
	}
}

type moodEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Decay    MoodDecay `json:"decay"`
	Text     string    `json:"text"`
	Source   string    `json:"source"`
	Offset   int64     `json:"offset"`
	Softened bool      `json:"softened,omitempty"`
}

type moodSourceCursor struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	SHA256 string `json:"sha256"`
}

type moodState struct {
	Version int                `json:"version"`
	Entries []moodEntry        `json:"entries,omitempty"`
	Sources []moodSourceCursor `json:"sources,omitempty"`
}

// MoodConsolidationReport contains no mood text, source-note text, or paths.
// It is safe for CLI maintenance output and operational checks.
type MoodConsolidationReport struct {
	ScannedSources int  `json:"scanned_sources"`
	Merged         int  `json:"merged"`
	Softened       int  `json:"softened"`
	Dropped        int  `json:"dropped"`
	Rejected       int  `json:"rejected"`
	SkippedSources int  `json:"skipped_sources"`
	ActiveEntries  int  `json:"active_entries"`
	Updated        bool `json:"updated"`
}

// MoodManager owns the one global mood-file seam. Session activation and
// consolidation are application concerns; the manager never stores state in
// a durable session.
type MoodManager struct {
	root string
}

func NewMoodManager(root string) (*MoodManager, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("mood root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &MoodManager{root: filepath.Clean(abs)}, nil
}

func (manager *MoodManager) Path() string {
	if manager == nil {
		return ""
	}
	return filepath.Join(manager.root, DefaultMoodFileName)
}

// Snapshot returns bounded prose for one active session lifetime. Missing,
// unsafe, unreadable, invalid-UTF-8, or empty files deliberately compose as
// absent so affective continuity can never prevent a session from starting.
func (manager *MoodManager) Snapshot() string {
	return manager.snapshotProjection()
}

// snapshotProjection performs one bounded, read-only baseline load and returns
// the model context rendered from it. It never consolidates or writes mood
// state.
func (manager *MoodManager) snapshotProjection() string {
	if manager == nil {
		return ""
	}
	contents, present, err := loadPrivatePromptFile(manager.Path(), "Bee mood file", MaxMoodFileBytes)
	if err != nil || !present || !utf8.ValidString(contents) {
		return ""
	}
	manual, state, err := parseMoodDocument(contents)
	if err != nil {
		return ""
	}
	return renderMoodSnapshot(manual, state.Entries)
}

// Consolidate merges complete append markers from bounded daily/delta notes
// into mood.md. The source notes remain untouched. The baseline and its source
// cursors are committed in one atomic rename, so a crash before commit retries
// the same deltas and a crash after commit observes their cursors.
func (manager *MoodManager) Consolidate(ctx context.Context, now time.Time) (MoodConsolidationReport, error) {
	var report MoodConsolidationReport
	if manager == nil {
		return report, errors.New("mood manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if _, err := preparePrivateStorageRoot(manager.root); err != nil {
		return report, err
	}
	lock, err := acquireSessionFileLockContext(ctx, filepath.Join(manager.root, ".mood.lock"))
	if err != nil {
		return report, err
	}
	defer lock.Close()

	existing, manual, state, present, err := manager.loadDocumentForMerge()
	if err != nil {
		return report, err
	}
	entriesByID := make(map[string]moodEntry, len(state.Entries))
	for _, entry := range state.Entries {
		entriesByID[entry.ID] = entry
	}
	cursors := make(map[string]moodSourceCursor, len(state.Sources))
	for _, cursor := range state.Sources {
		cursors[cursor.Name] = cursor
	}

	sources, skipped, err := manager.deltaSources()
	if err != nil {
		return report, err
	}
	report.SkippedSources += skipped
	for _, name := range sources {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path := filepath.Join(manager.root, name)
		data, err := readMoodDeltaSource(path)
		if err != nil {
			report.SkippedSources++
			continue
		}
		report.ScannedSources++
		cursor, tracked := cursors[name]
		start := int64(0)
		if tracked {
			if cursor.Offset < 0 || cursor.Offset > int64(len(data)) || digestBytes(data[:cursor.Offset]) != cursor.SHA256 {
				report.SkippedSources++
				continue
			}
			start = cursor.Offset
		}
		parsed, safeOffset, rejected, sawMarker := parseMoodDeltaBlocks(name, data, start, now)
		report.Rejected += rejected
		for _, entry := range parsed {
			if _, exists := entriesByID[entry.ID]; exists {
				continue
			}
			entriesByID[entry.ID] = entry
			report.Merged++
		}
		if tracked || sawMarker {
			cursors[name] = moodSourceCursor{Name: name, Offset: safeOffset, SHA256: digestBytes(data[:safeOffset])}
		}
	}

	entries := make([]moodEntry, 0, len(entriesByID))
	for _, entry := range entriesByID {
		entries = append(entries, entry)
	}
	sortMoodEntries(entries)
	entries, report.Softened, report.Dropped = applyMoodDecay(entries, now)
	if len(entries) > MaxMoodEntries {
		return report, fmt.Errorf("mood entries exceed bounded limit of %d", MaxMoodEntries)
	}
	state.Entries = entries
	state.Sources = sortedMoodCursors(cursors)
	state.Version = 1
	report.ActiveEntries = len(entries)
	if !present && len(entries) == 0 {
		return report, nil
	}
	updated, err := renderMoodDocument(manual, state)
	if err != nil {
		return report, err
	}
	if len(updated) > MaxMoodFileBytes {
		return report, fmt.Errorf("mood baseline exceeds bounded limit of %d bytes", MaxMoodFileBytes)
	}
	if bytes.Equal(existing, updated) {
		return report, nil
	}
	if err := atomicWriteFile(manager.Path(), updated, 0o600); err != nil {
		return report, err
	}
	report.Updated = true
	return report, nil
}

func (manager *MoodManager) loadDocumentForMerge() ([]byte, string, moodState, bool, error) {
	info, err := os.Lstat(manager.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", moodState{Version: 1}, false, nil
	}
	if err != nil {
		return nil, "", moodState{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, "", moodState{}, false, errors.New("mood baseline is not a private readable regular file")
	}
	data, err := os.ReadFile(manager.Path())
	if err != nil {
		return nil, "", moodState{}, false, err
	}
	if len(data) > MaxMoodFileBytes || !utf8.Valid(data) {
		return nil, "", moodState{}, false, errors.New("mood baseline is invalid or oversized")
	}
	manual, state, err := parseMoodDocument(string(data))
	if err != nil {
		return nil, "", moodState{}, false, err
	}
	return data, manual, state, true, nil
}

func parseMoodDocument(contents string) (string, moodState, error) {
	state := moodState{Version: 1}
	start := strings.Index(contents, moodManagedStart)
	end := strings.Index(contents, moodManagedEnd)
	if start < 0 && end < 0 {
		return strings.TrimSpace(contents), state, nil
	}
	if start < 0 || end < start || strings.Count(contents, moodManagedStart) != 1 || strings.Count(contents, moodManagedEnd) != 1 {
		return "", moodState{}, errors.New("mood managed section is malformed")
	}
	managed := contents[start+len(moodManagedStart) : end]
	var encoded string
	for _, line := range strings.Split(managed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, moodStatePrefix) && strings.HasSuffix(line, moodStateSuffix) {
			if encoded != "" {
				return "", moodState{}, errors.New("mood managed section has duplicate state")
			}
			encoded = strings.TrimSuffix(strings.TrimPrefix(line, moodStatePrefix), moodStateSuffix)
		}
	}
	if encoded == "" {
		return "", moodState{}, errors.New("mood managed section is missing state")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > MaxMoodFileBytes {
		return "", moodState{}, errors.New("mood managed state is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return "", moodState{}, errors.New("mood managed state is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", moodState{}, errors.New("mood managed state has trailing data")
	}
	if err := validateMoodState(state); err != nil {
		return "", moodState{}, err
	}
	manual := strings.TrimSpace(contents[:start] + "\n" + contents[end+len(moodManagedEnd):])
	return manual, state, nil
}

func validateMoodState(state moodState) error {
	if state.Version != 1 || len(state.Entries) > MaxMoodEntries || len(state.Sources) > MaxMoodDeltaSources {
		return errors.New("mood managed state is out of bounds")
	}
	entryIDs := make(map[string]struct{}, len(state.Entries))
	for _, entry := range state.Entries {
		if len(entry.ID) != sha256.Size*2 || entry.At.IsZero() || !entry.Decay.valid() || validateMoodText(entry.Text) != nil || entry.Source == "" || filepath.Base(entry.Source) != entry.Source || entry.Offset < 0 {
			return errors.New("mood managed entry is invalid")
		}
		if _, err := hex.DecodeString(entry.ID); err != nil {
			return errors.New("mood managed entry is invalid")
		}
		if _, exists := entryIDs[entry.ID]; exists {
			return errors.New("mood managed entries contain a duplicate")
		}
		entryIDs[entry.ID] = struct{}{}
	}
	sourceNames := make(map[string]struct{}, len(state.Sources))
	for _, source := range state.Sources {
		if source.Name == "" || filepath.Base(source.Name) != source.Name || source.Offset < 0 || len(source.SHA256) != sha256.Size*2 {
			return errors.New("mood source cursor is invalid")
		}
		if _, err := hex.DecodeString(source.SHA256); err != nil {
			return errors.New("mood source cursor is invalid")
		}
		if _, exists := sourceNames[source.Name]; exists {
			return errors.New("mood source cursors contain a duplicate")
		}
		sourceNames[source.Name] = struct{}{}
	}
	return nil
}

func renderMoodDocument(manual string, state moodState) ([]byte, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	var builder strings.Builder
	manual = strings.TrimSpace(manual)
	if manual == "" {
		builder.WriteString("# Mood")
	} else {
		builder.WriteString(manual)
	}
	builder.WriteString("\n\n")
	builder.WriteString(moodManagedStart)
	builder.WriteByte('\n')
	builder.WriteString(moodStatePrefix)
	builder.WriteString(encoded)
	builder.WriteString(moodStateSuffix)
	if len(state.Entries) > 0 {
		builder.WriteString("\n\n## Current affective movement\n\n")
		for _, entry := range state.Entries {
			builder.WriteString("- ")
			builder.WriteString(strings.ReplaceAll(renderMoodEntryText(entry), "\n", "\n  "))
			builder.WriteByte('\n')
		}
	}
	builder.WriteString(moodManagedEnd)
	builder.WriteByte('\n')
	return []byte(builder.String()), nil
}

func renderMoodSnapshot(manual string, entries []moodEntry) string {
	var builder strings.Builder
	manual = strings.TrimSpace(manual)
	if manual != "" {
		builder.WriteString(manual)
	}
	shown, older := recentMoodEntries(entries)
	if len(shown) > 0 {
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString("## Current affective movement\n\n")
		for _, entry := range shown {
			builder.WriteString("- ")
			builder.WriteString(strings.ReplaceAll(renderMoodEntryText(entry), "\n", "\n  "))
			builder.WriteByte('\n')
		}
		if older > 0 {
			fmt.Fprintf(&builder, "\n(%d older movements are not shown here; they remain in memory/mood.md and the daily notes.)\n", older)
		}
	}
	return strings.TrimSpace(builder.String())
}

// recentMoodEntries selects the newest entries that fit the snapshot bounds,
// in their original oldest-first order, and reports how many were left out.
// The newest entry is always shown.
func recentMoodEntries(entries []moodEntry) ([]moodEntry, int) {
	start := len(entries)
	used := 0
	for start > 0 && len(entries)-start < MaxMoodSnapshotEntries {
		size := len(renderMoodEntryText(entries[start-1])) + 3
		if start < len(entries) && used+size > MaxMoodSnapshotBytes {
			break
		}
		used += size
		start--
	}
	return entries[start:], start
}

func (manager *MoodManager) deltaSources() ([]string, int, error) {
	directory, err := os.Open(manager.root)
	if err != nil {
		return nil, 0, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(MaxMoodDirectoryEntries + 1)
	if err != nil {
		return nil, 0, err
	}
	skipped := 0
	if len(entries) > MaxMoodDirectoryEntries {
		entries = entries[:MaxMoodDirectoryEntries]
		skipped++
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == DefaultMoodFileName || name == ".mood.lock" {
			continue
		}
		if name != "mood-deltas.md" && !dailyMemoryNamePattern.MatchString(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > MaxMoodDeltaSources {
		names = names[:MaxMoodDeltaSources]
		skipped++
	}
	return names, skipped, nil
}

func readMoodDeltaSource(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxMoodDeltaSourceBytes {
		return nil, errors.New("mood delta source is unsafe or oversized")
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) || len(data) > MaxMoodDeltaSourceBytes {
		return nil, errors.New("mood delta source is unreadable or invalid")
	}
	return data, nil
}

func parseMoodDeltaBlocks(source string, data []byte, start int64, now time.Time) ([]moodEntry, int64, int, bool) {
	entries := make([]moodEntry, 0, 4)
	rejected := 0
	sawMarker := false
	position := int(start)
	safeOffset := int(start)
	for position < len(data) {
		relative := bytes.Index(data[position:], []byte(moodDeltaPrefix))
		if relative < 0 {
			safeOffset = len(data)
			break
		}
		blockStart := position + relative
		headerEndRelative := bytes.Index(data[blockStart:], []byte("-->"))
		if headerEndRelative < 0 {
			safeOffset = blockStart
			break
		}
		headerEnd := blockStart + headerEndRelative + len("-->")
		closeRelative := bytes.Index(data[headerEnd:], []byte(moodDeltaClose))
		if closeRelative < 0 {
			safeOffset = blockStart
			break
		}
		closeStart := headerEnd + closeRelative
		blockEnd := closeStart + len(moodDeltaClose)
		sawMarker = true
		header := strings.TrimSpace(string(data[blockStart+len(moodDeltaPrefix) : headerEnd-len("-->")]))
		at, decay, ok := parseMoodDeltaHeader(header)
		text := strings.TrimSpace(string(data[headerEnd:closeStart]))
		if !ok || at.After(now.Add(5*time.Minute)) || validateMoodText(text) != nil {
			rejected++
		} else {
			block := data[blockStart:blockEnd]
			hash := sha256.New()
			_, _ = hash.Write([]byte(source))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte(strconv.Itoa(blockStart)))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write(block)
			entries = append(entries, moodEntry{ID: hex.EncodeToString(hash.Sum(nil)), At: at, Decay: decay, Text: text, Source: source, Offset: int64(blockStart)})
		}
		safeOffset = blockEnd
		position = blockEnd
	}
	return entries, int64(safeOffset), rejected, sawMarker
}

func parseMoodDeltaHeader(header string) (time.Time, MoodDecay, bool) {
	const atPrefix = `at="`
	const decayMiddle = `" decay="`
	if !strings.HasPrefix(header, atPrefix) || !strings.HasSuffix(header, `"`) {
		return time.Time{}, "", false
	}
	remainder := strings.TrimSuffix(strings.TrimPrefix(header, atPrefix), `"`)
	parts := strings.Split(remainder, decayMiddle)
	if len(parts) != 2 {
		return time.Time{}, "", false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	decay := MoodDecay(parts[1])
	if err != nil || !decay.valid() {
		return time.Time{}, "", false
	}
	return at.UTC(), decay, true
}

func validateMoodText(text string) error {
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) || len(text) > MaxMoodDeltaBytes || strings.Contains(text, "bee:mood-") {
		return errors.New("mood delta prose is invalid")
	}
	return nil
}

func sortMoodEntries(entries []moodEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].At.Equal(entries[j].At) {
			return entries[i].At.Before(entries[j].At)
		}
		if entries[i].Source != entries[j].Source {
			return entries[i].Source < entries[j].Source
		}
		if entries[i].Offset != entries[j].Offset {
			return entries[i].Offset < entries[j].Offset
		}
		return entries[i].ID < entries[j].ID
	})
}

func applyMoodDecay(entries []moodEntry, now time.Time) ([]moodEntry, int, int) {
	active := make([]moodEntry, 0, len(entries))
	softened := 0
	dropped := 0
	// Only the newest MaxMoodLongEntries long movements keep the long
	// half-life; entries arrive sorted oldest first.
	longBudget := MaxMoodLongEntries
	effective := make([]MoodDecay, len(entries))
	for index := len(entries) - 1; index >= 0; index-- {
		effective[index] = entries[index].Decay
		if entries[index].Decay == MoodDecayLong {
			if longBudget > 0 {
				longBudget--
			} else {
				effective[index] = MoodDecayMedium
			}
		}
	}
	for index, entry := range entries {
		halfLife := moodHalfLife(effective[index])
		age := now.Sub(entry.At)
		if age >= 2*halfLife {
			dropped++
			continue
		}
		entry.Softened = age >= halfLife
		if entry.Softened {
			softened++
		}
		active = append(active, entry)
	}
	return active, softened, dropped
}

func moodHalfLife(decay MoodDecay) time.Duration {
	switch decay {
	case MoodDecayFleeting:
		return 6 * time.Hour
	case MoodDecayShort:
		return 48 * time.Hour
	case MoodDecayMedium:
		return 14 * 24 * time.Hour
	case MoodDecayLong:
		return 60 * 24 * time.Hour
	default:
		// Invalid values are rejected before consolidation reaches this point.
		return 14 * 24 * time.Hour
	}
}

func renderMoodEntryText(entry moodEntry) string {
	if entry.Softened {
		return "Fading, but still present: " + entry.Text
	}
	return entry.Text
}

func sortedMoodCursors(cursors map[string]moodSourceCursor) []moodSourceCursor {
	result := make([]moodSourceCursor, 0, len(cursors))
	for _, cursor := range cursors {
		result = append(result, cursor)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
