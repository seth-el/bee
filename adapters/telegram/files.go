package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// File is the bounded provider-owned result of getFile. FilePath is only used
// by the adapter to construct a subsequent download request.
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

type getFileResponse struct {
	OK          bool   `json:"ok"`
	Result      File   `json:"result"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

func (file File) Validate() error {
	if err := validateTelegramFileID(file.FileID); err != nil {
		return err
	}
	if file.FileUniqueID != "" && (len(file.FileUniqueID) > MaxFileIDBytes || !utf8.ValidString(file.FileUniqueID) || strings.TrimSpace(file.FileUniqueID) != file.FileUniqueID || containsUnsafeVoiceToken(file.FileUniqueID)) {
		return errors.New("telegram file_unique_id is invalid or too large")
	}
	if file.FileSize < 0 || file.FileSize > MaxFileBytes {
		return fmt.Errorf("telegram file size must be between 0 and %d bytes", MaxFileBytes)
	}
	if file.FilePath == "" {
		return errors.New("telegram file_path is required")
	}
	if err := validateTelegramFilePath(file.FilePath); err != nil {
		return err
	}
	return nil
}

func validateTelegramFileID(fileID string) error {
	if strings.TrimSpace(fileID) == "" || len(fileID) > MaxFileIDBytes || !utf8.ValidString(fileID) || strings.TrimSpace(fileID) != fileID || containsUnsafeVoiceToken(fileID) {
		return errors.New("telegram file_id is invalid or too large")
	}
	return nil
}

func validateTelegramFilePath(filePath string) error {
	if strings.TrimSpace(filePath) == "" || len(filePath) > MaxFilePathBytes || !utf8.ValidString(filePath) || strings.IndexByte(filePath, 0) >= 0 || strings.ContainsRune(filePath, '\\') || strings.HasPrefix(filePath, "/") {
		return errors.New("telegram file_path is invalid or too large")
	}
	for _, component := range strings.Split(filePath, "/") {
		if component == "" || component == "." || component == ".." || telegramContainsControl(component) {
			return errors.New("telegram file_path is invalid")
		}
		for _, char := range component {
			if !(char >= 'A' && char <= 'Z') && !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' && char != '.' {
				return errors.New("telegram file_path contains unsafe characters")
			}
		}
	}
	return nil
}

func telegramContainsControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

// GetFile resolves a bounded Telegram file identifier without exposing API
// descriptions, URLs, or bot credentials through returned errors.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	if c == nil || c.HTTPClient == nil {
		return File{}, errors.New("telegram client is not configured")
	}
	if ctx == nil {
		return File{}, errors.New("telegram request context is nil")
	}
	if err := validateTelegramFileID(fileID); err != nil {
		return File{}, err
	}
	var response getFileResponse
	if err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &response); err != nil {
		return File{}, err
	}
	if err := response.Result.Validate(); err != nil {
		return File{}, fmt.Errorf("decode telegram getFile result: %w", err)
	}
	return response.Result, nil
}

// DownloadFile streams a Telegram file into caller-owned storage. The client
// never chooses a filesystem path; callers must provide a writer rooted in
// their own private artifact directory. At most maxBytes are written.
func (c *Client) DownloadFile(ctx context.Context, filePath string, destination io.Writer, maxBytes int64) (int64, error) {
	if c == nil || c.HTTPClient == nil {
		return 0, errors.New("telegram client is not configured")
	}
	if ctx == nil {
		return 0, errors.New("telegram request context is nil")
	}
	if destination == nil {
		return 0, errors.New("telegram file destination is required")
	}
	if err := validateTelegramFilePath(filePath); err != nil {
		return 0, err
	}
	if maxBytes <= 0 || maxBytes > MaxFileBytes {
		return 0, errors.New("telegram file download limit is invalid")
	}
	requestURL := c.BaseURL + "/file/bot" + c.Token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, errors.New("create telegram file request")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, transportFailure(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := readBounded(resp.Body, c.responseLimit())
		if readErr != nil {
			return 0, readErr
		}
		return 0, decodeAPIError(resp.StatusCode, data)
	}
	if resp.ContentLength > maxBytes {
		return 0, errors.New("telegram file exceeds bounded size")
	}
	tracked := &writeTracker{w: destination}
	written, copyErr := io.Copy(tracked, io.LimitReader(resp.Body, maxBytes+1))
	if copyErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, ctxErr
		}
		if tracked.failed {
			return written, errors.New("write telegram file")
		}
		// The connection broke mid-download.
		return written, transportFailure(copyErr)
	}
	if written > maxBytes {
		return written, errors.New("telegram file exceeds bounded size")
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	return written, nil
}

// Telegram's file service sometimes answers 504 or drops connections for a
// minute or two (seen 2026-11-18), so downloads retry temporary failures.
var (
	downloadAttempts       = 3
	downloadAttemptTimeout = 30 * time.Second
	downloadRetryDelays    = []time.Duration{2 * time.Second, 5 * time.Second}
	downloadRetrySleep     = sleepContext
)

// resettableWriter is a destination that can be emptied for a retry, such as
// an *os.File.
type resettableWriter interface {
	io.Seeker
	Truncate(int64) error
}

func retryableDownload(err error) bool {
	var transport *TransportError
	var apiErr *APIError
	return errors.As(err, &transport) || (errors.As(err, &apiErr) && apiErr.Temporary())
}

// DownloadFileByID resolves fileID with getFile and then streams the file.
// Temporary failures (Telegram 5xx or 429, dropped or refused connections,
// an attempt over 30 s) are retried up to three attempts in all, when the
// destination can be reset (an *os.File) or nothing was written yet.
func (c *Client) DownloadFileByID(ctx context.Context, fileID string, destination io.Writer, maxBytes int64) (File, int64, error) {
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, downloadAttemptTimeout)
		file, written, err := c.downloadFileByIDOnce(attemptCtx, fileID, destination, maxBytes)
		cancel()
		if err == nil || ctx.Err() != nil {
			return file, written, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = &TransportError{Reason: "timeout"}
		}
		if attempt >= downloadAttempts || !retryableDownload(err) {
			return file, written, err
		}
		if written > 0 {
			reset, ok := destination.(resettableWriter)
			if !ok {
				return file, written, err
			}
			if _, seekErr := reset.Seek(0, io.SeekStart); seekErr != nil {
				return file, written, err
			}
			if truncErr := reset.Truncate(0); truncErr != nil {
				return file, written, err
			}
		}
		if sleepErr := downloadRetrySleep(ctx, downloadRetryDelays[min(attempt-1, len(downloadRetryDelays)-1)]); sleepErr != nil {
			return File{}, 0, sleepErr
		}
	}
}

// writeTracker tells a failed write (local) from a failed read (network).
type writeTracker struct {
	w      io.Writer
	failed bool
}

func (t *writeTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.failed = true
	}
	return n, err
}

func (c *Client) downloadFileByIDOnce(ctx context.Context, fileID string, destination io.Writer, maxBytes int64) (File, int64, error) {
	file, err := c.GetFile(ctx, fileID)
	if err != nil {
		return File{}, 0, err
	}
	if file.FileSize > maxBytes && maxBytes > 0 {
		return File{}, 0, errors.New("telegram file exceeds bounded size")
	}
	written, err := c.DownloadFile(ctx, file.FilePath, destination, maxBytes)
	if err != nil {
		return File{}, written, err
	}
	if file.FileSize > 0 && written != file.FileSize {
		return File{}, written, errors.New("telegram downloaded file size does not match metadata")
	}
	return file, written, nil
}
