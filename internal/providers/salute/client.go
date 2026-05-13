package salute

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
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
	segments = preferFinalUtterances(segments)
	segments = dedupeIdenticalMultiChannel(segments)

	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].Start == segments[j].Start {
			if segments[i].End == segments[j].End {
				return segments[i].Speaker < segments[j].Speaker
			}
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

		for key, val := range x {
			// results нельзя обходить рекурсивно.
			// Это список гипотез одного блока, а не отдельные реплики.
			if key == "results" {
				continue
			}
			walkTranscriptions(val, out)
		}

	case []any:
		for _, item := range x {
			walkTranscriptions(item, out)
		}
	}
}

func isTranscriptionNode(m map[string]any) bool {
	if _, ok := m["results"]; !ok {
		return false
	}

	if _, ok := m["processed_audio_start"]; ok {
		return true
	}
	if _, ok := m["processedAudioStart"]; ok {
		return true
	}
	if _, ok := m["processed_audio_end"]; ok {
		return true
	}
	if _, ok := m["processedAudioEnd"]; ok {
		return true
	}
	if _, ok := m["channel"]; ok {
		return true
	}
	if _, ok := m["speaker_info"]; ok {
		return true
	}
	if _, ok := m["speakerInfo"]; ok {
		return true
	}
	if _, ok := m["eou"]; ok {
		return true
	}

	return false
}

func segmentFromTranscription(m map[string]any) (domain.SpeechSegment, bool) {
	text := bestHypothesisText(m)
	if strings.TrimSpace(text) == "" {
		return domain.SpeechSegment{}, false
	}

	speaker, skip := speakerDisplayFromNode(m)
	if skip {
		return domain.SpeechSegment{}, false
	}

	channel := intFromAny(firstValue(m, "channel"))
	if _, has := m["channel"]; !has {
		channel = 0
	}

	processedStart := durationSeconds(firstValue(m, "processed_audio_start", "processedAudioStart"))
	processedEnd := durationSeconds(firstValue(m, "processed_audio_end", "processedAudioEnd"))
	start, end := utteranceTimeBounds(m, processedStart, processedEnd)

	bufS, bufE := processedStart, processedEnd
	if bufE <= bufS {
		bufS, bufE = start, end
	}

	eou := false
	if v, ok := m["eou"]; ok {
		eou, _ = v.(bool)
	}

	return domain.SpeechSegment{
		Speaker:           speaker,
		Text:              strings.TrimSpace(text),
		Start:             start,
		End:               end,
		Channel:           channel,
		EOU:               eou,
		BufferWindowStart: bufS,
		BufferWindowEnd:   bufE,
	}, true
}

// speakerDisplayFromNode maps SaluteSpeech RecognitionResponse.{speaker_info, channel} to a stable label.
// See recognition.proto (SpeakerInfo, channel) and public JSON examples on developers.sber.ru.
func speakerDisplayFromNode(m map[string]any) (label string, skip bool) {
	for _, key := range []string{"speaker_info", "speakerInfo"} {
		switch raw := m[key].(type) {
		case map[string]any:
			if v, ok := firstOptionalValue(raw, "speaker_id", "speakerId"); ok {
				return speakerLabelFromID(intFromAny(v))
			}
		case []any:
			if len(raw) == 0 {
				continue
			}
			if item, ok := raw[0].(map[string]any); ok {
				if v, ok := firstOptionalValue(item, "speaker_id", "speakerId"); ok {
					return speakerLabelFromID(intFromAny(v))
				}
			}
		}
	}

	if _, has := m["channel"]; has {
		ch := intFromAny(m["channel"])
		if ch < 0 {
			ch = 0
		}
		// channel is 0-based in API examples (0 = first channel / side).
		return fmt.Sprintf("Спикер %d", ch+1), false
	}

	return "Спикер 1", false
}

func speakerLabelFromID(id int) (label string, skip bool) {
	if id == -1 {
		// Overlapping / mixed-speaker hypothesis; keeping it duplicates diarized lines.
		return "", true
	}
	if id < -1 {
		return fmt.Sprintf("Спикер (%d)", id), false
	}
	if id == 0 {
		return "Спикер 1", false
	}
	return fmt.Sprintf("Спикер %d", id), false
}

func firstOptionalValue(m map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v, true
		}
	}
	return nil, false
}

// utteranceTimeBounds prefers per-hypothesis word alignments so segment timestamps reflect speech,
// not only the decoder buffer cursor (processed_audio_* is often identical across partial hypotheses).
func utteranceTimeBounds(m map[string]any, processedStart, processedEnd float64) (start, end float64) {
	start, end = processedStart, processedEnd
	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		return start, end
	}
	first, ok := results[0].(map[string]any)
	if !ok {
		return start, end
	}

	wa, ok := first["word_alignments"].([]any)
	if !ok {
		wa, ok = first["wordAlignments"].([]any)
	}
	if !ok || len(wa) == 0 {
		return start, end
	}

	firstWord, ok := wa[0].(map[string]any)
	if !ok {
		return start, end
	}
	lastWord, ok := wa[len(wa)-1].(map[string]any)
	if !ok {
		return start, end
	}

	wStart := durationSeconds(firstValue(firstWord, "start"))
	wEnd := durationSeconds(firstValue(lastWord, "end"))
	if wEnd < wStart {
		return start, end
	}
	return wStart, wEnd
}

func bestHypothesisText(m map[string]any) string {
	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		return ""
	}

	first, ok := results[0].(map[string]any)
	if !ok {
		return ""
	}

	if text := firstString(first, "normalized_text", "normalizedText"); text != "" {
		return text
	}

	return firstString(first, "text")
}

func compactSegments(in []domain.SpeechSegment) []domain.SpeechSegment {
	type candidate struct {
		seg  domain.SpeechSegment
		hash string
	}

	bestByInterval := make(map[string]candidate)

	for _, seg := range in {
		seg.Text = strings.TrimSpace(seg.Text)
		if seg.Text == "" {
			continue
		}

		// Один и тот же интервал не должен попадать в отчет дважды.
		// Это убирает дубли от dual-mono stereo и альтернативных гипотез.
		intervalKey := fmt.Sprintf(
			"%s|%d|%d",
			seg.Speaker,
			int(seg.Start*10),
			int(seg.End*10),
		)

		textHash := normalizedTextHash(seg.Text)

		current, exists := bestByInterval[intervalKey]
		if !exists {
			bestByInterval[intervalKey] = candidate{seg: seg, hash: textHash}
			continue
		}

		// Если SaluteSpeech дал две версии одного интервала,
		// оставляем более информативную: обычно она длиннее.
		if len([]rune(seg.Text)) > len([]rune(current.seg.Text)) {
			bestByInterval[intervalKey] = candidate{seg: seg, hash: textHash}
		}
	}

	out := make([]domain.SpeechSegment, 0, len(bestByInterval))
	seenText := make(map[string]struct{})

	for _, item := range bestByInterval {
		globalKey := fmt.Sprintf(
			"%s|%d|%s",
			item.seg.Speaker,
			int(item.seg.Start*10),
			item.hash,
		)

		if _, ok := seenText[globalKey]; ok {
			continue
		}

		seenText[globalKey] = struct{}{}
		out = append(out, item.seg)
	}

	return out
}

// preferFinalUtterances drops non-final SaluteSpeech refinements for the same speaker and time bucket.
// RecognitionResponse.eou marks a committed utterance; earlier rows often repeat partial text.
func preferFinalUtterances(in []domain.SpeechSegment) []domain.SpeechSegment {
	type groupKey struct {
		speaker string
		startMs int64
		endMs   int64
	}

	buckets := make(map[groupKey][]domain.SpeechSegment)
	order := make([]groupKey, 0)

	for _, seg := range in {
		seg.Text = strings.TrimSpace(seg.Text)
		if seg.Text == "" {
			continue
		}
		k := groupKey{seg.Speaker, quantMs(seg.BufferWindowStart), quantMs(seg.BufferWindowEnd)}
		if _, ok := buckets[k]; !ok {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], seg)
	}

	out := make([]domain.SpeechSegment, 0, len(order))
	for _, k := range order {
		items := buckets[k]
		var finals []domain.SpeechSegment
		for _, s := range items {
			if s.EOU {
				finals = append(finals, s)
			}
		}
		pick := items
		if len(finals) > 0 {
			pick = finals
		}
		best := pick[0]
		for _, s := range pick[1:] {
			if utteranceScore(s) > utteranceScore(best) {
				best = s
			}
		}
		out = append(out, best)
	}
	return out
}

func utteranceScore(s domain.SpeechSegment) int {
	n := len([]rune(s.Text))
	if s.EOU {
		n += 1_000_000
	}
	return n
}

func quantMs(seconds float64) int64 {
	if seconds < 0 {
		return 0
	}
	return int64(seconds*1000 + 0.5)
}

// dedupeIdenticalMultiChannel removes duplicate lines produced for stereo dual-mono (same text and time on two channels).
func dedupeIdenticalMultiChannel(in []domain.SpeechSegment) []domain.SpeechSegment {
	type key struct {
		startMs int64
		endMs   int64
		hash    string
	}

	best := make(map[key]domain.SpeechSegment)
	order := make([]key, 0)

	for _, seg := range in {
		seg.Text = strings.TrimSpace(seg.Text)
		if seg.Text == "" {
			continue
		}
		k := key{quantMs(seg.Start), quantMs(seg.End), normalizedTextHash(seg.Text)}
		prev, ok := best[k]
		if !ok {
			order = append(order, k)
			best[k] = seg
			continue
		}
		if utteranceScore(seg) > utteranceScore(prev) {
			best[k] = seg
		} else if utteranceScore(seg) == utteranceScore(prev) && seg.Channel < prev.Channel {
			best[k] = seg
		}
	}

	out := make([]domain.SpeechSegment, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

func normalizedTextHash(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")

	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func formatSpeakerTranscript(segments []domain.SpeechSegment) string {
	var b strings.Builder

	for _, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}

		speaker := strings.TrimSpace(seg.Speaker)
		if speaker == "" {
			speaker = "Спикер 1"
		}

		if b.Len() > 0 {
			b.WriteString("\n\n")
		}

		b.WriteString(formatTime(seg.Start))
		b.WriteString("\n")
		b.WriteString(speaker)
		b.WriteString("\n")
		b.WriteString(text)
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
		s := strings.TrimSpace(x)
		s = strings.TrimSuffix(s, "s")

		var f float64
		_, _ = fmt.Sscanf(s, "%f", &f)
		return f

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
		"audio_encoding":         enc,
		"language":               c.cfg.Language,
		"model":                  c.cfg.Model,
		"sample_rate":            c.cfg.SampleRate,
		"channels_count":         c.cfg.ChannelsCount,
		"hypotheses_count":       c.cfg.HypothesesCount,
		"enable_partial_results": c.cfg.EnablePartialResults,
	}

	if c.cfg.SpeakerSeparationEnabled {
		options["speaker_separation_options"] = map[string]any{
			"enable":                   true,
			"enable_only_main_speaker": false,
			"count":                    c.cfg.SpeakersCount,
		}
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
