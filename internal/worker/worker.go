package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/domain"
	"github.com/serkostrov/2b-empty-call/internal/service"
	"github.com/serkostrov/2b-empty-call/internal/supabase"
)

type Worker struct {
	cfg       config.Config
	db        *supabase.Client
	processor *service.Processor
	logger    *slog.Logger
	workerID  string
	sem       chan struct{}
	wg        sync.WaitGroup
}

func New(cfg config.Config, db *supabase.Client, processor *service.Processor, logger *slog.Logger) *Worker {
	workerID := cfg.Worker.ID
	if strings.TrimSpace(workerID) == "" {
		workerID = fmt.Sprintf("%s-%d", cfg.App.Name, time.Now().UnixNano())
	}
	return &Worker{
		cfg: cfg, db: db, processor: processor, logger: logger.With("worker_id", workerID), workerID: workerID,
		sem: make(chan struct{}, cfg.Worker.Concurrency),
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if !w.cfg.Worker.Enabled {
		w.logger.Info("worker disabled")
		<-ctx.Done()
		return ctx.Err()
	}

	w.logger.Info("worker started", "concurrency", w.cfg.Worker.Concurrency, "poll_interval", w.cfg.Worker.PollInterval)
	ticker := time.NewTicker(w.cfg.Worker.PollInterval)
	defer ticker.Stop()

	for {
		if err := w.claimAvailable(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("claim jobs failed", "error", err)
		}

		select {
		case <-ctx.Done():
			w.logger.Info("worker stopping")
			w.wg.Wait()
			w.logger.Info("worker stopped")
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) claimAvailable(ctx context.Context) error {
	for i := 0; i < w.cfg.Worker.ClaimBatchSize; i++ {
		select {
		case w.sem <- struct{}{}:
			// slot acquired
		default:
			return nil
		}

		job, err := w.db.ClaimJob(ctx, w.workerID)
		if err != nil {
			<-w.sem
			return err
		}
		if job == nil {
			<-w.sem
			return nil
		}

		w.wg.Add(1)
		go func(job domain.ProcessingJob) {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			w.processClaimedJob(ctx, job)
		}(*job)
	}
	return nil
}

func (w *Worker) processClaimedJob(parent context.Context, job domain.ProcessingJob) {
	ctx, cancel := context.WithTimeout(parent, w.cfg.Worker.JobTimeout)
	defer cancel()

	log := w.logger.With("job_id", job.ID, "call_id", job.CallID, "organization_id", job.OrganizationID, "job_type", job.Type)
	log.Info("job started")

	if err := w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "job_started", "Обработка задачи началась", map[string]any{"type": job.Type}); err != nil {
		log.Warn("insert start log failed", "error", err)
	}

	var err error
	switch job.Type {
	case domain.JobTypeAnalyzeCall:
		err = w.processAnalyzeCall(ctx, log, job)
	case domain.JobTypeRegenerateReport:
		err = w.processRegenerateReport(ctx, log, job)
	default:
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	}

	if err != nil {
		code := classifyError(err)
		msg := userMessage(code, err)
		log.Error("job failed", "error", err, "code", code)
		_ = w.db.InsertLog(context.Background(), job.OrganizationID, job.CallID, job.ID, "error", code, msg, map[string]any{"error": err.Error()})
		if failErr := w.db.FailJob(context.Background(), job.ID, job.CallID, code, msg, map[string]any{"error": err.Error()}); failErr != nil {
			log.Error("mark job failed failed", "error", failErr)
		}
		return
	}

	if err := w.db.CompleteJob(ctx, job.ID); err != nil {
		log.Error("complete job failed", "error", err)
		return
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "job_success", "Обработка задачи успешно завершена", nil)
	log.Info("job completed")
}

func (w *Worker) processAnalyzeCall(ctx context.Context, log *slog.Logger, job domain.ProcessingJob) error {
	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusPreparingAudio, nil, nil); err != nil {
		return fmt.Errorf("update status preparing_audio: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "preparing_audio", "Подготовка аудиофайла", nil)

	call, err := w.db.GetCall(ctx, job.CallID)
	if err != nil {
		return fmt.Errorf("get call: %w", err)
	}

	file, err := w.db.GetOriginalAudioFile(ctx, call)
	if err != nil {
		return fmt.Errorf("get original audio: %w", err)
	}

	audioBytes, err := w.db.DownloadObject(ctx, file.StorageBucket, file.StoragePath, w.cfg.Storage.MaxAudioBytes)
	if err != nil {
		return fmt.Errorf("download audio: %w", err)
	}
	filename := fallbackStringPtr(file.OriginalFilename, filepath.Base(file.StoragePath))
	contentType := fallbackStringPtr(file.MimeType, contentTypeFromFilename(filename))

	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusRecognizing, nil, nil); err != nil {
		return fmt.Errorf("update status recognizing: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "recognizing", "Распознавание речи запущено", map[string]any{"filename": filename, "size_bytes": len(audioBytes)})

	pipelineStart := time.Now()
	asr, err := w.processor.TranscribeOnly(ctx, domain.AudioFile{Filename: filename, ContentType: contentType, Bytes: audioBytes})
	if err != nil {
		return fmt.Errorf("process audio: %w", err)
	}

	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusRecognized, nil, nil); err != nil {
		return fmt.Errorf("update status recognized: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "recognized", "Транскрибация получена", map[string]any{"duration": time.Since(pipelineStart).String()})

	rawASRBytes, _ := json.MarshalIndent(asr.Raw, "", "  ")
	rawASRPath := w.db.RawPath(job.OrganizationID, job.CallID, "asr.json")
	if err := w.db.UploadObject(ctx, w.cfg.Supabase.RawBucket, rawASRPath, "application/json", rawASRBytes, true); err != nil {
		return fmt.Errorf("upload raw asr json: %w", err)
	}
	if _, err := w.db.InsertCallFile(ctx, job.OrganizationID, job.CallID, domain.FileTypeRawASRJSON, w.cfg.Supabase.RawBucket, rawASRPath, "asr.json", "application/json", int64(len(rawASRBytes))); err != nil {
		return fmt.Errorf("insert raw asr file: %w", err)
	}

	if err := w.db.InsertTranscription(ctx, job.OrganizationID, job.CallID, asr.Text, asr.Segments, asr.Raw, w.cfg.Sber.Salute.Model); err != nil {
		return fmt.Errorf("insert transcription: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "transcription_saved", "Транскрибация и сырой ASR сохранены", nil)

	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusAnalyzing, nil, nil); err != nil {
		return fmt.Errorf("update status analyzing: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "summarizing", "Запуск суммаризации (GigaChat)", nil)

	startedLLM := time.Now()
	llm, err := w.processor.SummarizeOnly(ctx, asr.Text)
	if err != nil {
		return fmt.Errorf("process audio: summarize: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "summarized", "Summary получен", map[string]any{"duration": time.Since(startedLLM).String()})

	rawLLMBytes, _ := json.MarshalIndent(llm.Raw, "", "  ")
	rawLLMPath := w.db.RawPath(job.OrganizationID, job.CallID, "gigachat.json")
	if err := w.db.UploadObject(ctx, w.cfg.Supabase.RawBucket, rawLLMPath, "application/json", rawLLMBytes, true); err != nil {
		return fmt.Errorf("upload raw llm json: %w", err)
	}
	if _, err := w.db.InsertCallFile(ctx, job.OrganizationID, job.CallID, domain.FileTypeRawLLMJSON, w.cfg.Supabase.RawBucket, rawLLMPath, "gigachat.json", "application/json", int64(len(rawLLMBytes))); err != nil {
		return fmt.Errorf("insert raw llm file: %w", err)
	}

	if err := w.db.InsertAnalysis(ctx, job.OrganizationID, job.CallID, call.AnalysisTemplateID, llm.Summary, llm.Raw, w.cfg.Sber.GigaChat.Model); err != nil {
		return fmt.Errorf("insert analysis: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "analysis_saved", "Summary и сырой ответ LLM сохранены", nil)

	result := domain.ProcessResult{
		Transcription: asr.Text,
		Summary:       llm.Summary,
		RawASR:        asr.Raw,
		RawGigaChat:   llm.Raw,
		Duration:      time.Since(pipelineStart),
	}

	if err := w.generateReports(ctx, job, result); err != nil {
		return err
	}

	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusReportReady, nil, nil); err != nil {
		return fmt.Errorf("update status report_ready: %w", err)
	}
	return nil
}

func (w *Worker) processRegenerateReport(ctx context.Context, log *slog.Logger, job domain.ProcessingJob) error {
	log.Info("regenerate_report requested")
	_ = ctx
	return fmt.Errorf("regenerate_report is not implemented yet")
}

func (w *Worker) generateReports(ctx context.Context, job domain.ProcessingJob, result domain.ProcessResult) error {
	if err := w.db.UpdateCallStatus(ctx, job.CallID, domain.CallStatusGeneratingReport, nil, nil); err != nil {
		return fmt.Errorf("update status generating_report: %w", err)
	}
	_ = w.db.InsertLog(ctx, job.OrganizationID, job.CallID, job.ID, "info", "generating_report", "Формирование TXT/JSON отчетов", nil)

	reports := []struct {
		fileType   string
		reportType string
		filename   string
		mime       string
		body       []byte
	}{
		{domain.FileTypeTranscriptionTXT, domain.ReportTypeTranscriptionTXT, "transcription.txt", "text/plain; charset=utf-8", []byte(result.Transcription)},
		{domain.FileTypeSummaryTXT, domain.ReportTypeSummaryTXT, "summary.txt", "text/plain; charset=utf-8", []byte(result.Summary)},
	}
	full, _ := json.MarshalIndent(map[string]any{"transcription": result.Transcription, "summary": result.Summary, "raw_asr": result.RawASR, "raw_gigachat": result.RawGigaChat}, "", "  ")
	reports = append(reports, struct {
		fileType   string
		reportType string
		filename   string
		mime       string
		body       []byte
	}{domain.FileTypeFullAnalysisJSON, domain.ReportTypeFullAnalysisJSON, "full-analysis.json", "application/json", full})

	for _, report := range reports {
		objectPath := w.db.ReportPath(job.OrganizationID, job.CallID, report.filename)
		if err := w.db.UploadObject(ctx, w.cfg.Supabase.ReportsBucket, objectPath, report.mime, report.body, true); err != nil {
			return fmt.Errorf("upload report %s: %w", report.filename, err)
		}
		fileID, err := w.db.InsertCallFile(ctx, job.OrganizationID, job.CallID, report.fileType, w.cfg.Supabase.ReportsBucket, objectPath, report.filename, report.mime, int64(len(report.body)))
		if err != nil {
			return fmt.Errorf("insert report file %s: %w", report.filename, err)
		}
		if err := w.db.InsertCallReport(ctx, job.OrganizationID, job.CallID, report.reportType, fileID); err != nil {
			return fmt.Errorf("insert call report %s: %w", report.filename, err)
		}
	}
	return nil
}

func fallbackStringPtr(v *string, fallback string) string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return fallback
	}
	return *v
}

func contentTypeFromFilename(filename string) string {
	if ct := mime.TypeByExtension(filepath.Ext(filename)); ct != "" {
		return ct
	}
	switch strings.ToLower(filepath.Ext(filename)) {
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

func classifyError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unsupported audio format"):
		return "unsupported_format"
	case strings.Contains(msg, "exceeds max") || strings.Contains(msg, "too large"):
		return "file_too_large"
	case strings.Contains(msg, "download audio"):
		return "storage_error"
	case strings.Contains(msg, "original audio file not found"):
		return "missing_audio_file"
	case strings.Contains(msg, "salute upload"):
		return "asr_upload_failed"
	case strings.Contains(msg, "salute task"):
		return "asr_task_failed"
	case strings.Contains(msg, "polling timeout") || strings.Contains(msg, "deadline exceeded"):
		return "asr_timeout"
	case strings.Contains(msg, "gigachat"):
		return "llm_failed"
	case strings.Contains(msg, "upload report") || strings.Contains(msg, "insert report"):
		return "report_generation_failed"
	default:
		return "processing_failed"
	}
}

func userMessage(code string, err error) string {
	switch code {
	case "unsupported_format":
		return "Формат файла не поддерживается или аудио не удалось прочитать"
	case "file_too_large":
		return "Файл превышает допустимый размер"
	case "storage_error":
		return "Ошибка чтения или сохранения файла"
	case "missing_audio_file":
		return "Для звонка не найден аудиофайл в базе (проверьте call_files и поле calls.audio_file_id)"
	case "asr_upload_failed":
		return "Не удалось отправить файл на распознавание"
	case "asr_task_failed":
		return "Сервис распознавания не смог обработать файл"
	case "asr_timeout":
		return "Истекло время ожидания результата распознавания"
	case "llm_failed":
		return "Не удалось сформировать summary звонка"
	case "report_generation_failed":
		return "Не удалось сформировать файлы отчета"
	default:
		return "Обработка завершилась с ошибкой: " + err.Error()
	}
}
