package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/uuid"
)

type OAuthClient struct {
	HTTPClient   *http.Client
	TokenURL     string
	AuthScheme   string
	AuthKey      string
	Scope        string
	EarlyRefresh time.Duration

	mu    sync.Mutex
	token string
	exp   time.Time
}

type oauthResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

func (c *OAuthClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	early := c.EarlyRefresh
	if early == 0 {
		early = time.Minute
	}
	if c.token != "" && time.Now().Before(c.exp.Add(-early)) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("scope", c.Scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("RqUID", uuid.NewString())
	req.Header.Set("Authorization", c.AuthScheme+" "+c.AuthKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth token failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out oauthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode oauth response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("oauth response does not contain access_token")
	}

	c.token = out.AccessToken
	if out.ExpiresAt > 0 {
		c.exp = time.Unix(out.ExpiresAt, 0)
	} else {
		c.exp = time.Now().Add(25 * time.Minute)
	}
	return c.token, nil
}
