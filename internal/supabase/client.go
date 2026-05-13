package supabase

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
	"path"
	"strings"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/domain"
)

type Client struct {
	cfg    config.SupabaseConfig
	http   *http.Client
	logger *slog.Logger
	base   string
}

func New(cfg config.SupabaseConfig, httpClient *http.Client, logger *slog.Logger) *Client {
	return &Client{cfg: cfg, http: httpClient, logger: logger, base: strings.TrimRight(cfg.URL, "/")}
}

func (c *Client) ClaimJob(ctx context.Context, workerID string) (*domain.ProcessingJob, error) {
	var jobs []domain.ProcessingJob
	payload := map[string]any{"p_worker_id": workerID}
	if err := c.rpc(ctx, "claim_processing_job", payload, &jobs); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return &jobs[0], nil
}

func (c *Client) GetCall(ctx context.Context, callID string) (domain.Call, error) {
	var calls []domain.Call
	u := c.restURL("calls", map[string]string{"id": "eq." + callID, "limit": "1"})
	if err := c.doJSON(ctx, http.MethodGet, u, nil, &calls, nil); err != nil {
		return domain.Call{}, err
	}
	if len(calls) == 0 {
		return domain.Call{}, fmt.Errorf("call not found: %s", callID)
	}
	return calls[0], nil
}

// GetOriginalAudioFile resolves the input audio row for ASR.
// Order: calls.audio_file_id → call_files with known file_type values (Lovable often uses not only original_audio) → latest audio/* MIME.
func (c *Client) GetOriginalAudioFile(ctx context.Context, call domain.Call) (domain.CallFile, error) {
	if ptr := call.AudioFileID; ptr != nil {
		if fid := strings.TrimSpace(*ptr); fid != "" {
			var byID []domain.CallFile
			u := c.restURL("call_files", map[string]string{
				"id":      "eq." + fid,
				"call_id": "eq." + call.ID,
				"limit":   "1",
			})
			if err := c.doJSON(ctx, http.MethodGet, u, nil, &byID, nil); err != nil {
				return domain.CallFile{}, err
			}
			if len(byID) > 0 {
				return byID[0], nil
			}
		}
	}
	types := []string{
		domain.FileTypeOriginalAudio,
		domain.FileTypeUpload,
		domain.FileTypeSource,
		domain.FileTypeRecording,
		domain.FileTypeAudio,
	}
	var files []domain.CallFile
	u := c.restURL("call_files", map[string]string{
		"call_id":   "eq." + call.ID,
		"file_type": "in.(" + strings.Join(types, ",") + ")",
		"order":     "created_at.desc",
		"limit":     "1",
	})
	if err := c.doJSON(ctx, http.MethodGet, u, nil, &files, nil); err != nil {
		return domain.CallFile{}, err
	}
	if len(files) > 0 {
		return files[0], nil
	}
	var byMime []domain.CallFile
	u2 := c.restURL("call_files", map[string]string{
		"call_id":   "eq." + call.ID,
		"mime_type": "like.audio*",
		"order":     "created_at.desc",
		"limit":     "1",
	})
	if err := c.doJSON(ctx, http.MethodGet, u2, nil, &byMime, nil); err != nil {
		return domain.CallFile{}, err
	}
	if len(byMime) > 0 {
		return byMime[0], nil
	}
	return domain.CallFile{}, fmt.Errorf("original audio file not found for call_id=%s", call.ID)
}

func (c *Client) DownloadObject(ctx context.Context, bucket, objectPath string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 300 << 20
	}
	u := c.storageObjectURL(bucket, objectPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download storage object failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("storage object exceeds max size")
	}
	return body, nil
}

func (c *Client) UploadObject(ctx context.Context, bucket, objectPath, contentType string, body []byte, upsert bool) error {
	u := c.storageObjectURL(bucket, objectPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	if upsert {
		req.Header.Set("x-upsert", "true")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload storage object failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) UpdateCallStatus(ctx context.Context, callID, status string, errorCode, errorMessage *string) error {
	payload := map[string]any{"status": status, "updated_at": time.Now().UTC().Format(time.RFC3339)}
	if errorCode != nil {
		payload["error_code"] = *errorCode
	} else if status != domain.CallStatusError {
		payload["error_code"] = nil
	}
	if errorMessage != nil {
		payload["error_message"] = *errorMessage
	} else if status != domain.CallStatusError {
		payload["error_message"] = nil
	}
	u := c.restURL("calls", map[string]string{"id": "eq." + callID})
	return c.doJSON(ctx, http.MethodPatch, u, payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) CompleteJob(ctx context.Context, jobID string) error {
	payload := map[string]any{
		"status":        domain.JobStatusSuccess,
		"finished_at":   time.Now().UTC().Format(time.RFC3339),
		"updated_at":    time.Now().UTC().Format(time.RFC3339),
		"error_code":    nil,
		"error_message": nil,
		"error_details": nil,
	}
	u := c.restURL("processing_jobs", map[string]string{"id": "eq." + jobID})
	return c.doJSON(ctx, http.MethodPatch, u, payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) FailJob(ctx context.Context, jobID, callID, code, message string, details any) error {
	detailsBytes, _ := json.Marshal(details)
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"status":        domain.JobStatusError,
		"finished_at":   now,
		"updated_at":    now,
		"error_code":    code,
		"error_message": message,
		"error_details": json.RawMessage(detailsBytes),
	}
	u := c.restURL("processing_jobs", map[string]string{"id": "eq." + jobID})
	if err := c.doJSON(ctx, http.MethodPatch, u, payload, nil, map[string]string{"Prefer": "return=minimal"}); err != nil {
		return err
	}
	return c.UpdateCallStatus(ctx, callID, domain.CallStatusError, &code, &message)
}

func (c *Client) InsertLog(ctx context.Context, organizationID, callID, jobID, level, step, message string, details any) error {
	payload := map[string]any{
		"organization_id": organizationID,
		"call_id":         callID,
		"job_id":          jobID,
		"level":           level,
		"step":            step,
		"message":         message,
		"details_json":    details,
	}
	return c.doJSON(ctx, http.MethodPost, c.tableURL("processing_logs"), payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) InsertTranscription(ctx context.Context, organizationID, callID, text string, segments []domain.SpeechSegment, rawASR any, asrModel string) error {
	payload := map[string]any{
		"organization_id":    organizationID,
		"call_id":            callID,
		"transcription_text": text,
		"segments_json":      segments,
		"raw_asr_json":       rawASR,
		"speaker_mode":       "salute_speaker_separation",
		"asr_provider":       "sber_salutespeech",
	}
	if strings.TrimSpace(asrModel) != "" {
		payload["asr_model"] = asrModel
	}
	return c.doJSON(ctx, http.MethodPost, c.tableURL("call_transcriptions"), payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) InsertAnalysis(ctx context.Context, organizationID, callID string, templateID *string, summary string, rawLLM any, llmModel string) error {
	payload := map[string]any{
		"organization_id":      organizationID,
		"call_id":              callID,
		"analysis_template_id": templateID,
		"summary":              summary,
		"topics_json":          []any{},
		"client_needs_json":    []any{},
		"agreements_json":      []any{},
		"next_steps_json":      []any{},
		"risks_json":           []any{},
		"strengths_json":       []any{},
		"weaknesses_json":      []any{},
		"recommendations_json": []any{},
		"quality_flags_json":   map[string]any{},
		"raw_llm_json":         rawLLM,
		"llm_provider":         "gigachat",
	}
	if strings.TrimSpace(llmModel) != "" {
		payload["llm_model"] = llmModel
	}
	return c.doJSON(ctx, http.MethodPost, c.tableURL("call_analysis"), payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) InsertCallFile(ctx context.Context, organizationID, callID, fileType, bucket, objectPath, filename, mime string, size int64) (string, error) {
	payload := map[string]any{
		"organization_id":   organizationID,
		"call_id":           callID,
		"file_type":         fileType,
		"storage_bucket":    bucket,
		"storage_path":      objectPath,
		"original_filename": filename,
		"mime_type":         mime,
		"size_bytes":        size,
	}
	var out []struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.tableURL("call_files"), payload, &out, map[string]string{"Prefer": "return=representation"}); err != nil {
		return "", err
	}
	if len(out) == 0 || out[0].ID == "" {
		return "", errors.New("insert call_files returned empty id")
	}
	return out[0].ID, nil
}

func (c *Client) InsertCallReport(ctx context.Context, organizationID, callID, reportType, fileID string) error {
	payload := map[string]any{"organization_id": organizationID, "call_id": callID, "report_type": reportType, "file_id": fileID}
	return c.doJSON(ctx, http.MethodPost, c.tableURL("call_reports"), payload, nil, map[string]string{"Prefer": "return=minimal"})
}

func (c *Client) ReportPath(organizationID, callID, filename string) string {
	return path.Join("organizations", organizationID, "calls", callID, "reports", filename)
}

func (c *Client) RawPath(organizationID, callID, filename string) string {
	return path.Join("organizations", organizationID, "calls", callID, "raw", filename)
}

func (c *Client) rpc(ctx context.Context, name string, payload any, out any) error {
	return c.doJSON(ctx, http.MethodPost, c.base+"/rest/v1/rpc/"+name, payload, out, nil)
}

func (c *Client) tableURL(table string) string {
	return c.base + "/rest/v1/" + table
}

func (c *Client) restURL(table string, params map[string]string) string {
	u, _ := url.Parse(c.tableURL(table))
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) storageObjectURL(bucket, objectPath string) string {
	return c.base + "/storage/v1/object/" + url.PathEscape(bucket) + "/" + escapeObjectPath(objectPath)
}

func escapeObjectPath(objectPath string) string {
	parts := strings.Split(strings.TrimLeft(objectPath, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (c *Client) doJSON(ctx context.Context, method, url string, payload any, out any, extraHeaders map[string]string) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("supabase request failed: method=%s url=%s status=%d body=%s", method, url, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode supabase response: %w body=%s", err, string(respBody))
		}
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("apikey", c.cfg.ServiceRoleKey)
	req.Header.Set("Authorization", "Bearer "+c.cfg.ServiceRoleKey)
	req.Header.Set("Accept", "application/json")
	if c.cfg.Schema != "" && c.cfg.Schema != "public" {
		req.Header.Set("Accept-Profile", c.cfg.Schema)
		req.Header.Set("Content-Profile", c.cfg.Schema)
	}
}
