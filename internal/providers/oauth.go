package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/uuid"
)

const defaultEarlyRefresh = time.Minute

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
	ExpiresAt   int64  `json:"expires_at"` // unix timestamp in milliseconds
}

func (c *OAuthClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isTokenValid(time.Now()) {
		return c.token, nil
	}

	token, exp, err := c.requestToken(ctx)
	if err != nil {
		return "", err
	}

	c.token = token
	c.exp = exp

	return c.token, nil
}

func (c *OAuthClient) isTokenValid(now time.Time) bool {
	if c.token == "" || c.exp.IsZero() {
		return false
	}

	earlyRefresh := c.EarlyRefresh
	if earlyRefresh <= 0 {
		earlyRefresh = defaultEarlyRefresh
	}

	return now.Before(c.exp.Add(-earlyRefresh))
}

func (c *OAuthClient) requestToken(ctx context.Context) (string, time.Time, error) {
	form := url.Values{}
	form.Set("scope", strings.TrimSpace(c.Scope))

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.TokenURL,
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return "", time.Time{}, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("RqUID", uuid.NewString())
	req.Header.Set("Authorization", strings.TrimSpace(c.AuthScheme)+" "+strings.TrimSpace(c.AuthKey))

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("oauth token failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out oauthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode oauth response: %w", err)
	}

	if strings.TrimSpace(out.AccessToken) == "" {
		return "", time.Time{}, fmt.Errorf("oauth response does not contain access_token")
	}

	exp := parseExpiresAt(out.ExpiresAt)
	if exp.IsZero() {
		exp = time.Now().Add(30 * time.Minute)
	}

	return strings.TrimSpace(out.AccessToken), exp, nil
}

func parseExpiresAt(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	return time.UnixMilli(v)
}
