package config

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
	_ "github.com/joho/godotenv/autoload"
)

type Config struct {
	App      AppConfig      `env-prefix:"APP_"`
	HTTP     HTTPConfig     `env-prefix:"HTTP_"`
	Worker   WorkerConfig   `env-prefix:"WORKER_"`
	Storage  StorageConfig  `env-prefix:"STORAGE_"`
	TLS      TLSConfig      `env-prefix:"TLS_"`
	Supabase SupabaseConfig `env-prefix:"SUPABASE_"`
	Sber     SberConfig
}

type AppConfig struct {
	Name     string     `env:"NAME" env-default:"call-worker"`
	Env      string     `env:"ENV" env-default:"local"`
	Version  string     `env:"VERSION" env-default:"dev"`
	LogLevel string     `env:"LOG_LEVEL" env-default:"info"`
	Level    slog.Level `env:"-"`
}

type HTTPConfig struct {
	Addr            string        `env:"ADDR" env-default:":8080"`
	ReadTimeout     time.Duration `env:"READ_TIMEOUT" env-default:"15s"`
	WriteTimeout    time.Duration `env:"WRITE_TIMEOUT" env-default:"30s"`
	IdleTimeout     time.Duration `env:"IDLE_TIMEOUT" env-default:"60s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" env-default:"30s"`
}

type WorkerConfig struct {
	Enabled             bool          `env:"ENABLED" env-default:"true"`
	ID                  string        `env:"ID" env-default:""`
	PollInterval        time.Duration `env:"POLL_INTERVAL" env-default:"5s"`
	Concurrency         int           `env:"CONCURRENCY" env-default:"2"`
	JobTimeout          time.Duration `env:"JOB_TIMEOUT" env-default:"45m"`
	ClaimBatchSize      int           `env:"CLAIM_BATCH_SIZE" env-default:"1"`
	IncludeRawResponses bool          `env:"INCLUDE_RAW_RESPONSES" env-default:"false"`
}

type StorageConfig struct {
	MaxAudioBytes int64 `env:"MAX_AUDIO_BYTES" env-default:"314572800"`
}

type TLSConfig struct {
	InsecureSkipVerify bool `env:"INSECURE_SKIP_VERIFY" env-default:"false"`
}

type SupabaseConfig struct {
	URL            string `env:"URL" env-required:"true"`
	ServiceRoleKey string `env:"SERVICE_ROLE_KEY" env-required:"true"`
	Schema         string `env:"SCHEMA" env-default:"public"`
	AudioBucket    string `env:"AUDIO_BUCKET" env-default:"call-audio"`
	RawBucket      string `env:"RAW_BUCKET" env-default:"call-raw"`
	ReportsBucket  string `env:"REPORTS_BUCKET" env-default:"call-reports"`
}

type SberConfig struct {
	OAuthURL string         `env:"SBER_OAUTH_URL" env-default:"https://ngw.devices.sberbank.ru:9443/api/v2/oauth"`
	Salute   SaluteConfig   `env-prefix:"SALUTE_"`
	GigaChat GigaChatConfig `env-prefix:"GIGACHAT_"`
}

type SaluteConfig struct {
	AuthHeaderScheme string        `env:"AUTH_HEADER_SCHEME" env-default:"Basic"`
	AuthKey          string        `env:"AUTH_KEY" env-required:"true"`
	Scope            string        `env:"SCOPE" env-default:"SALUTE_SPEECH_PERS"`
	UploadURL        string        `env:"UPLOAD_URL" env-required:"true"`
	RecognizeURL     string        `env:"RECOGNIZE_URL" env-required:"true"`
	TaskStatusURL    string        `env:"TASK_STATUS_URL" env-required:"true"`
	DownloadURL      string        `env:"DOWNLOAD_URL" env-required:"true"`
	PollInterval     time.Duration `env:"POLL_INTERVAL" env-default:"5s"`
	PollTimeout      time.Duration `env:"POLL_TIMEOUT" env-default:"25m"`
	Language         string        `env:"LANGUAGE" env-default:"ru-RU"`
	Model            string        `env:"MODEL" env-default:"general"`
	// SaluteSpeech RecognitionOptions.audio_encoding (e.g. MP3, PCM_S16LE). Empty = infer from filename / Content-Type.
	AudioEncoding string `env:"AUDIO_ENCODING" env-default:""`
	// Options passed with async_recognize (see RecognitionOptions in SaluteSpeech docs / reference clients).
	SampleRate               int  `env:"SAMPLE_RATE" env-default:"16000"`
	ChannelsCount            int  `env:"CHANNELS_COUNT" env-default:"1"`
	SpeakerSeparationEnabled bool `env:"SPEAKER_SEPARATION_ENABLED" env-default:"true"`
	SpeakersCount            int  `env:"SPEAKERS_COUNT" env-default:"2"`
}

type GigaChatConfig struct {
	AuthKey string `env:"AUTH_KEY" env-required:"true"`
	// Optional; default is SBER_OAUTH_URL (same NGW endpoint as SaluteSpeech token).
	OAuthURL string `env:"OAUTH_URL" env-default:""`
	// Must match your GigaChat Studio project: GIGACHAT_API_PERS | GIGACHAT_API_B2B | GIGACHAT_API_CORP.
	Scope       string  `env:"SCOPE" env-default:"GIGACHAT_API_PERS"`
	APIURL      string  `env:"API_URL" env-required:"true"`
	Model       string  `env:"MODEL" env-default:"GigaChat"`
	Temperature float64 `env:"TEMPERATURE" env-default:"0.2"`
}

func Load() (Config, error) {
	var cfg Config
	if err := cleanenv.ReadEnv(&cfg); err != nil {
		return Config{}, fmt.Errorf("read env: %w", err)
	}
	cfg.App.Level = parseLogLevel(cfg.App.LogLevel)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Worker.Concurrency < 1 {
		return fmt.Errorf("WORKER_CONCURRENCY must be >= 1")
	}
	if c.Worker.ClaimBatchSize < 1 {
		return fmt.Errorf("WORKER_CLAIM_BATCH_SIZE must be >= 1")
	}
	if c.Storage.MaxAudioBytes <= 0 {
		return fmt.Errorf("STORAGE_MAX_AUDIO_BYTES must be > 0")
	}
	if c.Sber.Salute.SampleRate <= 0 {
		return fmt.Errorf("SALUTE_SAMPLE_RATE must be > 0")
	}
	if c.Sber.Salute.ChannelsCount <= 0 {
		return fmt.Errorf("SALUTE_CHANNELS_COUNT must be > 0")
	}
	if c.Sber.Salute.SpeakersCount < 0 {
		return fmt.Errorf("SALUTE_SPEAKERS_COUNT must be >= 0")
	}
	gcScope := strings.TrimSpace(c.Sber.GigaChat.Scope)
	switch gcScope {
	case "GIGACHAT_API_PERS", "GIGACHAT_API_B2B", "GIGACHAT_API_CORP":
	default:
		return fmt.Errorf("GIGACHAT_SCOPE must be GIGACHAT_API_PERS, GIGACHAT_API_B2B, or GIGACHAT_API_CORP (got %q); wrong value causes NGW error code 7", gcScope)
	}
	return nil
}

func (c Config) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.TLS.InsecureSkipVerify} //nolint:gosec
}

// GigaChatOAuthURL returns the token endpoint for GigaChat (override or shared NGW URL).
func (s SberConfig) GigaChatOAuthURL() string {
	if u := strings.TrimSpace(s.GigaChat.OAuthURL); u != "" {
		return u
	}
	return s.OAuthURL
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
