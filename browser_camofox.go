package bee

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultCamofoxURL   = "http://127.0.0.1:9377"
	maxCamofoxBodyBytes = 1 << 20
)

type CamofoxError struct {
	Status int
	Class  string
}

func (e *CamofoxError) Error() string {
	if e == nil {
		return "camofox error"
	}
	if e.Status != 0 {
		return fmt.Sprintf("camofox %s (HTTP %d)", e.Class, e.Status)
	}
	return "camofox " + e.Class
}

type BrowserCamofoxClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
	AccessKey  string
}

var _ BrowserBackend = (*BrowserCamofoxClient)(nil)
var _ BrowserCloser = (*BrowserCamofoxClient)(nil)
var _ BrowserWaiter = (*BrowserCamofoxClient)(nil)
var _ BrowserScroller = (*BrowserCamofoxClient)(nil)

func NewBrowserCamofoxClient(baseURL string, client *http.Client, timeout time.Duration) (*BrowserCamofoxClient, error) {
	return NewBrowserCamofoxClientWithAccessKeyFile(baseURL, client, timeout, "")
}

func NewBrowserCamofoxClientWithAccessKeyFile(baseURL string, client *http.Client, timeout time.Duration, accessKeyFile string) (*BrowserCamofoxClient, error) {
	base, err := validateCamofoxControlURL(baseURL)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	if strings.TrimSpace(accessKeyFile) == "" && !isLoopbackHost(base.Hostname()) {
		return nil, errors.New("camofox endpoint must be loopback when access key is not configured")
	}
	key := ""
	if strings.TrimSpace(accessKeyFile) != "" {
		if err := validatePrivateSecretFile(accessKeyFile); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(accessKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read camofox access key: %w", err)
		}
		key = strings.TrimSpace(string(data))
		if key == "" {
			return nil, errors.New("camofox access key file is empty")
		}
		if !isLoopbackHost(base.Hostname()) && base.Scheme != "https" {
			return nil, errors.New("camofox access key requires an HTTPS endpoint off loopback")
		}
	}
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &BrowserCamofoxClient{BaseURL: strings.TrimRight(base.String(), "/"), HTTPClient: client, Timeout: timeout, AccessKey: key}, nil
}

func browserIdentity(userID string) string {
	if strings.TrimSpace(userID) == "" {
		userID = "sessionless"
	}
	sum := sha256.Sum256([]byte("bee-camofox:" + userID))
	return "bee-" + hex.EncodeToString(sum[:])[:24]
}

func (c *BrowserCamofoxClient) request(ctx context.Context, method, endpoint string, query url.Values, body any, out any) error {
	if c == nil {
		return errors.New("browser backend is not configured")
	}
	base, err := validateCamofoxControlURL(c.BaseURL)
	if err != nil {
		return fmt.Errorf("camofox endpoint: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + endpoint
	base.Fragment = ""
	values := base.Query()
	for key := range query {
		values.Del(key)
		for _, value := range query[key] {
			values.Add(key, value)
		}
	}
	base.RawQuery = values.Encode()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode camofox request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, base.String(), payload)
	if err != nil {
		return fmt.Errorf("create camofox request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.AccessKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessKey)
	}
	baseClient := c.HTTPClient
	if baseClient == nil {
		baseClient = &http.Client{Timeout: timeout}
	}
	client := *baseClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("camofox request failed: %w", err)
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxCamofoxBodyBytes+1))
	if readErr != nil {
		return fmt.Errorf("read camofox response: %w", readErr)
	}
	if len(data) > maxCamofoxBodyBytes {
		return errors.New("camofox response exceeds size bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &CamofoxError{Status: response.StatusCode, Class: "http status"}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &CamofoxError{Class: "invalid response"}
	}
	return nil
}

type camofoxTabResponse struct {
	TargetID string `json:"targetId"`
	TabID    string `json:"tabId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
}
type camofoxSnapshotResponse struct {
	TargetID   string
	URL        string
	Title      string
	Format     string
	Text       string
	Offset     int
	HasMore    bool
	NextOffset *int
	TotalChars int
	Truncated  bool
}

func (r *camofoxSnapshotResponse) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	readString := func(names ...string) string {
		for _, name := range names {
			if raw, ok := fields[name]; ok {
				var value string
				if json.Unmarshal(raw, &value) == nil {
					return value
				}
			}
		}
		return ""
	}
	r.TargetID = readString("targetId", "target_id")
	r.URL = readString("url", "finalUrl", "final_url")
	r.Title = readString("title")
	r.Format = readString("format")
	r.Text = readString("snapshot", "text", "content")
	if raw, ok := firstRaw(fields, "offset"); ok {
		_ = json.Unmarshal(raw, &r.Offset)
	}
	if raw, ok := firstRaw(fields, "hasMore", "has_more"); ok {
		_ = json.Unmarshal(raw, &r.HasMore)
	}
	if raw, ok := firstRaw(fields, "nextOffset", "next_offset"); ok && string(raw) != "null" {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			r.NextOffset = &v
		}
	}
	if raw, ok := firstRaw(fields, "totalChars", "total_chars"); ok {
		_ = json.Unmarshal(raw, &r.TotalChars)
	}
	if raw, ok := fields["truncated"]; ok {
		_ = json.Unmarshal(raw, &r.Truncated)
	}
	return nil
}
func firstRaw(fields map[string]json.RawMessage, names ...string) (json.RawMessage, bool) {
	for _, name := range names {
		if raw, ok := fields[name]; ok {
			return raw, true
		}
	}
	return nil, false
}

func camofoxQuery(userID, targetID string) url.Values {
	query := url.Values{"userId": []string{browserIdentity(userID)}}
	if targetID != "" {
		query.Set("targetId", targetID)
	}
	return query
}
func (c *BrowserCamofoxClient) Tabs(ctx context.Context) (BrowserTabsResult, error) {
	return c.TabsForSession(ctx, "")
}
func (c *BrowserCamofoxClient) TabsForSession(ctx context.Context, userID string) (BrowserTabsResult, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/tabs", url.Values{"userId": []string{browserIdentity(userID)}}, nil, &raw); err != nil {
		return BrowserTabsResult{}, err
	}
	var response struct {
		Running bool              `json:"running"`
		Tabs    []json.RawMessage `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return BrowserTabsResult{}, fmt.Errorf("decode camofox tabs: %w", err)
	}
	return BrowserTabsResult{Running: response.Running, Count: len(response.Tabs)}, nil
}
func (c *BrowserCamofoxClient) Open(ctx context.Context, request BrowserOpenRequest) (BrowserOpenResult, error) {
	user := browserIdentity(request.UserID)
	sessionKey := browserIdentity(request.SessionKey)
	body := map[string]any{"userId": user, "sessionKey": sessionKey, "url": request.URL}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/tabs", nil, body, &raw); err != nil {
		return BrowserOpenResult{}, err
	}
	var response camofoxTabResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return BrowserOpenResult{}, err
	}
	if response.TargetID == "" {
		response.TargetID = response.TabID
	}
	return BrowserOpenResult{TargetID: response.TargetID, URL: response.URL, Title: response.Title}, nil
}
func (c *BrowserCamofoxClient) Navigate(ctx context.Context, request BrowserNavigateRequest) (BrowserNavigateResult, error) {
	body := map[string]any{"userId": browserIdentity(request.UserID), "targetId": request.TargetID, "url": request.URL}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/navigate", nil, body, &raw); err != nil {
		return BrowserNavigateResult{}, err
	}
	var response camofoxTabResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return BrowserNavigateResult{}, err
	}
	if response.TargetID == "" {
		response.TargetID = request.TargetID
	}
	return BrowserNavigateResult{TargetID: response.TargetID, URL: response.URL, Title: response.Title}, nil
}
func (c *BrowserCamofoxClient) Snapshot(ctx context.Context, request BrowserSnapshotRequest) (BrowserSnapshotResult, error) {
	var raw json.RawMessage
	query := camofoxQuery(request.UserID, request.TargetID)
	if request.Format != "" {
		query.Set("format", request.Format)
	}
	if request.Offset > 0 {
		query.Set("offset", fmt.Sprintf("%d", request.Offset))
	}
	if err := c.request(ctx, http.MethodGet, "/snapshot", query, nil, &raw); err != nil {
		return BrowserSnapshotResult{}, err
	}
	var response camofoxSnapshotResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return BrowserSnapshotResult{}, err
	}
	if response.TargetID == "" {
		response.TargetID = request.TargetID
	}
	return BrowserSnapshotResult{TargetID: response.TargetID, URL: response.URL, Title: response.Title, Format: response.Format, Text: response.Text, Offset: response.Offset, HasMore: response.HasMore, NextOffset: response.NextOffset, TotalChars: response.TotalChars, Truncated: response.Truncated}, nil
}
func (c *BrowserCamofoxClient) Act(ctx context.Context, request BrowserActRequest) (BrowserActResult, error) {
	if request.Kind != "click" && request.Kind != "type" && request.Kind != "press" && request.Kind != "hover" {
		return BrowserActResult{}, fmt.Errorf("unsupported browser action %q", request.Kind)
	}
	if request.Kind == "hover" {
		body := map[string]any{"userId": browserIdentity(request.UserID), "targetId": request.TargetID, "kind": "hover", "ref": request.Ref}
		var raw json.RawMessage
		if err := c.request(ctx, http.MethodPost, "/act", nil, body, &raw); err != nil {
			return BrowserActResult{}, err
		}
		return BrowserActResult{}, nil
	}
	body := map[string]any{"userId": browserIdentity(request.UserID)}
	switch request.Kind {
	case "click", "hover":
		body["ref"] = request.Ref
	case "type":
		body["ref"] = request.Ref
		body["text"] = request.Text
	case "press":
		body["key"] = request.Key
	}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/tabs/"+url.PathEscape(request.TargetID)+"/"+request.Kind, nil, body, &raw); err != nil {
		return BrowserActResult{}, err
	}
	return BrowserActResult{}, nil
}
func (c *BrowserCamofoxClient) Close(ctx context.Context, request BrowserCloseRequest) error {
	return c.request(ctx, http.MethodDelete, "/tabs/"+url.PathEscape(request.TargetID), url.Values{"userId": []string{browserIdentity(request.UserID)}}, nil, nil)
}
func (c *BrowserCamofoxClient) Wait(ctx context.Context, request BrowserWaitRequest) (BrowserSnapshotResult, error) {
	body := map[string]any{"userId": browserIdentity(request.UserID), "timeout": request.Milliseconds}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/tabs/"+url.PathEscape(request.TargetID)+"/wait", nil, body, &raw); err != nil {
		return BrowserSnapshotResult{}, err
	}
	return BrowserSnapshotResult{TargetID: request.TargetID}, nil
}
func (c *BrowserCamofoxClient) Scroll(ctx context.Context, request BrowserScrollRequest) (BrowserSnapshotResult, error) {
	body := map[string]any{"userId": browserIdentity(request.UserID), "direction": request.Direction, "amount": request.Amount}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/tabs/"+url.PathEscape(request.TargetID)+"/scroll", nil, body, &raw); err != nil {
		return BrowserSnapshotResult{}, err
	}
	return BrowserSnapshotResult{TargetID: request.TargetID}, nil
}
