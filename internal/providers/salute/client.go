package salute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/domain"
	"github.com/serkostrov/2b-empty-call/internal/providers"
)

type Client struct {
	cfg    config.SaluteConfig
	oauth  *providers.OAuthClient
	http   *http.Client
	logger *slog.Logger
}

func New(cfg config.SberConfig, httpClient *http.Client, log *slog.Logger) *Client {
	return &Client{
		cfg:    cfg.Salute,
		http:   httpClient,
		logger: log,
		oauth: &providers.OAuthClient{
			HTTPClient: httpClient,
			TokenURL:   cfg.OAuthURL,
			AuthScheme: cfg.Salute.AuthHeaderScheme,
			AuthKey:    cfg.Salute.AuthKey,
			Scope:      cfg.Salute.Scope,
		},
	}
}

func (c *Client) Transcribe(ctx context.Context, audio domain.AudioFile) (domain.ASRResult, error) {
	token, err := c.oauth.Token(ctx)
	if err != nil {
		return domain.ASRResult{}, fmt.Errorf("salute auth: %w", err)
	}

	fileID, rawUpload, err := c.upload(ctx, token, audio)
	if err != nil {
		return domain.ASRResult{Raw: rawUpload}, err
	}

	taskID, rawTask, err := c.createTask(ctx, token, fileID)
	if err != nil {
		return domain.ASRResult{Raw: rawTask}, err
	}

	resultFileID, rawStatus, err := c.poll(ctx, token, taskID)
	if err != nil {
		return domain.ASRResult{Raw: rawStatus}, err
	}

	rawResult, err := c.download(ctx, token, resultFileID)
	if err != nil {
		return domain.ASRResult{Raw: rawResult}, err
	}

	text := extractText(rawResult)
	if strings.TrimSpace(text) == "" {
		return domain.ASRResult{Raw: rawResult}, errors.New("empty transcription or unsupported ASR response structure")
	}
	return domain.ASRResult{Text: text, Raw: rawResult}, nil
}

func (c *Client) upload(ctx context.Context, token string, audio domain.AudioFile) (string, any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.UploadURL, bytes.NewReader(audio.Bytes))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType(audio))

	var raw map[string]any
	status, body, err := doJSON(c.http, req, &raw)
	if err != nil {
		return "", raw, err
	}
	if status < 200 || status >= 300 {
		return "", raw, fmt.Errorf("salute upload failed: status=%d body=%s", status, body)
	}

	id := firstString(raw, "file_id", "fileId", "id", "request_file_id")
	if id == "" {
		return "", raw, fmt.Errorf("salute upload response does not contain file id")
	}
	return id, raw, nil
}

func (c *Client) createTask(ctx context.Context, token string, fileID string) (string, any, error) {
	payload := map[string]any{
		"file_id": fileID,
		"options": map[string]any{
			"language": c.cfg.Language,
			"model":    c.cfg.Model,
		},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.RecognizeURL, bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	var raw map[string]any
	status, body, err := doJSON(c.http, req, &raw)
	if err != nil {
		return "", raw, err
	}
	if status < 200 || status >= 300 {
		return "", raw, fmt.Errorf("salute create task failed: status=%d body=%s", status, body)
	}

	id := firstString(raw, "task_id", "taskId", "id")
	if id == "" {
		return "", raw, fmt.Errorf("salute task response does not contain task id")
	}
	return id, raw, nil
}

func (c *Client) poll(ctx context.Context, token string, taskID string) (string, any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PollTimeout)
	defer cancel()
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	var last any

	for {
		resultID, raw, done, err := c.pollOnce(ctx, token, taskID)
		last = raw
		if err != nil {
			return "", raw, err
		}
		if done {
			return resultID, raw, nil
		}
		select {
		case <-ctx.Done():
			return "", last, fmt.Errorf("salute polling timeout: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) pollOnce(ctx context.Context, token, taskID string) (string, any, bool, error) {
	u, err := url.Parse(c.cfg.TaskStatusURL)
	if err != nil {
		return "", nil, false, err
	}
	q := u.Query()
	q.Set("task_id", taskID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	var raw map[string]any
	status, body, err := doJSON(c.http, req, &raw)
	if err != nil {
		return "", raw, false, err
	}
	if status < 200 || status >= 300 {
		return "", raw, false, fmt.Errorf("salute task status failed: status=%d body=%s", status, body)
	}

	state := strings.ToLower(firstString(raw, "status", "task_status", "state"))
	c.logger.Debug("salute task status", "task_id", taskID, "status", state)
	switch state {
	case "done", "success", "completed", "complete":
		id := firstString(raw, "result_file_id", "resultFileId", "file_id", "fileId", "response_file_id")
		if id == "" {
			return "", raw, false, fmt.Errorf("salute task completed without result file id")
		}
		return id, raw, true, nil
	case "error", "failed", "failure":
		return "", raw, false, fmt.Errorf("salute task failed: %v", raw)
	default:
		return "", raw, false, nil
	}
}

func (c *Client) download(ctx context.Context, token, fileID string) (any, error) {
	u, err := url.Parse(c.cfg.DownloadURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("file_id", fileID)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	var raw any
	status, body, err := doJSON(c.http, req, &raw)
	if err != nil {
		return raw, err
	}
	if status < 200 || status >= 300 {
		return raw, fmt.Errorf("salute download failed: status=%d body=%s", status, body)
	}
	return raw, nil
}

func doJSON(client *http.Client, req *http.Request, out any) (int, string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return resp.StatusCode, string(body), err
	}
	if len(body) > 0 && out != nil {
		if err := json.Unmarshal(body, out); err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, string(body), fmt.Errorf("decode json: %w", err)
		}
	}
	return resp.StatusCode, string(body), nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func extractText(raw any) string {
	parts := make([]string, 0, 128)
	walk(raw, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func walk(v any, parts *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			lk := strings.ToLower(k)
			if lk == "text" || lk == "transcript" || lk == "transcription" {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					*parts = append(*parts, strings.TrimSpace(s))
				}
			}
			walk(val, parts)
		}
	case []any:
		for _, item := range x {
			walk(item, parts)
		}
	}
}

func contentType(audio domain.AudioFile) string {
	if audio.ContentType != "" && audio.ContentType != "application/octet-stream" {
		return audio.ContentType
	}
	switch strings.ToLower(filepath.Ext(audio.Filename)) {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}
