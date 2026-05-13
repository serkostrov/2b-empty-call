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
	"sort"
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

	taskID, rawTask, err := c.createTask(ctx, token, fileID, audio)
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

	segments := extractSegments(rawResult)
	text := formatSpeakerTranscript(segments)

	if strings.TrimSpace(text) == "" {
		return domain.ASRResult{Raw: rawResult}, errors.New("empty transcription or unsupported ASR response structure")
	}

	return domain.ASRResult{
		Text:     text,
		Segments: segments,
		Raw:      rawResult,
	}, nil
}
func extractSegments(raw any) []domain.SpeechSegment {
	var segments []domain.SpeechSegment
	walkTranscriptions(raw, &segments)

	segments = compactSegments(segments)

	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].Start == segments[j].Start {
			return segments[i].End < segments[j].End
		}
		return segments[i].Start < segments[j].Start
	})

	return segments
}

func walkTranscriptions(v any, out *[]domain.SpeechSegment) {
	switch x := v.(type) {
	case map[string]any:
		if isTranscriptionNode(x) {
			if seg, ok := segmentFromTranscription(x); ok {
				*out = append(*out, seg)
			}
			return
		}

		for _, val := range x {
			walkTranscriptions(val, out)
		}

	case []any:
		for _, item := range x {
			walkTranscriptions(item, out)
		}
	}
}

func isTranscriptionNode(m map[string]any) bool {
	_, hasResults := m["results"]
	_, hasChannel := m["channel"]
	_, hasSpeakerInfo := m["speaker_info"]
	_, hasSpeakerInfoCamel := m["speakerInfo"]

	return hasResults && (hasChannel || hasSpeakerInfo || hasSpeakerInfoCamel)
}

func segmentFromTranscription(m map[string]any) (domain.SpeechSegment, bool) {
	if eou, ok := boolValue(m, "eou"); ok && !eou {
		return domain.SpeechSegment{}, false
	}

	text := bestHypothesisText(m)
	if strings.TrimSpace(text) == "" {
		return domain.SpeechSegment{}, false
	}

	channel := intFromAny(firstValue(m, "channel"))

	speakerID := extractSpeakerID(m)
	if speakerID <= 0 && channel > 0 {
		speakerID = channel
	}
	if speakerID <= 0 {
		speakerID = 1
	}

	start := durationSeconds(firstValue(m, "processed_audio_start", "processedAudioStart"))
	end := durationSeconds(firstValue(m, "processed_audio_end", "processedAudioEnd"))

	return domain.SpeechSegment{
		Speaker: fmt.Sprintf("Спикер %d", speakerID),
		Text:    strings.TrimSpace(text),
		Start:   start,
		End:     end,
		Channel: channel,
	}, true
}

func bestHypothesisText(m map[string]any) string {
	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		return ""
	}

	h, ok := results[0].(map[string]any)
	if !ok {
		return ""
	}

	text := firstString(h, "normalized_text", "normalizedText")
	if strings.TrimSpace(text) != "" {
		return text
	}

	return firstString(h, "text")
}

func boolValue(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}

	b, ok := v.(bool)
	return b, ok
}

func compactSegments(in []domain.SpeechSegment) []domain.SpeechSegment {
	out := make([]domain.SpeechSegment, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for _, seg := range in {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}

		key := fmt.Sprintf("%s|%.3f|%.3f|%s", seg.Speaker, seg.Start, seg.End, text)
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		seg.Text = text
		out = append(out, seg)
	}

	return out
}

func walkSegments(v any, segments *[]domain.SpeechSegment) {
	switch x := v.(type) {
	case map[string]any:
		if seg, ok := segmentFromMap(x); ok {
			*segments = append(*segments, seg)
			return
		}
		for _, val := range x {
			walkSegments(val, segments)
		}
	case []any:
		for _, item := range x {
			walkSegments(item, segments)
		}
	}
}

func segmentFromMap(m map[string]any) (domain.SpeechSegment, bool) {
	text := firstString(m, "normalized_text", "normalizedText", "text", "transcript", "transcription")
	if strings.TrimSpace(text) == "" {
		return domain.SpeechSegment{}, false
	}

	speakerID := extractSpeakerID(m)
	channel := intFromAny(firstValue(m, "channel"))

	if speakerID <= 0 && channel > 0 {
		speakerID = channel
	}
	if speakerID <= 0 {
		speakerID = 1
	}

	return domain.SpeechSegment{
		Speaker: fmt.Sprintf("Спикер %d", speakerID),
		Text:    strings.TrimSpace(text),
		Start:   durationSeconds(firstValue(m, "start", "processed_audio_start", "processedAudioStart")),
		End:     durationSeconds(firstValue(m, "end", "processed_audio_end", "processedAudioEnd")),
		Channel: channel,
	}, true
}

func extractSpeakerID(m map[string]any) int {
	for _, key := range []string{"speaker_info", "speakerInfo"} {
		if raw, ok := m[key].(map[string]any); ok {
			if id := intFromAny(firstValue(raw, "speaker_id", "speakerId")); id > 0 {
				return id
			}
		}
	}
	return intFromAny(firstValue(m, "speaker_id", "speakerId"))
}

func formatSpeakerTranscript(segments []domain.SpeechSegment) string {
	var b strings.Builder

	for _, seg := range segments {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}

		b.WriteString(formatTime(seg.Start))
		b.WriteString("\n")
		b.WriteString(seg.Speaker)
		b.WriteString("\n")
		b.WriteString(seg.Text)
	}

	return strings.TrimSpace(b.String())
}

func formatTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}

	total := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func firstValue(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(strings.TrimSpace(x), "%d", &i)
		return i
	default:
		return 0
	}
}

func durationSeconds(v any) float64 {
	switch x := v.(type) {
	case map[string]any:
		seconds := floatFromAny(firstValue(x, "seconds"))
		nanos := floatFromAny(firstValue(x, "nanos"))
		return seconds + nanos/1_000_000_000
	case string:
		s := strings.TrimSuffix(strings.TrimSpace(x), "s")
		var f float64
		_, _ = fmt.Sscanf(s, "%f", &f)
		return f
	default:
		return 0
	}
}

func floatFromAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
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

	id := extractUploadFileID(raw)
	if id == "" {
		return "", raw, fmt.Errorf("salute upload response does not contain file id: body=%s", truncateForLog(body, 2048))
	}
	return id, raw, nil
}

func (c *Client) resolveAudioEncoding(audio domain.AudioFile) (string, error) {
	if s := strings.TrimSpace(c.cfg.AudioEncoding); s != "" {
		return s, nil
	}
	return inferSaluteAudioEncoding(audio)
}

func inferSaluteAudioEncoding(audio domain.AudioFile) (string, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(audio.Filename)))
	switch ext {
	case ".mp3":
		return "MP3", nil
	case ".wav":
		return "PCM_S16LE", nil
	case ".flac":
		return "FLAC", nil
	case ".ogg":
		return "OPUS", nil
	}

	ct := strings.ToLower(strings.TrimSpace(audio.ContentType))
	ctBase := ct
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ctBase = strings.TrimSpace(ct[:idx])
	}
	switch {
	case ctBase == "audio/mpeg" || ctBase == "audio/mp3":
		return "MP3", nil
	case ctBase == "audio/wav" || ctBase == "audio/x-wav" || ctBase == "audio/wave":
		return "PCM_S16LE", nil
	case ctBase == "audio/flac":
		return "FLAC", nil
	case strings.Contains(ct, "codecs=opus"):
		return "OPUS", nil
	case ctBase == "audio/ogg":
		return "OPUS", nil
	case ctBase == "audio/x-pcm" || strings.HasPrefix(ct, "audio/x-pcm"):
		return "PCM_S16LE", nil
	case strings.HasPrefix(ct, "audio/pcma"):
		return "ALAW", nil
	case strings.HasPrefix(ct, "audio/pcmu"):
		return "MULAW", nil
	}

	return "", fmt.Errorf("cannot infer salute audio_encoding for filename %q (content-type %q); set SALUTE_AUDIO_ENCODING (e.g. MP3, PCM_S16LE, FLAC, OPUS)", audio.Filename, audio.ContentType)
}

func (c *Client) createTask(ctx context.Context, token string, fileID string, audio domain.AudioFile) (string, any, error) {
	enc, err := c.resolveAudioEncoding(audio)
	if err != nil {
		return "", nil, err
	}
	// https://developers.sber.ru/docs/ru/salutespeech/rest/post-async-speech-recognition — required: request_file_id, options (incl. audio_encoding)
	options := map[string]any{
		"audio_encoding": enc,
		"language":       c.cfg.Language,
		"model":          c.cfg.Model,

		// Для этого файла корректно:
		"sample_rate":    c.cfg.SampleRate,
		"channels_count": c.cfg.ChannelsCount,

		// Чтобы не получать пачку альтернатив.
		"hypotheses_count": 1,

		// Чтобы не тащить partial-гипотезы.
		"enable_partial_results": map[string]any{
			"enable": false,
		},

		// Чтобы SaluteSpeech сам разделял говорящих.
		"speaker_separation_options": map[string]any{
			"enable":                   true,
			"enable_only_main_speaker": false,
			"count":                    2,
		},

		"normalization_options": map[string]any{
			"enable": map[string]any{
				"enable": true,
			},
			"punctuation": map[string]any{
				"enable": true,
			},
			"capitalization": map[string]any{
				"enable": true,
			},
		},
	}

	payload := map[string]any{
		"request_file_id": fileID,
		"options":         options,
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

	id := extractAsyncTaskID(raw)
	if id == "" {
		return "", raw, fmt.Errorf("salute task response does not contain task id: body=%s", truncateForLog(body, 2048))
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
	// SaluteSpeech REST expects query param "id", not "task_id" (see public clients / gateway).
	q.Set("id", taskID)
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

	inner := saluteResultPayload(raw)
	state := strings.ToLower(firstString(inner, "status", "task_status", "state"))
	c.logger.Debug("salute task status", "task_id", taskID, "status", state)
	switch state {
	case "done", "success", "completed", "complete":
		id := firstString(inner, "response_file_id", "responseFileId", "result_file_id", "resultFileId", "file_id", "fileId")
		if id == "" {
			return "", raw, false, fmt.Errorf("salute task completed without result file id")
		}
		return id, raw, true, nil
	case "error", "failed", "failure":
		return "", raw, false, fmt.Errorf("salute task failed: %v", inner)
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
	q.Set("response_file_id", fileID)
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
			if s := stringFromAny(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// extractUploadFileID parses SmartSpeech data:upload JSON; the API has used both flat and wrapped shapes.
func extractUploadFileID(raw map[string]any) string {
	const maxDepth = 8
	var walk func(m map[string]any, depth int) string
	walk = func(m map[string]any, depth int) string {
		if m == nil || depth > maxDepth {
			return ""
		}
		if s := firstString(m,
			"file_id", "fileId", "id",
			"request_file_id", "requestFileId", "request_fileId",
		); s != "" {
			return s
		}
		for _, wrap := range []string{"result", "data", "response", "payload", "file", "upload"} {
			if v, ok := m[wrap]; ok {
				if child, ok := v.(map[string]any); ok {
					if s := walk(child, depth+1); s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	return walk(raw, 0)
}

// saluteResultPayload returns the inner Task object when the API wraps it as { "status": 200, "result": { ... } }.
func saluteResultPayload(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	if r, ok := raw["result"].(map[string]any); ok {
		return r
	}
	return raw
}

// extractAsyncTaskID parses speech:async_recognize JSON (Task or gateway-wrapped shapes).
func extractAsyncTaskID(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	if arr, ok := raw["tasks"].([]any); ok && len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			if s := extractAsyncTaskIDFromMap(m, 1); s != "" {
				return s
			}
		}
	}
	if v, ok := raw["name"].(string); ok {
		if s := extractIDFromResourceName(v); s != "" {
			return s
		}
	}
	return extractAsyncTaskIDFromMap(raw, 0)
}

func extractIDFromResourceName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
		return strings.TrimSpace(name[i+1:])
	}
	return ""
}

func extractAsyncTaskIDFromMap(m map[string]any, depth int) string {
	const maxDepth = 10
	if m == nil || depth > maxDepth {
		return ""
	}
	for _, k := range []string{
		"task_id", "taskId",
		"operation_id", "operationId",
		"request_id", "requestId",
		"uuid", "id",
	} {
		if v, ok := m[k]; ok {
			if s := stringFromAny(v); s != "" {
				return s
			}
		}
	}
	for _, wrap := range []string{"result", "data", "response", "payload", "task"} {
		v, ok := m[wrap]
		if !ok {
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			if s := extractAsyncTaskIDFromMap(child, depth+1); s != "" {
				return s
			}
		case string:
			if s := strings.TrimSpace(child); s != "" {
				return s
			}
		}
	}
	return ""
}

func stringFromAny(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case json.Number:
		return strings.TrimSpace(s.String())
	case float64:
		if s == float64(int64(s)) {
			return fmt.Sprintf("%.0f", s)
		}
		return strings.TrimSpace(fmt.Sprintf("%g", s))
	case int:
		return fmt.Sprintf("%d", s)
	case int64:
		return fmt.Sprintf("%d", s)
	default:
		return ""
	}
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
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
