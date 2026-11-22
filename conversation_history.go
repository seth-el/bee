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
	"unicode"
	"unicode/utf8"
)

const (
	ConversationHistorySearchToolName = "conversation_search"
	MaxConversationHistoryQueryBytes  = 256
	MaxConversationHistorySessions    = 128
	MaxConversationHistoryMessages    = 256
	MaxConversationHistoryResults     = 16
	MaxConversationHistorySnippet     = 320
	MaxConversationHistoryBytes       = 16 << 10
	// Session JSONL is the source of truth, but searches must not replay a
	// whole session into memory. The file bound is only a corruption/DoS guard;
	// searchable sessions may be substantially larger than the old 4 MiB cap.
	MaxConversationHistoryFileBytes        = 256 << 20
	MaxConversationHistorySearchBytes      = 512 << 20
	MaxConversationHistoryLineBytes        = 8 << 20
	MaxConversationHistoryIndexBytes       = 512 << 20
	MaxConversationHistoryIndexChunk       = 64 << 10
	ConversationHistoryIndexVersion        = 1
	autoConversationHistoryMaxSessions     = 32
	autoConversationHistoryMaxMessages     = 128
	autoConversationHistoryMaxResults      = 4
	autoConversationHistoryMaxSnippetBytes = 320
	autoConversationHistoryMaxBytes        = 8 << 10
)

// ConversationHistorySearch is a provider-neutral, read-only view over the
// existing durable session JSONL files. It writes only rebuildable local search
// sidecars; JSONL remains the source of truth and history is never promoted
// into durable memory.
type ConversationHistorySearch struct {
	Store *SessionStore
	Actor MemoryActor
}

// ConversationHistorySearchRequest bounds one local history lookup.
type ConversationHistorySearchRequest struct {
	Actor     MemoryActor
	Query     string
	SessionID SessionID
	// ExcludeSessionID prevents automatic recall from duplicating the active
	// session, whose complete transcript is already in the provider request.
	// It is an internal composition hint and is not exposed by the tool schema.
	ExcludeSessionID SessionID
	MaxSessions      int
	MaxMessages      int
	MaxResults       int
	MaxSnippetBytes  int
	MaxBytes         int
	// Since (inclusive) and Before (exclusive) bound matched messages by
	// their recorded time. Zero values leave that side of the window open.
	Since  time.Time
	Before time.Time
}

type ConversationHistoryHit struct {
	SessionID        SessionID `json:"session_id"`
	SessionTitle     string    `json:"session_title,omitempty"`
	MessageID        string    `json:"message_id"`
	Sequence         uint64    `json:"sequence"`
	Role             Role      `json:"role"`
	At               time.Time `json:"at"`
	SessionCreatedAt time.Time `json:"session_created_at"`
	SessionUpdatedAt time.Time `json:"session_updated_at"`
	Snippet          string    `json:"snippet"`
	Score            int       `json:"score,omitempty"`
}

type ConversationHistorySearchResult struct {
	Hits          []ConversationHistoryHit `json:"hits,omitempty"`
	Total         int                      `json:"total"`
	MoreAvailable bool                     `json:"more_available"`
}

// ConversationContextPacket is transient turn context. It is never persisted
// into the active session and is rendered as untrusted reference data.
type ConversationContextPacket struct {
	Hits          []ConversationHistoryHit
	MoreAvailable bool
}

type historySessionFile struct {
	id         SessionID
	modifiedAt time.Time
	size       int64
}

func NewConversationHistorySearch(store *SessionStore, actor MemoryActor) (*ConversationHistorySearch, error) {
	if store == nil {
		return nil, errors.New("conversation history session store is required")
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("conversation history actor: %w", err)
	}
	if actor.Kind == MemoryActorChild {
		return nil, errors.New("conversation history is unavailable to child actors")
	}
	return &ConversationHistorySearch{Store: store, Actor: actor}, nil
}

func (service *ConversationHistorySearch) Search(ctx context.Context, request ConversationHistorySearchRequest) (ConversationHistorySearchResult, error) {
	if service == nil || service.Store == nil {
		return ConversationHistorySearchResult{}, errors.New("conversation history search service is not configured")
	}
	if err := request.Actor.Validate(); err != nil {
		return ConversationHistorySearchResult{}, err
	}
	if request.Actor != service.Actor {
		return ConversationHistorySearchResult{}, errors.New("conversation history actor is not authorized")
	}
	query := strings.TrimSpace(request.Query)
	if query == "" {
		return ConversationHistorySearchResult{}, errors.New("conversation history search query is required")
	}
	if len(query) > MaxConversationHistoryQueryBytes || utf8.RuneCountInString(query) > MaxConversationHistoryQueryBytes {
		return ConversationHistorySearchResult{}, fmt.Errorf("conversation history search query must be at most %d characters", MaxConversationHistoryQueryBytes)
	}
	if request.SessionID != "" {
		if err := validateSessionID(request.SessionID); err != nil {
			return ConversationHistorySearchResult{}, err
		}
	}
	if request.ExcludeSessionID != "" {
		if err := validateSessionID(request.ExcludeSessionID); err != nil {
			return ConversationHistorySearchResult{}, err
		}
	}
	since, before := request.Since.UTC(), request.Before.UTC()
	if !request.Since.IsZero() && !request.Before.IsZero() && !before.After(since) {
		return ConversationHistorySearchResult{}, errors.New("conversation history search before must be later than since")
	}
	window := historyTimeWindow{since: since, before: before, hasSince: !request.Since.IsZero(), hasBefore: !request.Before.IsZero()}
	maxSessions := boundPositive(request.MaxSessions, MaxConversationHistorySessions)
	maxMessages := boundPositive(request.MaxMessages, MaxConversationHistoryMessages)
	maxResults := boundPositive(request.MaxResults, MaxConversationHistoryResults)
	maxSnippet := boundPositive(request.MaxSnippetBytes, MaxConversationHistorySnippet)
	maxBytes := boundPositive(request.MaxBytes, MaxConversationHistoryBytes)
	needle := strings.ToLower(query)
	terms := historyQueryTerms(query)

	files, err := func() ([]historySessionFile, error) {
		service.Store.mu.Lock()
		defer service.Store.mu.Unlock()
		entries, err := os.ReadDir(service.Store.root)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list conversation history: %w", err)
		}
		files := make([]historySessionFile, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			id := SessionID(strings.TrimSuffix(entry.Name(), ".jsonl"))
			if request.SessionID != "" && id != request.SessionID {
				continue
			}
			if request.ExcludeSessionID != "" && id == request.ExcludeSessionID {
				continue
			}
			if err := validateSessionID(id); err != nil {
				return nil, fmt.Errorf("history file %q: %w", entry.Name(), err)
			}
			path := service.Store.path(id)
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return nil, fmt.Errorf("stat conversation history %q: %w", id, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("history file %q: session history path must be a regular file", entry.Name())
			}
			// Session JSONL is append-only, so a file last modified before the
			// window opens cannot hold a message recorded inside it. Skipping it
			// keeps older sessions from consuming the bounded session budget.
			// Filesystem mtimes come from a coarse kernel clock that can trail
			// the event timestamp, so only files clearly older than the window
			// are skipped; anything closer is still filtered per message.
			if window.hasSince && info.ModTime().Before(window.since.Add(-historyMtimeSlack)) {
				continue
			}
			files = append(files, historySessionFile{id: id, modifiedAt: info.ModTime(), size: info.Size()})
		}
		return files, nil
	}()
	if err != nil {
		return ConversationHistorySearchResult{}, err
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].modifiedAt.Equal(files[j].modifiedAt) {
			return files[i].modifiedAt.After(files[j].modifiedAt)
		}
		return files[i].id < files[j].id
	})
	moreSessions := false
	selected := make([]historySessionFile, 0, minInt(len(files), maxSessions))
	var selectedBytes int64
	for _, file := range files {
		if len(selected) >= maxSessions || selectedBytes+file.size > MaxConversationHistorySearchBytes {
			moreSessions = true
			continue
		}
		selected = append(selected, file)
		selectedBytes += file.size
	}
	files = selected

	candidates := make([]ConversationHistoryHit, 0, maxResults)
	totalMatches := 0
	var processedBytes int64
	for _, file := range files {
		id := file.id
		if err := ctx.Err(); err != nil {
			return ConversationHistorySearchResult{}, err
		}
		path := service.Store.path(id)
		lock, lockErr := acquireSessionFileLockContext(ctx, path+".lock")
		if lockErr != nil {
			return ConversationHistorySearchResult{}, lockErr
		}
		currentInfo, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			_ = lock.Close()
			moreSessions = true
			continue
		}
		if statErr != nil {
			_ = lock.Close()
			return ConversationHistorySearchResult{}, statErr
		}
		if processedBytes+currentInfo.Size() > MaxConversationHistorySearchBytes {
			_ = lock.Close()
			moreSessions = true
			continue
		}
		processedBytes += currentInfo.Size()
		index, indexErr := ensureConversationHistoryIndex(ctx, path, id)
		if errors.Is(indexErr, os.ErrNotExist) {
			_ = lock.Close()
			moreSessions = true
			continue
		}
		if indexErr != nil {
			_ = lock.Close()
			return ConversationHistorySearchResult{}, fmt.Errorf("index conversation history %q: %w", id, indexErr)
		}
		sessionCandidates := make([]ConversationHistoryHit, 0, maxResults)
		matched, searchErr := searchConversationHistoryIndex(ctx, conversationHistoryIndexPath(path), index, id, query, needle, terms, window, maxMessages, maxSnippet, maxResults, &sessionCandidates)
		if errors.Is(searchErr, errConversationHistoryIndexInvalid) {
			_ = os.Remove(conversationHistoryIndexPath(path))
			index, indexErr = ensureConversationHistoryIndex(ctx, path, id)
			if indexErr == nil {
				sessionCandidates = sessionCandidates[:0]
				matched, searchErr = searchConversationHistoryIndex(ctx, conversationHistoryIndexPath(path), index, id, query, needle, terms, window, maxMessages, maxSnippet, maxResults, &sessionCandidates)
			}
		}
		_ = lock.Close()
		if indexErr != nil {
			return ConversationHistorySearchResult{}, fmt.Errorf("rebuild conversation history index %q: %w", id, indexErr)
		}
		if searchErr != nil {
			return ConversationHistorySearchResult{}, fmt.Errorf("search conversation history %q: %w", id, searchErr)
		}
		for _, hit := range sessionCandidates {
			insertConversationHistoryCandidate(&candidates, hit, maxResults)
		}
		totalMatches += matched
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if !candidates[i].At.Equal(candidates[j].At) {
			return candidates[i].At.After(candidates[j].At)
		}
		if candidates[i].SessionID != candidates[j].SessionID {
			return candidates[i].SessionID < candidates[j].SessionID
		}
		return candidates[i].Sequence < candidates[j].Sequence
	})
	result := ConversationHistorySearchResult{Total: totalMatches, MoreAvailable: moreSessions || totalMatches > len(candidates)}
	for _, hit := range candidates {
		if len(result.Hits) >= maxResults {
			result.MoreAvailable = true
			break
		}
		candidate := hit
		candidate.Score = 0
		encoded, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return ConversationHistorySearchResult{}, marshalErr
		}
		if len(encoded) > maxBytes || resultBytes(result)+len(encoded) > maxBytes {
			result.MoreAvailable = true
			break
		}
		result.Hits = append(result.Hits, candidate)
	}
	return result, nil
}

type conversationHistoryIndexManifest struct {
	Kind         string    `json:"kind"`
	Version      int       `json:"version"`
	SessionID    SessionID `json:"session_id"`
	SourceSize   int64     `json:"source_size"`
	SourceMtime  int64     `json:"source_mtime_unix_nano"`
	EventCount   uint64    `json:"event_count"`
	MessageCount int       `json:"message_count"`
	ChunkCount   int       `json:"chunk_count"`
	DataSHA256   string    `json:"data_sha256"`
	Title        string    `json:"title,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type conversationHistoryIndexEntry struct {
	Kind           string    `json:"kind"`
	ID             string    `json:"id,omitempty"`
	Sequence       uint64    `json:"sequence,omitempty"`
	At             time.Time `json:"at,omitempty"`
	Role           Role      `json:"role,omitempty"`
	MessageOrdinal int       `json:"message_ordinal,omitempty"`
	Chunk          string    `json:"chunk,omitempty"`
}

func conversationHistoryIndexPath(path string) string { return path + ".search.idx" }

func ensureConversationHistoryIndex(ctx context.Context, path string, id SessionID) (*conversationHistoryIndexManifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat session file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session history path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("session history path is not a regular file")
	}
	if info.Size() > MaxConversationHistoryFileBytes {
		return nil, fmt.Errorf("session history file exceeds %d bytes", MaxConversationHistoryFileBytes)
	}
	indexPath := conversationHistoryIndexPath(path)
	if manifest, loadErr := loadConversationHistoryIndexManifest(indexPath, info, id); loadErr == nil {
		return manifest, nil
	} else if !errors.Is(loadErr, errConversationHistoryIndexStale) && !errors.Is(loadErr, errConversationHistoryIndexInvalid) {
		return nil, loadErr
	}
	manifest, buildErr := buildConversationHistoryIndex(ctx, path, indexPath, id, info)
	if buildErr != nil {
		return nil, buildErr
	}
	return manifest, nil
}

var (
	errConversationHistoryIndexStale   = errors.New("conversation history index is stale")
	errConversationHistoryIndexInvalid = errors.New("conversation history index is invalid")
)

func loadConversationHistoryIndexManifest(indexPath string, source os.FileInfo, id SessionID) (*conversationHistoryIndexManifest, error) {
	info, err := os.Lstat(indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errConversationHistoryIndexInvalid
		}
		return nil, fmt.Errorf("stat conversation history index: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errConversationHistoryIndexInvalid
	}
	if info.Size() > MaxConversationHistoryIndexBytes {
		return nil, errConversationHistoryIndexInvalid
	}
	file, err := os.Open(indexPath)
	if err != nil {
		return nil, errConversationHistoryIndexInvalid
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	line, readErr := reader.ReadBytes('\n')
	if readErr != nil && readErr != io.EOF {
		return nil, errConversationHistoryIndexInvalid
	}
	if len(line) == 0 || len(line) > MaxConversationHistoryLineBytes {
		return nil, errConversationHistoryIndexInvalid
	}
	var manifest conversationHistoryIndexManifest
	if err := json.Unmarshal(bytes.TrimSpace(line), &manifest); err != nil || manifest.Kind != "manifest" || manifest.Version != ConversationHistoryIndexVersion || manifest.SessionID != id || manifest.EventCount == 0 || manifest.MessageCount < 0 || manifest.ChunkCount < 0 || len(manifest.DataSHA256) != sha256.Size*2 || manifest.MessageCount == 0 && manifest.ChunkCount != 0 || manifest.MessageCount > 0 && manifest.ChunkCount == 0 {
		return nil, errConversationHistoryIndexInvalid
	}
	if manifest.SourceSize != source.Size() || manifest.SourceMtime != source.ModTime().UnixNano() {
		return nil, errConversationHistoryIndexStale
	}
	return &manifest, nil
}

func buildConversationHistoryIndex(ctx context.Context, path, indexPath string, id SessionID, source os.FileInfo) (*conversationHistoryIndexManifest, error) {
	root := filepath.Dir(indexPath)
	stage, err := os.CreateTemp(root, ".conversation-index-")
	if err != nil {
		return nil, fmt.Errorf("create conversation history index stage: %w", err)
	}
	stagePath := stage.Name()
	defer os.Remove(stagePath)
	dataPath := stagePath + ".data"
	data, err := os.OpenFile(dataPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		_ = stage.Close()
		return nil, fmt.Errorf("create conversation history index data: %w", err)
	}
	defer os.Remove(dataPath)
	manifest := conversationHistoryIndexManifest{Kind: "manifest", Version: ConversationHistoryIndexVersion, SessionID: id, SourceSize: source.Size(), SourceMtime: source.ModTime().UnixNano()}
	digest := sha256.New()
	encoder := json.NewEncoder(io.MultiWriter(data, digest))
	messageOrdinal := 0
	var scanErr error
	scanErr = scanHistoryEvents(ctx, path, id, func(event Event) error {
		manifest.EventCount++
		if manifest.CreatedAt.IsZero() {
			manifest.CreatedAt = event.At.UTC()
		}
		manifest.UpdatedAt = event.At.UTC()
		switch event.Type {
		case EventMetadata:
			meta, err := decodeEventPayload[MetadataEvent](event, EventMetadata)
			if err != nil {
				return err
			}
			manifest.Title = truncateUTF8(redactString(strings.TrimSpace(meta.Metadata.Title)), 256)
		case EventMessage:
			messageEvent, err := decodeEventPayload[MessageEvent](event, EventMessage)
			if err != nil {
				return err
			}
			if messageEvent.Message.Role != RoleUser && messageEvent.Message.Role != RoleAssistant {
				return nil
			}
			content := strings.TrimSpace(redactString(messageEvent.Message.Content))
			if content == "" {
				return nil
			}
			messageOrdinal++
			manifest.MessageCount++
			if err := forEachConversationHistoryUTF8Chunk(content, MaxConversationHistoryIndexChunk, func(chunk string) error {
				if err := encoder.Encode(conversationHistoryIndexEntry{Kind: "message", ID: event.ID, Sequence: event.Seq, At: event.At.UTC(), Role: messageEvent.Message.Role, MessageOrdinal: messageOrdinal, Chunk: chunk}); err != nil {
					return err
				}
				manifest.ChunkCount++
				info, err := data.Stat()
				if err != nil {
					return err
				}
				if info.Size() > MaxConversationHistoryIndexBytes {
					return fmt.Errorf("conversation history index exceeds %d bytes", MaxConversationHistoryIndexBytes)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if scanErr != nil {
		_ = data.Close()
		return nil, fmt.Errorf("scan session JSONL: %w", scanErr)
	}
	if err := data.Sync(); err != nil {
		_ = data.Close()
		return nil, err
	}
	if err := data.Close(); err != nil {
		return nil, err
	}
	if manifest.EventCount == 0 {
		return nil, errors.New("session history is empty")
	}
	manifest.DataSHA256 = hex.EncodeToString(digest.Sum(nil))
	if _, err := stage.WriteString("\n"); err != nil {
		_ = stage.Close()
		return nil, err
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		_ = stage.Close()
		return nil, err
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		_ = stage.Close()
		return nil, err
	}
	if err := stage.Truncate(0); err != nil {
		_ = stage.Close()
		return nil, err
	}
	if _, err := stage.Write(append(manifestData, '\n')); err != nil {
		_ = stage.Close()
		return nil, err
	}
	dataFile, err := os.Open(dataPath)
	if err != nil {
		_ = stage.Close()
		return nil, err
	}
	if _, err := io.Copy(stage, contextReader{ctx: ctx, reader: dataFile}); err != nil {
		_ = dataFile.Close()
		_ = stage.Close()
		return nil, err
	}
	_ = dataFile.Close()
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return nil, err
	}
	if err := stage.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(stagePath, indexPath); err != nil {
		return nil, fmt.Errorf("install conversation history index: %w", err)
	}
	return &manifest, nil
}

func scanHistoryEvents(ctx context.Context, path string, id SessionID, visit func(Event) error) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat session file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session history path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("session history path is not a regular file")
	}
	if info.Size() > MaxConversationHistoryFileBytes {
		return fmt.Errorf("session history file exceeds %d bytes", MaxConversationHistoryFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var sequence uint64
	for {
		if err := contextErr(ctx); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > MaxConversationHistoryLineBytes {
			return fmt.Errorf("session history line exceeds %d bytes", MaxConversationHistoryLineBytes)
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var event Event
			if err := json.Unmarshal(trimmed, &event); err != nil {
				return fmt.Errorf("decode session JSONL %q line %d: %w", path, sequence+1, err)
			}
			sequence++
			if event.SessionID != id || event.Seq != sequence || event.Type == "" || event.ID == "" || event.At.IsZero() {
				return fmt.Errorf("invalid session event %d", sequence)
			}
			if visit != nil {
				if err := visit(event); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			if len(bytes.TrimSpace(line)) > 0 && !bytes.HasSuffix(line, []byte{'\n'}) {
				return errors.New("session history has a truncated final line")
			}
			break
		}
		if readErr != nil {
			return fmt.Errorf("read session JSONL: %w", readErr)
		}
	}
	return nil
}

// historyMtimeSlack bounds how far a session file's mtime may trail the
// recorded time of its last event before the file is treated as stale.
const historyMtimeSlack = time.Minute

// historyTimeWindow is the optional [since, before) message-time filter.
type historyTimeWindow struct {
	since, before       time.Time
	hasSince, hasBefore bool
}

func (window historyTimeWindow) contains(at time.Time) bool {
	if window.hasSince && at.Before(window.since) {
		return false
	}
	if window.hasBefore && !at.Before(window.before) {
		return false
	}
	return true
}

func searchConversationHistoryIndex(ctx context.Context, indexPath string, manifest *conversationHistoryIndexManifest, id SessionID, query, needle string, terms []string, window historyTimeWindow, maxMessages, maxSnippet, maxResults int, candidates *[]ConversationHistoryHit) (int, error) {
	if manifest == nil {
		return 0, errConversationHistoryIndexInvalid
	}
	file, err := os.Open(indexPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	manifestLine, err := reader.ReadBytes('\n')
	if err != nil {
		return 0, errConversationHistoryIndexInvalid
	}
	var actual conversationHistoryIndexManifest
	if err := json.Unmarshal(bytes.TrimSpace(manifestLine), &actual); err != nil || actual.Kind != "manifest" || actual.Version != manifest.Version || actual.SessionID != id || actual.ChunkCount != manifest.ChunkCount || actual.DataSHA256 != manifest.DataSHA256 {
		return 0, errConversationHistoryIndexInvalid
	}
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(reader, digest))
	scanner.Buffer(make([]byte, 4096), MaxConversationHistoryLineBytes)
	cutoff := manifest.MessageCount - maxMessages
	if cutoff < 0 {
		cutoff = 0
	}
	total := 0
	currentID := ""
	currentOrdinal := 0
	lastOrdinal := 0
	chunkCount := 0
	currentMatched := false
	carry := ""
	for scanner.Scan() {
		if err := contextErr(ctx); err != nil {
			return 0, err
		}
		var entry conversationHistoryIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.Kind != "message" || entry.ID == "" || entry.Role != RoleUser && entry.Role != RoleAssistant || entry.MessageOrdinal <= 0 || entry.MessageOrdinal > manifest.MessageCount {
			return 0, errConversationHistoryIndexInvalid
		}
		chunkCount++
		if entry.ID != currentID {
			if lastOrdinal > 0 && entry.MessageOrdinal != lastOrdinal+1 {
				return 0, errConversationHistoryIndexInvalid
			}
			currentID = entry.ID
			currentOrdinal = entry.MessageOrdinal
			lastOrdinal = entry.MessageOrdinal
			currentMatched = false
			carry = ""
		}
		if currentOrdinal != entry.MessageOrdinal {
			return 0, errConversationHistoryIndexInvalid
		}
		if entry.MessageOrdinal <= cutoff || !window.contains(entry.At) {
			continue
		}
		combined := carry + entry.Chunk
		lower := strings.ToLower(combined)
		if !currentMatched && historyMatchesQuery(lower, needle, terms) {
			currentMatched = true
			total++
			score := historyMatchScore(lower, needle, terms)
			hit := ConversationHistoryHit{SessionID: id, SessionTitle: manifest.Title, MessageID: entry.ID, Sequence: entry.Sequence, Role: entry.Role, At: entry.At.UTC(), SessionCreatedAt: manifest.CreatedAt.UTC(), SessionUpdatedAt: manifest.UpdatedAt.UTC(), Snippet: historySearchSnippet(combined, query, terms, maxSnippet), Score: score}
			insertConversationHistoryCandidate(candidates, hit, maxResults)
		}
		carry = historyUTF8Suffix(combined, MaxConversationHistoryQueryBytes)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if lastOrdinal != manifest.MessageCount || chunkCount != manifest.ChunkCount || hex.EncodeToString(digest.Sum(nil)) != manifest.DataSHA256 {
		return 0, errConversationHistoryIndexInvalid
	}
	return total, nil
}

func forEachConversationHistoryUTF8Chunk(content string, maxBytes int, visit func(string) error) error {
	if maxBytes <= 0 {
		return nil
	}
	for start := 0; start < len(content); {
		end := start
		for end < len(content) {
			_, size := utf8.DecodeRuneInString(content[end:])
			if size == 0 {
				break
			}
			if end > start && end-start+size > maxBytes {
				break
			}
			end += size
		}
		if end == start {
			_, size := utf8.DecodeRuneInString(content[start:])
			end = start + size
		}
		if err := visit(content[start:end]); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func historyUTF8Suffix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func insertConversationHistoryCandidate(candidates *[]ConversationHistoryHit, hit ConversationHistoryHit, maxResults int) {
	if maxResults <= 0 {
		return
	}
	items := *candidates
	items = append(items, hit)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if !items[i].At.Equal(items[j].At) {
			return items[i].At.After(items[j].At)
		}
		if items[i].SessionID != items[j].SessionID {
			return items[i].SessionID < items[j].SessionID
		}
		return items[i].Sequence < items[j].Sequence
	})
	if len(items) > maxResults {
		items = items[:maxResults]
	}
	*candidates = items
}

func boundPositive(value, maximum int) int {
	if value <= 0 || value > maximum {
		return maximum
	}
	return value
}

func historyQueryTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	terms := make([]string, 0, len(fields))
	stopWords := map[string]struct{}{
		"a": {}, "about": {}, "an": {}, "and": {}, "are": {}, "did": {}, "do": {}, "for": {},
		"have": {}, "i": {}, "in": {}, "is": {}, "it": {}, "me": {}, "of": {}, "on": {},
		"our": {}, "the": {}, "that": {}, "this": {}, "to": {}, "was": {}, "we": {}, "what": {},
		"when": {}, "where": {}, "who": {}, "with": {}, "you": {},
	}
	for _, term := range fields {
		if term == "" {
			continue
		}
		if _, stop := stopWords[term]; stop {
			continue
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return fields
	}
	return terms
}

func historyMatchesQuery(content, query string, terms []string) bool {
	if strings.Contains(content, query) {
		return true
	}
	for _, term := range terms {
		if strings.Contains(content, term) {
			return true
		}
	}
	return false
}

func historyMatchScore(content, query string, terms []string) int {
	matched := 0
	for _, term := range terms {
		if strings.Contains(content, term) {
			matched++
		}
	}
	score := matched * 10
	if matched == len(terms) {
		score += 20
	}
	if strings.Contains(content, query) {
		score += 100
	}
	if strings.HasPrefix(content, query) {
		score += 20
	}
	return score
}

func historySearchSnippet(content, query string, terms []string, maxBytes int) string {
	if strings.Contains(strings.ToLower(content), strings.ToLower(query)) {
		return sessionSearchSnippet(content, query, maxBytes)
	}
	for _, term := range terms {
		if strings.Contains(strings.ToLower(content), term) {
			return sessionSearchSnippet(content, term, maxBytes)
		}
	}
	return truncateUTF8(redactString(strings.Join(strings.Fields(content), " ")), maxBytes)
}

func resultBytes(result ConversationHistorySearchResult) int {
	data, _ := json.Marshal(result)
	return len(data)
}

// ConversationHistoryService is the narrow read-only seam consumed by tools
// and adapters; implementations must preserve local actor scope and bounds.
type ConversationHistoryService interface {
	Search(context.Context, ConversationHistorySearchRequest) (ConversationHistorySearchResult, error)
}

var _ ConversationHistoryService = (*ConversationHistorySearch)(nil)
