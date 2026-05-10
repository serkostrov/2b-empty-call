package gigachat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/domain"
	"github.com/serkostrov/2b-empty-call/internal/providers"
)

type Client struct {
	cfg   config.GigaChatConfig
	oauth *providers.OAuthClient
	http  *http.Client
}

func New(cfg config.SberConfig, httpClient *http.Client) *Client {
	return &Client{
		cfg:  cfg.GigaChat,
		http: httpClient,
		oauth: &providers.OAuthClient{
			HTTPClient: httpClient,
			TokenURL:   cfg.OAuthURL,
			AuthScheme: "Basic",
			AuthKey:    cfg.GigaChat.AuthKey,
			Scope:      cfg.GigaChat.Scope,
		},
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *Client) Summarize(ctx context.Context, transcription string) (domain.SummaryResult, error) {
	token, err := c.oauth.Token(ctx)
	if err != nil {
		return domain.SummaryResult{}, fmt.Errorf("gigachat auth: %w", err)
	}

	payload := chatRequest{
		Model:       c.cfg.Model,
		Temperature: c.cfg.Temperature,
		Messages: []chatMessage{
			{Role: "system", Content: "Ты профессиональный аналитик клиентских звонков. Отвечай строго на русском языке."},
			{Role: "user", Content: buildPrompt(transcription)},
		},
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(b))
	if err != nil {
		return domain.SummaryResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.SummaryResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.SummaryResult{}, fmt.Errorf("gigachat completion failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.SummaryResult{}, fmt.Errorf("decode raw gigachat: %w", err)
	}
	var typed chatResponse
	if err := json.Unmarshal(body, &typed); err != nil {
		return domain.SummaryResult{Raw: raw}, fmt.Errorf("decode gigachat response: %w", err)
	}
	if len(typed.Choices) == 0 || strings.TrimSpace(typed.Choices[0].Message.Content) == "" {
		return domain.SummaryResult{Raw: raw}, fmt.Errorf("empty gigachat completion")
	}
	return domain.SummaryResult{Summary: strings.TrimSpace(typed.Choices[0].Message.Content), Raw: raw}, nil
}

func buildPrompt(transcription string) string {
	return `Сделай краткое summary транскрибации звонка.

Верни понятный текст без Markdown.

Структура ответа:
1. О чем был звонок.
2. Что хотел клиент.
3. Что предложил менеджер.
4. Какие договоренности были достигнуты.
5. Какие следующие шаги.
6. Какие риски или открытые вопросы остались.

Если данных по пункту нет, напиши, что информация не зафиксирована.

Транскрибация:
` + transcription
}
