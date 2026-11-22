package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxMediaURLBytes = MaxWebURLBytes
	// MaxMediaBytes is retained as the conservative image/general media cap.
	MaxMediaBytes              int64 = MaxArtifactBytes
	MaxMediaImageBytes         int64 = MaxArtifactBytes
	MaxMediaVideoBytes         int64 = MaxFileBackedArtifactBytes
	MaxMediaDiskBytes          int64 = 2 << 30
	MaxMediaDuration                 = 3 * time.Hour
	MaxMediaRedirects                = 5
	MaxMediaImagePixels              = 25_000_000
	MaxMediaImageDimension           = 8192
	MaxMediaCommandOutputBytes       = 64 << 10
	MaxMediaErrorExcerptBytes        = 1024
	defaultMediaServiceTimeout       = 4 * time.Hour
)

const (
	mediaCommandVideo CommandName = "video"
	mediaCommandImage CommandName = "image"
)

// MediaCommand is an explicit local command. URL is never interpreted as a
// shell fragment; it is validated as a public HTTP(S) URL before use.
type MediaCommand struct {
	Kind CommandName `json:"kind"`
	URL  string      `json:"url"`
}

// MediaJobEvent is a bounded lifecycle projection safe for SSE/UI consumers.
// It intentionally contains no temporary paths, command argv, or credentials.
type MediaJobEvent struct {
	Command  CommandName `json:"command"`
	Stage    string      `json:"stage"`
	Status   string      `json:"status"`
	Message  string      `json:"message,omitempty"`
	Progress int         `json:"progress,omitempty"`
}

type mediaProgressEmitterKey struct{}

func withMediaProgressEmitter(ctx context.Context, emit func(MediaJobEvent)) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, mediaProgressEmitterKey{}, emit)
}

func emitMediaProgressEvent(ctx context.Context, event MediaJobEvent) {
	if ctx == nil {
		return
	}
	if emit, ok := ctx.Value(mediaProgressEmitterKey{}).(func(MediaJobEvent)); ok && emit != nil {
		emit(event)
	}
}

type MediaHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type MediaCommandRunner interface {
	Run(context.Context, string, string, ...string) ([]byte, error)
}

type execMediaCommandRunner struct{}

type boundedMediaOutput struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	exceeded bool
}

func (output *boundedMediaOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := MaxMediaCommandOutputBytes - output.buf.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = output.buf.Write(data[:remaining])
			output.exceeded = true
			return len(data), nil
		}
		_, _ = output.buf.Write(data)
		return len(data), nil
	}
	output.exceeded = true
	return len(data), nil
}

func (output *boundedMediaOutput) Bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.buf.Bytes()...)
}

func (output *boundedMediaOutput) Exceeded() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.exceeded
}

func (execMediaCommandRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("required media tool %q is unavailable: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	var output boundedMediaOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	data := output.Bytes()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if output.Exceeded() {
		return data, fmt.Errorf("media tool %q output exceeds %d byte limit", name, MaxMediaCommandOutputBytes)
	}
	if err != nil {
		if excerpt := mediaCommandErrorExcerpt(data); excerpt != "" {
			return data, fmt.Errorf("media tool %q failed: %w: %s", name, err, excerpt)
		}
		return data, fmt.Errorf("media tool %q failed: %w", name, err)
	}
	return data, nil
}

var (
	mediaCommandURLPattern    = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s]+`)
	mediaCommandPathPattern   = regexp.MustCompile(`(?i)\b(?:path|file|filename)=/[^\s]+`)
	mediaCommandSecretPattern = regexp.MustCompile(`(?i)\b(token|password|passwd|secret|api[_-]?key|authorization|cookie)=([^\s]+)`)
)

// mediaCommandErrorExcerpt is intentionally presentation-only: command
// output can contain URLs, temporary paths, or credential-like query values.
// Keep the useful tool reason while bounding and redacting that output.
func mediaCommandErrorExcerpt(data []byte) string {
	text := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == 0 || r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(data)))
	if text == "" {
		return ""
	}
	text = mediaCommandSecretPattern.ReplaceAllString(text, "$1=[redacted]")
	text = mediaCommandPathPattern.ReplaceAllString(text, "path=[path omitted]")
	text = mediaCommandURLPattern.ReplaceAllString(text, "[url omitted]")
	fields := strings.Fields(text)
	for index, field := range fields {
		if strings.HasPrefix(field, "/") || strings.HasPrefix(field, "~/") {
			fields[index] = "[path omitted]"
		}
	}
	text = strings.Join(fields, " ")
	if len(text) > MaxMediaErrorExcerptBytes {
		text = text[:MaxMediaErrorExcerptBytes] + "..."
	}
	return text
}

type MediaURLValidator func(string) (*url.URL, error)

// MediaService owns explicit, bounded public-media retrieval. It is separate
// from model-facing web research: no natural-language search is implied.
type MediaService struct {
	Artifacts   *ArtifactStore
	HTTPClient  MediaHTTPClient
	CommandRun  MediaCommandRunner
	ValidateURL MediaURLValidator
	Timeout     time.Duration
	semaphore   chan struct{}
	once        sync.Once
}

func NewMediaService(artifacts *ArtifactStore) *MediaService {
	return &MediaService{Artifacts: artifacts, HTTPClient: &http.Client{Timeout: 30 * time.Second}, CommandRun: execMediaCommandRunner{}, ValidateURL: validatePublicMediaURL, Timeout: defaultMediaServiceTimeout, semaphore: make(chan struct{}, 1)}
}

func (service *MediaService) init() {
	service.once.Do(func() {
		if service.HTTPClient == nil {
			service.HTTPClient = &http.Client{Timeout: 30 * time.Second}
		}
		if service.CommandRun == nil {
			service.CommandRun = execMediaCommandRunner{}
		}
		if service.ValidateURL == nil {
			service.ValidateURL = validatePublicMediaURL
		}
		if service.Timeout <= 0 {
			service.Timeout = defaultMediaServiceTimeout
		}
		if service.semaphore == nil {
			service.semaphore = make(chan struct{}, 1)
		}
	})
}

func validatePublicMediaURL(raw string) (*url.URL, error) {
	if len(raw) > MaxMediaURLBytes {
		return nil, errors.New("media URL is too large")
	}
	return validateWebURL(strings.TrimSpace(raw), false)
}

func (service *MediaService) Execute(ctx context.Context, sessionID SessionID, runID string, command MediaCommand, progress func(MediaJobEvent)) ([]Artifact, error) {
	if service == nil || service.Artifacts == nil {
		return nil, errors.New("media service is not configured")
	}
	if ctx == nil {
		return nil, errors.New("media context is nil")
	}
	service.init()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if command.Kind != mediaCommandImage && command.Kind != mediaCommandVideo {
		return nil, errors.New("unsupported media command")
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		return nil, errors.New("media commands require a durable session")
	}
	if _, err := service.Artifacts.store.Load(sessionID); err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(MediaJobEvent) {}
	}
	progress(MediaJobEvent{Command: command.Kind, Stage: "queued", Status: "running", Message: "Media job queued."})
	select {
	case service.semaphore <- struct{}{}:
		defer func() { <-service.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	progress(MediaJobEvent{Command: command.Kind, Stage: "validating", Status: "running", Message: "Validating public URL."})
	validated, err := service.ValidateURL(command.URL)
	if err != nil {
		return nil, err
	}
	if validated == nil {
		return nil, errors.New("media URL validation returned no URL")
	}
	jobCtx, cancel := context.WithTimeout(ctx, service.Timeout)
	defer cancel()
	if command.Kind == mediaCommandImage {
		progress(MediaJobEvent{Command: command.Kind, Stage: "fetching", Status: "running", Message: "Fetching image."})
		artifact, err := service.fetchImage(jobCtx, sessionID, runID, validated)
		if err != nil {
			return nil, err
		}
		progress(MediaJobEvent{Command: command.Kind, Stage: "complete", Status: "completed", Message: "Image ready.", Progress: 100})
		return []Artifact{artifact}, nil
	}
	progress(MediaJobEvent{Command: command.Kind, Stage: "downloading", Status: "running", Message: "Downloading one public video item."})
	artifacts, err := service.fetchVideo(jobCtx, sessionID, runID, validated, progress)
	if err != nil {
		return nil, err
	}
	progress(MediaJobEvent{Command: command.Kind, Stage: "complete", Status: "completed", Message: "Video ready.", Progress: 100})
	return artifacts, nil
}

func (service *MediaService) fetchImage(ctx context.Context, sessionID SessionID, runID string, target *url.URL) (Artifact, error) {
	var client MediaHTTPClient = service.HTTPClient
	if configured, ok := service.HTTPClient.(*http.Client); ok {
		copy := *configured
		copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxMediaRedirects {
				return errors.New("media redirect limit exceeded")
			}
			if _, err := service.ValidateURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		}
		client = &copy
	}
	if client == nil {
		return Artifact{}, errors.New("media HTTP client is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Artifact{}, err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "Bee/1.0 (explicit public media)")
	resp, err := client.Do(req)
	if err != nil {
		return Artifact{}, fmt.Errorf("image request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Artifact{}, fmt.Errorf("image request returned HTTP %s", resp.Status)
	}
	if resp.ContentLength > MaxMediaImageBytes {
		return Artifact{}, fmt.Errorf("image exceeds %d byte limit", MaxMediaImageBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxMediaImageBytes+1))
	if err != nil {
		return Artifact{}, err
	}
	if int64(len(data)) > MaxMediaImageBytes {
		return Artifact{}, fmt.Errorf("image exceeds %d byte limit", MaxMediaImageBytes)
	}
	mediaType, err := validateImagePayload(resp.Header.Get("Content-Type"), data)
	if err != nil {
		return Artifact{}, err
	}
	filename := mediaFilename(target, mediaType, "image")
	return service.Artifacts.Create(ctx, sessionID, runID, ArtifactInput{Filename: filename, MediaType: mediaType, Content: data})
}

func validateImagePayload(header string, data []byte) (string, error) {
	provided := strings.TrimSpace(header)
	if parsed, _, err := mime.ParseMediaType(provided); err == nil {
		provided = parsed
	}
	sniffed := http.DetectContentType(data)
	if provided == "" || provided == "application/octet-stream" {
		provided = sniffed
	}
	if !strings.HasPrefix(strings.ToLower(provided), "image/") {
		return "", fmt.Errorf("URL did not return an image content type")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("decode image header: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxMediaImageDimension || config.Height > MaxMediaImageDimension || int64(config.Width)*int64(config.Height) > MaxMediaImagePixels {
		return "", errors.New("image dimensions exceed safety bounds")
	}
	return strings.ToLower(provided), nil
}

func mediaFilename(target *url.URL, mediaType, fallback string) string {
	name := ""
	if target != nil {
		name = filepath.Base(target.Path)
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}
	name = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(name, "\\", "_"), "/", "_"))
	if name == "" || name == "." || name == ".." {
		name = fallback
	}
	if filepath.Ext(name) == "" {
		ext := ".bin"
		switch mediaType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/png":
			ext = ".png"
		case "image/gif":
			ext = ".gif"
		case "video/mp4":
			ext = ".mp4"
		}
		name += ext
	}
	return name
}

func (service *MediaService) fetchVideo(ctx context.Context, sessionID SessionID, runID string, target *url.URL, progress func(MediaJobEvent)) ([]Artifact, error) {
	workdir, err := os.MkdirTemp("", "bee-video-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workdir)
	if err := os.Chmod(workdir, 0o700); err != nil {
		return nil, err
	}
	sourceTemplate := filepath.Join(workdir, "source.%(ext)s")
	_, err = service.CommandRun.Run(ctx, workdir, "yt-dlp", "--no-playlist", "--no-progress", "--no-part", "--restrict-filenames", "-f", boundedVideoFormatSelector(), "--max-filesize", strconv.FormatInt(MaxMediaVideoBytes, 10), "--socket-timeout", "15", "--retries", "1", "--concurrent-fragments", "1", "-o", sourceTemplate, target.String())
	if err != nil {
		return nil, err
	}
	if err := validateMediaWorkdir(workdir); err != nil {
		return nil, err
	}
	source, err := findVideoInputs(workdir)
	if err != nil {
		return nil, err
	}
	output := filepath.Join(workdir, "video.mp4")
	progress(MediaJobEvent{Command: mediaCommandVideo, Stage: "probing", Status: "running", Message: "Checking source video duration."})
	probe, err := service.CommandRun.Run(ctx, workdir, "ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_type", "-of", "json", source)
	if err != nil {
		return nil, err
	}
	duration, err := parseVideoDuration(probe)
	if err != nil {
		return nil, err
	}
	progress(MediaJobEvent{Command: mediaCommandVideo, Stage: "transcoding", Status: "running", Message: "Preparing browser-compatible video."})
	if _, err := service.CommandRun.Run(ctx, workdir, "ffmpeg", browserVideoTranscodeArgs(source, output, duration)...); err != nil {
		return nil, err
	}
	if err := validateRegularMediaFile(workdir, output); err != nil {
		return nil, err
	}
	if info, err := os.Stat(output); err != nil || info.Size() <= 0 || info.Size() > MaxMediaVideoBytes {
		return nil, fmt.Errorf("video output exceeds %d byte limit", MaxMediaVideoBytes)
	}
	if err := os.Chmod(output, 0o600); err != nil {
		return nil, err
	}
	progress(MediaJobEvent{Command: mediaCommandVideo, Stage: "probing", Status: "running", Message: "Checking video metadata."})
	probe, err = service.CommandRun.Run(ctx, workdir, "ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_type", "-of", "json", output)
	if err != nil {
		return nil, err
	}
	if _, err := parseVideoDuration(probe); err != nil {
		return nil, err
	}
	if err := validateMediaWorkdir(workdir); err != nil {
		return nil, err
	}
	artifacts := make([]Artifact, 0, 1)
	videoArtifact, err := service.Artifacts.CreateFromFile(ctx, sessionID, runID, ArtifactFileInput{Path: output, Filename: "video.mp4", MediaType: "video/mp4", MaxBytes: MaxMediaVideoBytes})
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, videoArtifact)
	return artifacts, nil
}

func boundedVideoFormatSelector() string {
	limit := strconv.FormatInt(MaxMediaVideoBytes, 10)
	// Keep each advertised stream bounded by the media-specific video cap. The
	// transcode and final artifact checks apply the same cap after merging.
	return fmt.Sprintf("bv*[height<=240][filesize_approx<=%s]+ba[abr<=64][filesize_approx<=%s]/bv*[height<=360][filesize_approx<=%s]+ba[abr<=64][filesize_approx<=%s]/b[height<=240][filesize_approx<=%s]/b[height<=360][filesize_approx<=%s]", limit, limit, limit, limit, limit, limit)
}

func validateMediaWorkdir(workdir string) error {
	root, err := os.Lstat(workdir)
	if err != nil {
		return err
	}
	if !root.IsDir() || root.Mode()&os.ModeSymlink != 0 || root.Mode().Perm()&0o077 != 0 {
		return errors.New("media workdir is not a private real directory")
	}
	var total int64
	err = filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("media workdir contains a symlink")
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if info.Size() > MaxMediaVideoBytes {
				return fmt.Errorf("media file exceeds %d byte limit", MaxMediaVideoBytes)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if total > MaxMediaDiskBytes {
		return fmt.Errorf("media workdir exceeds %d byte disk limit", MaxMediaDiskBytes)
	}
	return nil
}

func validateRegularMediaFile(workdir, path string) error {
	cleanRoot, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("media output escaped private workdir")
	}
	info, err := os.Lstat(cleanPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("media output is not a safe regular file")
	}
	return nil
}

func findVideoInputs(workdir string) (string, error) {
	entries, err := os.ReadDir(workdir)
	if err != nil {
		return "", err
	}
	var source string
	for _, entry := range entries {
		path := filepath.Join(workdir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if strings.HasPrefix(entry.Name(), "source.") && !strings.Contains(entry.Name(), ".part") && !isVideoThumbnailName(entry.Name()) {
			source = path
		}
	}
	if source == "" {
		return "", errors.New("yt-dlp produced no regular video file")
	}
	return source, nil
}

func isVideoThumbnailName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func readBoundedMediaFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxMediaImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxMediaImageBytes {
		return nil, fmt.Errorf("media file exceeds %d byte limit", MaxMediaImageBytes)
	}
	return data, nil
}

func validateVideoDuration(data []byte) error {
	_, err := parseVideoDuration(data)
	return err
}

func parseVideoDuration(data []byte) (float64, error) {
	var payload struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("decode video metadata: %w", err)
	}
	hasVideo := false
	for _, stream := range payload.Streams {
		if stream.CodecType == "video" {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		return 0, errors.New("media file does not contain a video stream")
	}
	if strings.TrimSpace(payload.Format.Duration) == "" {
		return 0, errors.New("video duration metadata is missing")
	}
	duration, err := strconv.ParseFloat(payload.Format.Duration, 64)
	if err != nil || duration <= 0 || duration > MaxMediaDuration.Seconds() {
		return 0, fmt.Errorf("video duration exceeds %s limit", MaxMediaDuration)
	}
	return duration, nil
}

func browserVideoTranscodeArgs(source, output string, duration float64) []string {
	// Leave headroom for MP4/container overhead and metadata while budgeting
	// against the dedicated one-gigabyte video artifact cap.
	budgetBitsPerSecond := (float64(MaxMediaVideoBytes) * 8 * 0.80) / duration
	audioBitsPerSecond := 64000.0
	if budgetBitsPerSecond < 160000 {
		audioBitsPerSecond = 32000
	}
	videoBitsPerSecond := budgetBitsPerSecond - audioBitsPerSecond
	if videoBitsPerSecond < 64000 {
		videoBitsPerSecond = 64000
	}
	videoRate := fmt.Sprintf("%dk", int(videoBitsPerSecond/1000))
	audioRate := fmt.Sprintf("%dk", int(audioBitsPerSecond/1000))
	return []string{
		"-hide_banner", "-loglevel", "error", "-y", "-i", source,
		"-map", "0:v:0?", "-map", "0:a:0?", "-vf", "scale=w='min(640,iw)':h=-2",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-b:v", videoRate, "-maxrate", videoRate, "-bufsize", videoRate,
		"-c:a", "aac", "-b:a", audioRate, "-movflags", "+faststart", output,
	}
}
