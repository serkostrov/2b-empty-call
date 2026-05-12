package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/domain"
)

type Transcriber interface {
	Transcribe(ctx context.Context, audio domain.AudioFile) (domain.ASRResult, error)
}

type Summarizer interface {
	Summarize(ctx context.Context, transcription string) (domain.SummaryResult, error)
}

type Processor struct {
	transcriber Transcriber
	summarizer  Summarizer
	sem         chan struct{}
}

func NewProcessor(transcriber Transcriber, summarizer Summarizer, concurrency int) *Processor {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Processor{transcriber: transcriber, summarizer: summarizer, sem: make(chan struct{}, concurrency)}
}

func (p *Processor) Process(ctx context.Context, audio domain.AudioFile) (domain.ProcessResult, error) {
	if err := validateAudio(audio); err != nil {
		return domain.ProcessResult{}, err
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return domain.ProcessResult{}, ctx.Err()
	}

	started := time.Now()
	asr, err := p.transcriber.Transcribe(ctx, audio)
	if err != nil {
		return domain.ProcessResult{}, fmt.Errorf("transcribe: %w", err)
	}

	summary, err := p.summarizer.Summarize(ctx, asr.Text)
	if err != nil {
		return domain.ProcessResult{}, fmt.Errorf("summarize: %w", err)
	}

	return domain.ProcessResult{
		Transcription: asr.Text,
		Summary:       summary.Summary,
		RawASR:        asr.Raw,
		RawGigaChat:   summary.Raw,
		Duration:      time.Since(started),
	}, nil
}

// TranscribeOnly runs ASR under the same concurrency limit as Process.
func (p *Processor) TranscribeOnly(ctx context.Context, audio domain.AudioFile) (domain.ASRResult, error) {
	if err := validateAudio(audio); err != nil {
		return domain.ASRResult{}, err
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return domain.ASRResult{}, ctx.Err()
	}
	return p.transcriber.Transcribe(ctx, audio)
}

// SummarizeOnly runs the LLM step under the same concurrency limit.
func (p *Processor) SummarizeOnly(ctx context.Context, transcription string) (domain.SummaryResult, error) {
	if strings.TrimSpace(transcription) == "" {
		return domain.SummaryResult{}, errors.New("empty transcription")
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return domain.SummaryResult{}, ctx.Err()
	}
	return p.summarizer.Summarize(ctx, transcription)
}

func validateAudio(audio domain.AudioFile) error {
	if len(audio.Bytes) == 0 {
		return errors.New("empty audio file")
	}
	name := strings.ToLower(audio.Filename)
	for _, ext := range []string{".mp3", ".wav", ".ogg", ".flac"} {
		if strings.HasSuffix(name, ext) {
			return nil
		}
	}
	ct := strings.ToLower(audio.ContentType)
	if strings.HasPrefix(ct, "audio/") {
		return nil
	}
	return fmt.Errorf("unsupported audio format: %s", audio.Filename)
}
