package telegram

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const (
	MaxRequestBytes  = 16 << 10
	MaxResponseBytes = 1 << 20
	MaxFileIDBytes   = MaxTelegramVoiceFileIDBytes
	MaxFilePathBytes = 1024
	MaxFileBytes     = MaxTelegramVoiceFileSize
)

type APIError struct {
	HTTPStatus int
	Code       int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return "telegram API error"
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("telegram API request failed with HTTP status %d", e.HTTPStatus)
	}
	if e.Code != 0 {
		return fmt.Sprintf("telegram API returned error code %d", e.Code)
	}
	return "telegram API returned an error"
}

func (e *APIError) Temporary() bool {
	return e != nil && (e.HTTPStatus == http.StatusTooManyRequests || e.HTTPStatus >= 500 || e.Code == 429)
}

// NewHTTPClient returns the HTTP client the Telegram adapter uses. It speaks
// HTTP/1.1 only. Over HTTP/2 every request shares one connection and a
// timed-out request does not close it, so when the network path silently
// dies, every later request queues on the dead connection until the kernel
// gives up on it, about 15 minutes later (seen in production 2026-11-17). Over
// HTTP/1.1 a timed-out request closes its own connection and the retry dials
// a fresh one.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.IdleConnTimeout = 30 * time.Second
	return &http.Client{Transport: transport}
}

// TransportError is a request that got no HTTP response at all: a timeout, a
// refused or reset connection, a DNS failure. Reason is one fixed word or
// phrase, never the underlying error text, because that text includes the
// request URL and therefore the bot token.
type TransportError struct {
	Reason string
}

func (e *TransportError) Error() string {
	if e == nil || e.Reason == "" {
		return "telegram HTTP request failed"
	}
	return "telegram HTTP request failed (" + e.Reason + ")"
}

func transportFailure(err error) *TransportError {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &TransportError{Reason: "timeout"}
	case errors.As(err, &dnsErr):
		return &TransportError{Reason: "dns"}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &TransportError{Reason: "connection refused"}
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return &TransportError{Reason: "connection reset"}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &TransportError{Reason: "connection closed"}
	case errors.As(err, &netErr) && netErr.Timeout():
		return &TransportError{Reason: "timeout"}
	default:
		return &TransportError{Reason: "network"}
	}
}

type Client struct {
	BaseURL          string
	Token            string
	HTTPClient       *http.Client
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxUploadBytes   int64
}

func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("telegram client base URL and token are required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram client base URL must be an absolute http(s) URL")
	}
	if len(token) > MaxTokenBytes || strings.ContainsAny(token, "\r\n \t/?#%") {
		return nil, errors.New("telegram client token is invalid")
	}
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTPClient: httpClient, MaxRequestBytes: MaxRequestBytes, MaxResponseBytes: MaxResponseBytes, MaxUploadBytes: MaxFileBytes}, nil
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

type Message struct {
	MessageID       int         `json:"message_id"`
	From            *User       `json:"from,omitempty"`
	Chat            Chat        `json:"chat"`
	Date            int64       `json:"date"`
	Text            string      `json:"text,omitempty"`
	Caption         string      `json:"caption,omitempty"`
	MessageThreadID int         `json:"message_thread_id,omitempty"`
	ReplyToMessage  *Message    `json:"reply_to_message,omitempty"`
	Photo           []PhotoSize `json:"photo,omitempty"`
	Audio           *Audio      `json:"audio,omitempty"`
	Document        *Document   `json:"document,omitempty"`
	Voice           *Voice      `json:"voice,omitempty"`
	Video           *struct{}   `json:"video,omitempty"`
	Sticker         *struct{}   `json:"sticker,omitempty"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Duration     int64  `json:"duration"`
	Performer    string `json:"performer,omitempty"`
	Title        string `json:"title,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Update struct {
	UpdateID          int64     `json:"update_id"`
	Message           *Message  `json:"message,omitempty"`
	EditedMessage     *Message  `json:"edited_message,omitempty"`
	ChannelPost       *Message  `json:"channel_post,omitempty"`
	EditedChannelPost *Message  `json:"edited_channel_post,omitempty"`
	CallbackQuery     *struct{} `json:"callback_query,omitempty"`
	InlineQuery       *struct{} `json:"inline_query,omitempty"`
}

type getMeResponse struct {
	OK          bool   `json:"ok"`
	Result      User   `json:"result"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

type updatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []Update `json:"result"`
	ErrorCode   int      `json:"error_code,omitempty"`
	Description string   `json:"description,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

type sendMessageResponse struct {
	OK          bool    `json:"ok"`
	Result      Message `json:"result"`
	ErrorCode   int     `json:"error_code,omitempty"`
	Description string  `json:"description,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

type SendMessageRequest struct {
	ChatID           int64  `json:"chat_id"`
	Text             string `json:"text"`
	ParseMode        string `json:"parse_mode,omitempty"`
	MessageThreadID  int64  `json:"message_thread_id,omitempty"`
	ReplyToMessageID int64  `json:"reply_to_message_id,omitempty"`
}

type SendVoiceRequest struct {
	ChatID           int64
	Voice            io.Reader
	Filename         string
	ContentType      string
	SizeBytes        int64
	MessageThreadID  int64
	ReplyToMessageID int64
}

type SendAttachmentRequest struct {
	ChatID           int64
	Attachment       io.Reader
	FieldName        string
	Filename         string
	ContentType      string
	SizeBytes        int64
	Caption          string
	MessageThreadID  int64
	ReplyToMessageID int64
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	if c == nil {
		return User{}, errors.New("telegram client is nil")
	}
	var response getMeResponse
	if err := c.call(ctx, "getMe", nil, &response); err != nil {
		return User{}, err
	}
	return response.Result, nil
}

// getUpdatesGrace is how long past the long-poll timeout a getUpdates call
// may take before it counts as a timeout.
var getUpdatesGrace = 20 * time.Second

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout, limit int) ([]Update, error) {
	if offset < 0 || offset > MaxCheckpointOffset || timeout <= 0 || timeout > MaxPollTimeoutSeconds || limit <= 0 || limit > MaxPollLimit {
		return nil, errors.New("telegram getUpdates parameters are out of bounds")
	}
	values := url.Values{}
	values.Set("offset", strconv.FormatInt(offset, 10))
	values.Set("timeout", strconv.Itoa(timeout))
	values.Set("limit", strconv.Itoa(limit))
	// Telegram holds the request open for up to timeout seconds. Bound the
	// wait beyond that, so a connection that silently stops answering is
	// treated as a timeout instead of hanging the poller forever.
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+getUpdatesGrace)
	defer cancel()
	var response updatesResponse
	if err := c.call(callCtx, "getUpdates?"+values.Encode(), nil, &response); err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, &TransportError{Reason: "timeout"}
		}
		return nil, err
	}
	return response.Result, nil
}

func (c *Client) SendMessage(ctx context.Context, request SendMessageRequest) (Message, error) {
	if request.ChatID == 0 || strings.TrimSpace(request.Text) == "" || len(request.Text) > 4096 {
		return Message{}, errors.New("telegram sendMessage request is invalid")
	}
	var response sendMessageResponse
	if err := c.call(ctx, "sendMessage", request, &response); err != nil {
		return Message{}, err
	}
	if err := validateSuccessfulSendMessage(response.Result); err != nil {
		return Message{}, err
	}
	return response.Result, nil
}

func (c *Client) SendDocument(ctx context.Context, request SendAttachmentRequest) (Message, error) {
	request.FieldName = "document"
	return c.sendAttachment(ctx, "sendDocument", request)
}

func (c *Client) SendAudio(ctx context.Context, request SendAttachmentRequest) (Message, error) {
	request.FieldName = "audio"
	return c.sendAttachment(ctx, "sendAudio", request)
}

func (c *Client) SendPhoto(ctx context.Context, request SendAttachmentRequest) (Message, error) {
	request.FieldName = "photo"
	return c.sendAttachment(ctx, "sendPhoto", request)
}

func (c *Client) sendAttachment(ctx context.Context, method string, request SendAttachmentRequest) (Message, error) {
	if c == nil || c.HTTPClient == nil || ctx == nil || request.Attachment == nil || request.SizeBytes <= 0 || request.SizeBytes > c.uploadLimit() {
		return Message{}, errors.New("telegram attachment request is invalid")
	}
	if request.FieldName != "document" && request.FieldName != "audio" && request.FieldName != "photo" {
		return Message{}, errors.New("telegram attachment field is invalid")
	}
	if request.Filename == "" || len(request.Filename) > bee.MaxChannelFilenameBytes || strings.ContainsAny(request.Filename, `/\\`) || telegramContainsControl(request.Filename) {
		return Message{}, errors.New("telegram attachment filename is invalid")
	}
	if len(request.Caption) > MaxTelegramTextBytes || !utf8.ValidString(request.Caption) {
		return Message{}, errors.New("telegram attachment caption is invalid")
	}
	data, err := readBounded(request.Attachment, c.uploadLimit())
	if err != nil || int64(len(data)) != request.SizeBytes {
		return Message{}, errors.New("telegram attachment size does not match metadata")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(request.ChatID, 10)}
	if request.MessageThreadID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(request.MessageThreadID, 10)
	}
	if request.ReplyToMessageID != 0 {
		fields["reply_to_message_id"] = strconv.FormatInt(request.ReplyToMessageID, 10)
	}
	if request.Caption != "" {
		fields["caption"] = request.Caption
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return Message{}, errors.New("encode telegram attachment request")
		}
	}
	part, err := writer.CreateFormFile(request.FieldName, request.Filename)
	if err != nil {
		return Message{}, errors.New("encode telegram attachment")
	}
	if _, err := part.Write(data); err != nil {
		return Message{}, errors.New("write telegram attachment")
	}
	if err := writer.Close(); err != nil || int64(body.Len()) > c.uploadLimit() {
		return Message{}, errors.New("telegram attachment request exceeds bounded size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/"+method, &body)
	if err != nil {
		return Message{}, errors.New("create telegram attachment request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, errors.New("telegram attachment HTTP request failed")
	}
	defer resp.Body.Close()
	responseData, err := readBounded(resp.Body, c.responseLimit())
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Message{}, decodeAPIError(resp.StatusCode, responseData)
	}
	var response sendMessageResponse
	if err := json.Unmarshal(responseData, &response); err != nil {
		return Message{}, errors.New("decode telegram attachment response")
	}
	if !response.OK {
		return Message{}, &APIError{Code: response.ErrorCode}
	}
	if err := validateSuccessfulSendMessage(response.Result); err != nil {
		return Message{}, err
	}
	return response.Result, nil
}

func (c *Client) SendVoice(ctx context.Context, request SendVoiceRequest) (Message, error) {
	if c == nil || c.HTTPClient == nil {
		return Message{}, errors.New("telegram client is not configured")
	}
	if ctx == nil {
		return Message{}, errors.New("telegram request context is nil")
	}
	if request.ChatID == 0 || request.Voice == nil || request.SizeBytes <= 0 || request.SizeBytes > c.uploadLimit() {
		return Message{}, errors.New("telegram sendVoice request is invalid")
	}
	if request.Filename == "" {
		request.Filename = "voice.ogg"
	}
	if len(request.Filename) > bee.MaxChannelFilenameBytes || strings.ContainsAny(request.Filename, `/\\`) || telegramContainsControl(request.Filename) || request.Filename == "." || request.Filename == ".." {
		return Message{}, errors.New("telegram sendVoice filename is invalid")
	}
	data, err := readBounded(request.Voice, c.uploadLimit())
	if err != nil {
		return Message{}, errors.New("read telegram voice")
	}
	if int64(len(data)) != request.SizeBytes {
		return Message{}, errors.New("telegram voice size does not match metadata")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(request.ChatID, 10)}
	if request.MessageThreadID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(request.MessageThreadID, 10)
	}
	if request.ReplyToMessageID != 0 {
		fields["reply_to_message_id"] = strconv.FormatInt(request.ReplyToMessageID, 10)
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return Message{}, errors.New("encode telegram sendVoice request")
		}
	}
	part, err := writer.CreateFormFile("voice", request.Filename)
	if err != nil {
		return Message{}, errors.New("encode telegram voice")
	}
	if _, err := part.Write(data); err != nil {
		return Message{}, errors.New("encode telegram voice")
	}
	if err := writer.Close(); err != nil || int64(body.Len()) > c.uploadLimit() {
		return Message{}, errors.New("telegram sendVoice request exceeds bounded size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/sendVoice", &body)
	if err != nil {
		return Message{}, errors.New("create telegram sendVoice request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, errors.New("telegram sendVoice HTTP request failed")
	}
	defer resp.Body.Close()
	responseData, err := readBounded(resp.Body, c.responseLimit())
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Message{}, decodeAPIError(resp.StatusCode, responseData)
	}
	var response sendMessageResponse
	if err := json.Unmarshal(responseData, &response); err != nil {
		return Message{}, errors.New("decode telegram sendVoice response")
	}
	if !response.OK {
		apiErr := &APIError{Code: response.ErrorCode}
		if response.Parameters != nil {
			apiErr.RetryAfter = time.Duration(response.Parameters.RetryAfter) * time.Second
		}
		return Message{}, apiErr
	}
	if err := validateSuccessfulSendMessage(response.Result); err != nil {
		return Message{}, err
	}
	return response.Result, nil
}

func validateSuccessfulSendMessage(message Message) error {
	if message.MessageID <= 0 {
		return errors.New("telegram successful send response is missing a valid message ID")
	}
	return nil
}

func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	if chatID == 0 || strings.TrimSpace(action) == "" || len(action) > 64 {
		return errors.New("telegram sendChatAction request is invalid")
	}
	var response struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code,omitempty"`
		Description string `json:"description,omitempty"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after,omitempty"`
		} `json:"parameters,omitempty"`
	}
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, &response)
}

func (c *Client) call(ctx context.Context, method string, requestBody any, responseBody any) error {
	if c == nil || c.HTTPClient == nil {
		return errors.New("telegram client is not configured")
	}
	if ctx == nil {
		return errors.New("telegram request context is nil")
	}
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return errors.New("encode telegram request")
		}
		if int64(len(data)) > c.requestLimit() {
			return errors.New("telegram request exceeds bounded size")
		}
		body = bytes.NewReader(data)
	}
	requestURL := c.BaseURL + "/bot" + c.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, body)
	if err != nil {
		return errors.New("create telegram request")
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return transportFailure(err)
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, c.responseLimit())
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, responseBody); err != nil {
		return errors.New("decode telegram API response")
	}
	if ok, code, _, retryAfter := responseEnvelope(responseBody); !ok {
		return &APIError{Code: code, RetryAfter: retryAfter}
	}
	return nil
}

func (c *Client) requestLimit() int64 {
	if c.MaxRequestBytes > 0 {
		return c.MaxRequestBytes
	}
	return MaxRequestBytes
}
func (c *Client) responseLimit() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return MaxResponseBytes
}

func (c *Client) uploadLimit() int64 {
	if c.MaxUploadBytes > 0 {
		return c.MaxUploadBytes
	}
	return MaxFileBytes
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("telegram response limit is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		// The connection broke mid-response.
		return nil, transportFailure(err)
	}
	if int64(len(data)) > limit {
		return nil, errors.New("telegram API response exceeds bounded size")
	}
	return data, nil
}

func decodeAPIError(status int, data []byte) error {
	var envelope struct {
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after,omitempty"`
		} `json:"parameters,omitempty"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		err := &APIError{HTTPStatus: status, Code: envelope.ErrorCode}
		if envelope.Parameters != nil {
			err.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		return err
	}
	return &APIError{HTTPStatus: status}
}

func responseEnvelope(value any) (bool, int, string, time.Duration) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, 0, "", 0
	}
	var envelope struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after,omitempty"`
		} `json:"parameters,omitempty"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return false, 0, "", 0
	}
	if envelope.OK {
		return true, 0, "", 0
	}
	var retry time.Duration
	if envelope.Parameters != nil {
		retry = time.Duration(envelope.Parameters.RetryAfter) * time.Second
	}
	return false, envelope.ErrorCode, envelope.Description, retry
}
