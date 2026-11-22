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
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSemanticEndpoint       = "http://127.0.0.1:8090"
	DefaultSemanticModel          = "Qwen3-Embedding-0.6B"
	DefaultSemanticTimeout        = 15 * time.Second
	SemanticIndexFormatVersion    = 3
	SemanticIndexManifestFile     = "manifest.json"
	SemanticIndexVectorsFile      = "vectors.jsonl"
	MaxSemanticIndexBytes         = 1 << 30
	MaxSemanticIndexRecords       = 100_000
	MaxSemanticVectorLineBytes    = 1 << 20
	MaxSemanticEmbeddingBodyBytes = 16 << 20
)

// SemanticHealthCode is a compact, stable diagnostic vocabulary for the
// optional M6 layer. Lexical retrieval remains authoritative when the code is
// anything other than ready/empty.
type SemanticHealthCode string

const (
	SemanticHealthDisabled            SemanticHealthCode = "disabled"
	SemanticHealthReady               SemanticHealthCode = "ready"
	SemanticHealthEmpty               SemanticHealthCode = "empty"
	SemanticHealthMissing             SemanticHealthCode = "missing"
	SemanticHealthStale               SemanticHealthCode = "stale"
	SemanticHealthInvalid             SemanticHealthCode = "invalid"
	SemanticHealthMisconfigured       SemanticHealthCode = "misconfigured"
	SemanticHealthProviderUnavailable SemanticHealthCode = "provider_unavailable"
	SemanticHealthModelMismatch       SemanticHealthCode = "model_mismatch"
	SemanticHealthRootMismatch        SemanticHealthCode = "root_mismatch"
)

func (code SemanticHealthCode) valid() bool {
	switch code {
	case "", SemanticHealthDisabled, SemanticHealthReady, SemanticHealthEmpty, SemanticHealthMissing, SemanticHealthStale, SemanticHealthInvalid, SemanticHealthMisconfigured, SemanticHealthProviderUnavailable, SemanticHealthModelMismatch, SemanticHealthRootMismatch:
		return true
	default:
		return false
	}
}

type SemanticHealthStatus struct {
	Code        SemanticHealthCode `json:"code"`
	Enabled     bool               `json:"enabled"`
	IndexRoot   string             `json:"index_root,omitempty"`
	Model       string             `json:"model,omitempty"`
	RecordCount int                `json:"record_count,omitempty"`
}

type SemanticConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// BaseURL is retained as a compatibility alias for early M6 config files.
	BaseURL        string `json:"base_url,omitempty"`
	Model          string `json:"model,omitempty"`
	APIKeyFile     string `json:"api_key_file,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	IndexRoot      string `json:"index_root,omitempty"`
}

// Validate checks the operator-facing semantic configuration without touching
// the filesystem. A missing or stale derived index is intentionally valid: the
// composition root degrades to lexical retrieval until it is rebuilt.
func (config SemanticConfig) Validate() error {
	if strings.TrimSpace(config.Endpoint) != "" && strings.TrimSpace(config.BaseURL) != "" && strings.TrimRight(strings.TrimSpace(config.Endpoint), "/") != strings.TrimRight(strings.TrimSpace(config.BaseURL), "/") {
		return errors.New("semantic endpoint and base_url must match when both are configured")
	}
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(config.BaseURL)
	}
	if endpoint != "" {
		if _, err := NewOpenAICompatibleEmbedder(endpoint, firstNonEmpty(config.Model, DefaultSemanticModel), "", semanticTimeout(config)); err != nil {
			return fmt.Errorf("semantic config: %w", err)
		}
	}
	if strings.TrimSpace(config.Model) != "" {
		if err := ValidateModelName(config.Model); err != nil {
			return fmt.Errorf("semantic model: %w", err)
		}
	}
	if config.TimeoutSeconds < 0 {
		return errors.New("semantic timeout_seconds must not be negative")
	}
	if config.TimeoutSeconds > 120 {
		return errors.New("semantic timeout_seconds must not exceed 120")
	}
	if strings.TrimSpace(config.IndexRoot) != "" {
		root, err := ResolveBeeHomePath(config.IndexRoot)
		if err != nil {
			return fmt.Errorf("semantic index_root: %w", err)
		}
		if err := ValidateBeePath(root); err != nil {
			return fmt.Errorf("semantic index_root: %w", err)
		}
	}
	if strings.ContainsRune(config.APIKeyFile, '\x00') {
		return errors.New("semantic api_key_file contains NUL")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func semanticTimeout(config SemanticConfig) time.Duration {
	if config.TimeoutSeconds <= 0 {
		return DefaultSemanticTimeout
	}
	return time.Duration(config.TimeoutSeconds) * time.Second
}

func (config SemanticConfig) endpoint() string {
	return firstNonEmpty(config.Endpoint, config.BaseURL, DefaultSemanticEndpoint)
}

func (config SemanticConfig) EffectiveEndpoint() string { return config.endpoint() }

func (config SemanticConfig) EffectiveModel() string {
	return firstNonEmpty(config.Model, DefaultSemanticModel)
}

func (config SemanticConfig) EffectiveTimeout() time.Duration { return semanticTimeout(config) }

func (config SemanticConfig) EffectiveIndexRoot() (string, error) {
	if strings.TrimSpace(config.IndexRoot) == "" {
		return DefaultBeeSemanticIndexRoot()
	}
	return ResolveBeeHomePath(config.IndexRoot)
}

func (config SemanticConfig) NewEmbedder() (*OpenAICompatibleEmbedder, error) {
	apiKey := ""
	if strings.TrimSpace(config.APIKeyFile) != "" {
		path, err := ResolveBeeHomePath(config.APIKeyFile)
		if err != nil {
			return nil, err
		}
		apiKey, err = (ProviderConfig{APIKeyFile: path}).APIKey()
		if err != nil {
			return nil, err
		}
	}
	return NewOpenAICompatibleEmbedder(config.EffectiveEndpoint(), config.EffectiveModel(), apiKey, config.EffectiveTimeout())
}

// RetrievalOptions resolves the configured local semantic components. It does
// not fail composition when the derived index is absent, malformed, stale, or
// tied to another memory root; those conditions are reported as bounded
// degraded diagnostics by the normal lexical fallback path.
func (config SemanticConfig) RetrievalOptions(memoryRoot string) MemoryRetrievalOptions {
	if !config.Enabled {
		return MemoryRetrievalOptions{SemanticHealth: SemanticHealthDisabled}
	}
	options := MemoryRetrievalOptions{SemanticEnabled: true, SemanticTimeout: semanticTimeout(config)}
	health := config.Health(memoryRoot)
	options.SemanticHealth = health.Code
	indexRoot, _ := config.EffectiveIndexRoot()
	if indexRoot == "" || strings.TrimSpace(memoryRoot) == "" {
		return options
	}
	embedder, err := config.NewEmbedder()
	if err != nil {
		options.SemanticHealth = SemanticHealthProviderUnavailable
		return options
	}
	options.Embedder = embedder
	options.VectorIndex = &LiveMemoryVectorIndex{MemoryRoot: memoryRoot, IndexRoot: indexRoot, Model: config.EffectiveModel()}
	return options
}

// Health diagnoses configuration and derived-index state without contacting a
// provider. Provider outages are reported at retrieval time as
// provider_unavailable; this method stays deterministic and read-only.
func (config SemanticConfig) Health(memoryRoot string) SemanticHealthStatus {
	status := SemanticHealthStatus{Code: SemanticHealthDisabled, Enabled: config.Enabled, Model: config.EffectiveModel()}
	if !config.Enabled {
		return status
	}
	if err := config.Validate(); err != nil {
		status.Code = SemanticHealthMisconfigured
		return status
	}
	if strings.TrimSpace(memoryRoot) == "" {
		status.Code = SemanticHealthRootMismatch
		return status
	}
	indexRoot, _ := config.EffectiveIndexRoot()
	status.IndexRoot = indexRoot
	if indexRoot == "" {
		status.Code = SemanticHealthMissing
		return status
	}
	info, err := os.Lstat(indexRoot)
	if errors.Is(err, os.ErrNotExist) {
		status.Code = SemanticHealthMissing
		return status
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		status.Code = SemanticHealthInvalid
		return status
	}
	index, err := LoadSemanticIndex(indexRoot)
	if err != nil {
		status.Code = SemanticHealthInvalid
		return status
	}
	manifest := index.Manifest()
	status.RecordCount = manifest.RecordCount
	if filepath.Clean(manifest.MemoryRoot) != filepath.Clean(memoryRoot) {
		status.Code = SemanticHealthRootMismatch
		return status
	}
	if manifest.Model != status.Model {
		status.Code = SemanticHealthModelMismatch
		return status
	}
	if _, err := VerifySemanticIndex(memoryRoot, indexRoot); err != nil {
		status.Code = SemanticHealthStale
		return status
	}
	if manifest.RecordCount == 0 {
		status.Code = SemanticHealthEmpty
	} else {
		status.Code = SemanticHealthReady
	}
	return status
}

func DiagnoseSemanticHealth(config SemanticConfig, memoryRoot string) SemanticHealthStatus {
	return config.Health(memoryRoot)
}

type OpenAICompatibleEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
	apiKey  string
}

func NewOpenAICompatibleEmbedder(baseURL, model, apiKey string, timeout time.Duration) (*OpenAICompatibleEmbedder, error) {
	baseURL = strings.TrimSpace(strings.TrimRight(baseURL, "/"))
	model = strings.TrimSpace(model)
	if baseURL == "" || model == "" {
		return nil, errors.New("semantic endpoint and model are required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("semantic endpoint must be an absolute http(s) URL")
	}
	if timeout <= 0 {
		timeout = DefaultSemanticTimeout
	}
	if timeout > 2*time.Minute {
		return nil, errors.New("semantic timeout exceeds two minutes")
	}
	return &OpenAICompatibleEmbedder{baseURL: baseURL, model: model, apiKey: apiKey, client: &http.Client{Timeout: timeout}}, nil
}

type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}
type embeddingsResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

func (embedder *OpenAICompatibleEmbedder) Embed(ctx context.Context, request MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	if embedder == nil {
		return MemoryEmbedding{}, errors.New("semantic embedder is nil")
	}
	if err := contextErr(ctx); err != nil {
		return MemoryEmbedding{}, err
	}
	input := strings.TrimSpace(request.QueryText)
	if input == "" {
		return MemoryEmbedding{}, errors.New("semantic embedding input is empty")
	}
	if len(input) > MaxMemoryQueryTextLength {
		return MemoryEmbedding{}, errors.New("semantic embedding input exceeds bound")
	}
	body, err := json.Marshal(embeddingsRequest{Model: embedder.model, Input: input})
	if err != nil {
		return MemoryEmbedding{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, embeddingsURL(embedder.baseURL), bytes.NewReader(body))
	if err != nil {
		return MemoryEmbedding{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if embedder.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+embedder.apiKey)
	}
	response, err := embedder.client.Do(httpRequest)
	if err != nil {
		return MemoryEmbedding{}, fmt.Errorf("semantic embedder request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return MemoryEmbedding{}, fmt.Errorf("semantic embedder returned HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxSemanticEmbeddingBodyBytes+1))
	if err != nil {
		return MemoryEmbedding{}, err
	}
	if len(data) > MaxSemanticEmbeddingBodyBytes {
		return MemoryEmbedding{}, errors.New("semantic embedding response exceeds bound")
	}
	var payload embeddingsResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return MemoryEmbedding{}, fmt.Errorf("decode semantic embedding response: %w", err)
	}
	if len(payload.Data) != 1 {
		return MemoryEmbedding{}, errors.New("semantic embedding response must contain exactly one vector")
	}
	embedding := MemoryEmbedding{Values: payload.Data[0].Embedding}
	if err := embedding.Validate(); err != nil {
		return MemoryEmbedding{}, err
	}
	return embedding, nil
}

func embeddingsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/embeddings") {
		return base
	}
	if parsed, err := url.Parse(base); err == nil && strings.HasSuffix(parsed.Path, "/v1") {
		return base + "/embeddings"
	}
	return base + "/v1/embeddings"
}

type SemanticIndexManifest struct {
	FormatVersion   int       `json:"format_version"`
	MemoryRoot      string    `json:"memory_root"`
	EventSHA256     string    `json:"event_sha256"`
	EventSize       int64     `json:"event_size,omitempty"`
	EventMtimeNS    int64     `json:"event_mtime_unix_nano,omitempty"`
	VectorsSHA256   string    `json:"vectors_sha256,omitempty"`
	RecordCount     int       `json:"record_count"`
	Dimensions      int       `json:"dimensions"`
	Model           string    `json:"model"`
	BuiltAt         time.Time `json:"built_at"`
	ReusedRecords   int       `json:"reused_records,omitempty"`
	EmbeddedRecords int       `json:"embedded_records,omitempty"`
}

type semanticVectorEntry struct {
	RecordID      string    `json:"record_id"`
	ContentSHA256 string    `json:"content_sha256"`
	Values        []float64 `json:"values"`
}

type FileMemoryVectorIndex struct {
	manifest SemanticIndexManifest
	vectors  map[string]MemoryEmbedding
	hashes   map[string]string
	healthMu sync.Mutex
	health   semanticRuntimeHealthCache
}

// LiveMemoryVectorIndex reloads an atomically published index generation when
// its manifest changes. Retrieval options can therefore be composed once and
// become healthy after an asynchronous refresh without restarting Bee.
type LiveMemoryVectorIndex struct {
	MemoryRoot string
	IndexRoot  string
	Model      string

	mu            sync.Mutex
	manifestSize  int64
	manifestMtime int64
	index         *FileMemoryVectorIndex
}

func (index *LiveMemoryVectorIndex) Search(ctx context.Context, query MemoryEmbedding, limit int) ([]MemoryVectorHit, error) {
	return index.SearchFiltered(ctx, query, nil, limit)
}

func (index *LiveMemoryVectorIndex) SearchFiltered(ctx context.Context, query MemoryEmbedding, allowed map[string]struct{}, limit int) ([]MemoryVectorHit, error) {
	loaded, err := index.current()
	if err != nil {
		return nil, err
	}
	if code := index.runtimeHealth(ctx); code != SemanticHealthReady && code != SemanticHealthEmpty {
		return nil, fmt.Errorf("semantic index is not ready: %s", code)
	}
	return loaded.SearchFiltered(ctx, query, allowed, limit)
}

func (index *LiveMemoryVectorIndex) current() (*FileMemoryVectorIndex, error) {
	if index == nil || strings.TrimSpace(index.MemoryRoot) == "" || strings.TrimSpace(index.IndexRoot) == "" {
		return nil, errors.New("semantic live index is not configured")
	}
	info, err := os.Lstat(filepath.Join(index.IndexRoot, SemanticIndexManifestFile))
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("semantic index manifest is not a regular file")
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.index != nil && index.manifestSize == size && index.manifestMtime == mtime {
		return index.index, nil
	}
	loaded, err := LoadSemanticIndex(index.IndexRoot)
	if err != nil {
		return nil, err
	}
	index.index = loaded
	index.manifestSize = size
	index.manifestMtime = mtime
	return loaded, nil
}

func (index *LiveMemoryVectorIndex) runtimeHealth(ctx context.Context) SemanticHealthCode {
	loaded, err := index.current()
	if errors.Is(err, os.ErrNotExist) {
		return SemanticHealthMissing
	}
	if err != nil {
		return SemanticHealthInvalid
	}
	manifest := loaded.Manifest()
	if filepath.Clean(manifest.MemoryRoot) != filepath.Clean(index.MemoryRoot) {
		return SemanticHealthRootMismatch
	}
	if manifest.Model != index.Model {
		return SemanticHealthModelMismatch
	}
	if code := loaded.runtimeHealth(ctx); code != "" {
		return code
	}
	if manifest.RecordCount == 0 {
		return SemanticHealthEmpty
	}
	return SemanticHealthReady
}

type semanticRuntimeHealthCache struct {
	set     bool
	size    int64
	mtimeNS int64
	code    SemanticHealthCode
}

func (index *FileMemoryVectorIndex) Search(ctx context.Context, query MemoryEmbedding, limit int) ([]MemoryVectorHit, error) {
	return index.SearchFiltered(ctx, query, nil, limit)
}

func (index *FileMemoryVectorIndex) SearchFiltered(ctx context.Context, query MemoryEmbedding, allowed map[string]struct{}, limit int) ([]MemoryVectorHit, error) {
	if index == nil {
		return nil, errors.New("semantic vector index is nil")
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxMemorySemanticHits {
		return nil, errors.New("semantic vector search limit is invalid")
	}
	type scored struct {
		id    string
		score float64
	}
	values := make([]scored, 0, len(index.vectors))
	for id, vector := range index.vectors {
		if allowed != nil {
			if _, ok := allowed[id]; !ok {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if len(vector.Values) != len(query.Values) {
			continue
		}
		values = append(values, scored{id: id, score: cosineSimilarity(query.Values, vector.Values)})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].score != values[j].score {
			return values[i].score > values[j].score
		}
		return values[i].id < values[j].id
	})
	if len(values) > limit {
		values = values[:limit]
	}
	hits := make([]MemoryVectorHit, 0, len(values))
	for _, item := range values {
		hits = append(hits, MemoryVectorHit{RecordID: item.id, Score: item.score})
	}
	return hits, nil
}

func cosineSimilarity(left, right []float64) float64 {
	dot, leftNorm, rightNorm := 0.0, 0.0, 0.0
	for i := range left {
		dot += left[i] * right[i]
		leftNorm += left[i] * left[i]
		rightNorm += right[i] * right[i]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	value := dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func LoadSemanticIndex(root string) (*FileMemoryVectorIndex, error) {
	if err := ValidateBeePath(root); err != nil {
		return nil, err
	}
	lock, err := acquireSessionFileLock(semanticIndexLockPath(root))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("inspect semantic index root")
	}
	manifestPath := filepath.Join(root, SemanticIndexManifestFile)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return nil, errors.New("read semantic index manifest")
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, errors.New("read semantic index manifest")
	}
	var manifest SemanticIndexManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, errors.New("decode semantic index manifest")
	}
	if manifest.FormatVersion != SemanticIndexFormatVersion || manifest.RecordCount < 0 || manifest.RecordCount > MaxSemanticIndexRecords || manifest.Dimensions < 0 || manifest.Dimensions > MaxMemoryEmbeddingDimensions || manifest.Model == "" || manifest.ReusedRecords < 0 || manifest.EmbeddedRecords < 0 || manifest.ReusedRecords+manifest.EmbeddedRecords != manifest.RecordCount || len(manifest.VectorsSHA256) != sha256.Size*2 || manifest.RecordCount == 0 && manifest.Dimensions != 0 || manifest.RecordCount > 0 && manifest.Dimensions == 0 {
		return nil, errors.New("invalid semantic index manifest")
	}
	vectorPath := filepath.Join(root, SemanticIndexVectorsFile)
	info, err := os.Lstat(vectorPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxSemanticIndexBytes {
		return nil, errors.New("inspect semantic vector index")
	}
	file, err := os.Open(vectorPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	vectors := make(map[string]MemoryEmbedding, minInt(manifest.RecordCount, MaxSemanticIndexRecords))
	hashes := make(map[string]string, minInt(manifest.RecordCount, MaxSemanticIndexRecords))
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, digest))
	scanner.Buffer(make([]byte, 4096), MaxSemanticVectorLineBytes)
	for scanner.Scan() {
		var entry semanticVectorEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, errors.New("decode semantic vector entry")
		}
		if err := validateSafeID("semantic vector record_id", entry.RecordID, MaxMemoryIDLength); err != nil {
			return nil, err
		}
		if len(entry.ContentSHA256) != sha256.Size*2 {
			return nil, errors.New("invalid semantic vector content checksum")
		}
		if _, err := hex.DecodeString(entry.ContentSHA256); err != nil {
			return nil, errors.New("invalid semantic vector content checksum")
		}
		embedding := MemoryEmbedding{Values: entry.Values}
		if err := embedding.Validate(); err != nil {
			return nil, err
		}
		if len(embedding.Values) != manifest.Dimensions {
			return nil, errors.New("semantic vector dimensions do not match manifest")
		}
		if _, exists := vectors[entry.RecordID]; exists {
			return nil, errors.New("duplicate semantic vector record")
		}
		vectors[entry.RecordID] = embedding
		hashes[entry.RecordID] = entry.ContentSHA256
		if len(vectors) > MaxSemanticIndexRecords {
			return nil, errors.New("semantic index exceeds record limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(vectors) != manifest.RecordCount {
		return nil, errors.New("semantic index record count does not match manifest")
	}
	if hex.EncodeToString(digest.Sum(nil)) != manifest.VectorsSHA256 {
		return nil, errors.New("semantic vector index checksum does not match manifest")
	}
	return &FileMemoryVectorIndex{manifest: manifest, vectors: vectors, hashes: hashes}, nil
}

func (index *FileMemoryVectorIndex) Manifest() SemanticIndexManifest {
	if index == nil {
		return SemanticIndexManifest{}
	}
	return index.manifest
}

func (index *FileMemoryVectorIndex) markVerified(root string) {
	if index == nil {
		return
	}
	size, mtimeNS, err := memoryEventMetadata(root)
	if err != nil {
		return
	}
	index.healthMu.Lock()
	index.health = semanticRuntimeHealthCache{set: true, size: size, mtimeNS: mtimeNS}
	index.healthMu.Unlock()
}

func (index *FileMemoryVectorIndex) runtimeHealth(ctx context.Context) SemanticHealthCode {
	if index == nil {
		return SemanticHealthInvalid
	}
	root := filepath.Clean(index.manifest.MemoryRoot)
	if root == "." || root == "" {
		return SemanticHealthRootMismatch
	}
	size, mtimeNS, err := memoryEventMetadata(root)
	if err != nil {
		return SemanticHealthStale
	}
	index.healthMu.Lock()
	if index.health.set && index.health.size == size && index.health.mtimeNS == mtimeNS {
		code := index.health.code
		index.healthMu.Unlock()
		return code
	}
	index.healthMu.Unlock()
	hash, err := memoryEventSHA256Context(ctx, root)
	code := SemanticHealthCode("")
	if err != nil || hash != index.manifest.EventSHA256 {
		code = SemanticHealthStale
	}
	index.healthMu.Lock()
	index.health = semanticRuntimeHealthCache{set: true, size: size, mtimeNS: mtimeNS, code: code}
	index.healthMu.Unlock()
	return code
}

func BuildSemanticIndex(ctx context.Context, memoryRoot, indexRoot, model string, embedder MemoryEmbedder) (SemanticIndexManifest, error) {
	if err := ValidateBeePath(memoryRoot); err != nil {
		return SemanticIndexManifest{}, err
	}
	if err := ValidateBeePath(indexRoot); err != nil {
		return SemanticIndexManifest{}, err
	}
	if embedder == nil {
		return SemanticIndexManifest{}, errors.New("semantic embedder is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return SemanticIndexManifest{}, errors.New("semantic model is required")
	}
	service, err := NewLocalMemoryService(memoryRoot, DefaultMemoryPolicy())
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	export, err := service.Export(ctx, MemoryExportRequest{Actor: MemoryActor{Kind: MemoryActorParent, ID: "bee-semantic-index"}, AuditID: "semantic-index-rebuild", Reason: "explicit local semantic index rebuild"})
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	eventHash, err := memoryEventSHA256Context(ctx, memoryRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	eventSize, eventMtimeNS, err := memoryEventMetadata(memoryRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	var previous *FileMemoryVectorIndex
	if loaded, loadErr := LoadSemanticIndex(indexRoot); loadErr == nil && loaded.manifest.Model == model && filepath.Clean(loaded.manifest.MemoryRoot) == filepath.Clean(memoryRoot) {
		previous = loaded
	}
	entries := make([]semanticVectorEntry, 0, len(export.Memory))
	dimensions := 0
	reusedRecords := 0
	embeddedRecords := 0
	for _, record := range export.Memory {
		select {
		case <-ctx.Done():
			return SemanticIndexManifest{}, ctx.Err()
		default:
		}
		text := semanticRecordText(record)
		contentHash := semanticContentSHA256(text)
		embedding := MemoryEmbedding{}
		if previous != nil && previous.hashes[record.ID] == contentHash {
			embedding = previous.vectors[record.ID]
			reusedRecords++
		} else {
			var embedErr error
			embedding, embedErr = embedder.Embed(ctx, MemoryEmbeddingRequest{QueryText: text, AllowMemoryText: true})
			if embedErr != nil {
				return SemanticIndexManifest{}, fmt.Errorf("embed memory record %s: %w", record.ID, embedErr)
			}
			embeddedRecords++
		}
		if err := embedding.Validate(); err != nil {
			return SemanticIndexManifest{}, err
		}
		if dimensions == 0 {
			dimensions = len(embedding.Values)
		}
		if len(embedding.Values) != dimensions {
			return SemanticIndexManifest{}, errors.New("semantic embeddings have inconsistent dimensions")
		}
		entries = append(entries, semanticVectorEntry{RecordID: record.ID, ContentSHA256: contentHash, Values: embedding.Values})
	}
	currentHash, err := memoryEventSHA256Context(ctx, memoryRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	currentSize, currentMtimeNS, err := memoryEventMetadata(memoryRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	if currentHash != eventHash || currentSize != eventSize || currentMtimeNS != eventMtimeNS {
		return SemanticIndexManifest{}, errors.New("memory changed during semantic index rebuild")
	}
	manifest := SemanticIndexManifest{FormatVersion: SemanticIndexFormatVersion, MemoryRoot: memoryRoot, EventSHA256: eventHash, EventSize: eventSize, EventMtimeNS: eventMtimeNS, RecordCount: len(entries), Dimensions: dimensions, Model: model, BuiltAt: time.Now().UTC(), ReusedRecords: reusedRecords, EmbeddedRecords: embeddedRecords}
	if err := writeSemanticIndexAtomic(indexRoot, manifest, entries); err != nil {
		return SemanticIndexManifest{}, err
	}
	return manifest, nil
}

func semanticRecordText(record MemoryRecord) string {
	return string(record.Namespace) + "\n" + record.Key + "\n" + record.Summary + "\n" + record.Content
}

func semanticContentSHA256(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func memoryEventSHA256(root string) (string, error) {
	return memoryEventSHA256Context(context.Background(), root)
}

func memoryEventSHA256Context(ctx context.Context, root string) (string, error) {
	path := filepath.Join(root, memoryEventsFile)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		empty := sha256.Sum256(nil)
		return hex.EncodeToString(empty[:]), nil
	}
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", errors.New("memory event log is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > MaxMemoryBytes {
		return "", fmt.Errorf("memory event log exceeds %d bytes", MaxMemoryBytes)
	}
	digest := sha256.New()
	copied, err := io.CopyN(digest, contextReader{ctx: ctx, reader: file}, info.Size())
	if err != nil || copied != info.Size() {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return "", errors.New("memory event log changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func memoryEventMetadata(root string) (int64, int64, error) {
	path := filepath.Join(root, memoryEventsFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, 0, errors.New("memory event log is not a regular file")
	}
	if info.Size() > MaxMemoryBytes {
		return 0, 0, fmt.Errorf("memory event log exceeds %d bytes", MaxMemoryBytes)
	}
	return info.Size(), info.ModTime().UnixNano(), nil
}

func semanticIndexLockPath(root string) string { return filepath.Clean(root) + ".lock" }

func writeSemanticIndexAtomic(root string, manifest SemanticIndexManifest, entries []semanticVectorEntry) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(root)), 0o700); err != nil {
		return err
	}
	lock, err := acquireSessionFileLock(semanticIndexLockPath(root))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".semantic-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(stage, SemanticIndexVectorsFile), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	digest := sha256.New()
	encoder := json.NewEncoder(io.MultiWriter(file, digest))
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			file.Close()
			return err
		}
		if info, statErr := file.Stat(); statErr != nil {
			file.Close()
			return statErr
		} else if info.Size() > MaxSemanticIndexBytes {
			file.Close()
			return fmt.Errorf("semantic vector index exceeds %d bytes", MaxSemanticIndexBytes)
		}
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	manifest.VectorsSHA256 = hex.EncodeToString(digest.Sum(nil))
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(filepath.Join(stage, SemanticIndexManifestFile), manifestData, 0o600); err != nil {
		return err
	}
	// Publish vectors before the manifest. A crash between renames leaves the
	// old manifest pointing at a checksum-mismatched vector file, which fails
	// closed instead of silently serving mixed-generation rankings.
	for _, name := range []string{SemanticIndexVectorsFile, SemanticIndexManifestFile} {
		target := filepath.Join(root, name)
		temp := filepath.Join(stage, name)
		if err := os.Rename(temp, target+".new"); err != nil {
			return err
		}
		if err := os.Rename(target+".new", target); err != nil {
			return err
		}
	}
	return nil
}

func VerifySemanticIndex(memoryRoot, indexRoot string) (SemanticIndexManifest, error) {
	index, err := LoadSemanticIndex(indexRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	if filepath.Clean(index.manifest.MemoryRoot) != filepath.Clean(memoryRoot) {
		return SemanticIndexManifest{}, errors.New("semantic index memory root mismatch")
	}
	currentHash, err := memoryEventSHA256(memoryRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	if currentHash != index.manifest.EventSHA256 {
		return SemanticIndexManifest{}, errors.New("semantic index is stale relative to memory event log")
	}
	return index.manifest, nil
}

func SemanticIndexStatus(indexRoot string) (SemanticIndexManifest, error) {
	index, err := LoadSemanticIndex(indexRoot)
	if err != nil {
		return SemanticIndexManifest{}, err
	}
	return index.manifest, nil
}

// SemanticIndexRefresher coalesces memory mutations into incremental,
// atomically published semantic index builds. Refresh failures never roll back
// the durable memory write; lexical retrieval remains available while health
// diagnostics report the stale generation.
type SemanticIndexRefresher struct {
	memoryRoot string
	indexRoot  string
	model      string
	embedder   MemoryEmbedder

	buildMu sync.Mutex
	mu      sync.Mutex
	running bool
	pending bool
	lastErr error
	last    SemanticIndexManifest
}

func NewSemanticIndexRefresher(memoryRoot, indexRoot, model string, embedder MemoryEmbedder) (*SemanticIndexRefresher, error) {
	if err := ValidateBeePath(memoryRoot); err != nil {
		return nil, err
	}
	if err := ValidateBeePath(indexRoot); err != nil {
		return nil, err
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("semantic refresh model is required")
	}
	if embedder == nil {
		return nil, errors.New("semantic refresh embedder is required")
	}
	return &SemanticIndexRefresher{memoryRoot: memoryRoot, indexRoot: indexRoot, model: model, embedder: embedder}, nil
}

func (refresher *SemanticIndexRefresher) Refresh(ctx context.Context) (SemanticIndexManifest, error) {
	if refresher == nil {
		return SemanticIndexManifest{}, errors.New("semantic index refresher is nil")
	}
	refresher.buildMu.Lock()
	defer refresher.buildMu.Unlock()
	manifest, err := BuildSemanticIndex(ctx, refresher.memoryRoot, refresher.indexRoot, refresher.model, refresher.embedder)
	refresher.mu.Lock()
	refresher.last, refresher.lastErr = manifest, err
	refresher.mu.Unlock()
	return manifest, err
}

// Notify requests a background refresh. Concurrent notifications collapse
// into at most one additional pass after the current build.
func (refresher *SemanticIndexRefresher) Notify() {
	if refresher == nil {
		return
	}
	refresher.mu.Lock()
	refresher.pending = true
	if refresher.running {
		refresher.mu.Unlock()
		return
	}
	refresher.running = true
	refresher.mu.Unlock()
	go refresher.drain()
}

func (refresher *SemanticIndexRefresher) drain() {
	for {
		refresher.mu.Lock()
		refresher.pending = false
		refresher.mu.Unlock()
		_, _ = refresher.Refresh(context.Background())
		refresher.mu.Lock()
		if !refresher.pending {
			refresher.running = false
			refresher.mu.Unlock()
			return
		}
		refresher.mu.Unlock()
	}
}

func (refresher *SemanticIndexRefresher) Status() (SemanticIndexManifest, error, bool) {
	if refresher == nil {
		return SemanticIndexManifest{}, errors.New("semantic index refresher is nil"), false
	}
	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	return refresher.last, refresher.lastErr, refresher.running || refresher.pending
}
