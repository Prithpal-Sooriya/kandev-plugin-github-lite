// Package githublite implements the concrete GitHub adapters behind the
// vendored source-control recipe boundary: repository listing/inspection,
// pull-request change requests, associations via Host state, and PR status
// reads — all pull-based, TTL-cached, core REST only.
package githublite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultCacheTTL bounds how long a GitHub API response is reused before the
// next identical call refetches. Every API call in this plugin goes through
// the cache; nothing refreshes on its own, so the cache is what keeps the
// plugin frugal: a task viewed twice in five minutes costs one fetch, not two.
const defaultCacheTTL = 300 * time.Second

// RateLimitSleepCap bounds the single backoff sleep the client takes when
// GitHub answers 403/429 rate-limited. Primary (per-hour) limits report a
// reset that may legitimately be an hour away; waiting that long inside a
// user-initiated request is worse than failing fast, so the cap is short and
// the caller surfaces the error. A var (not a const) so tests can shrink it.
var RateLimitSleepCap = 10 * time.Second

// clientCacheEntry is one cached response body, keyed by method+path+body.
type clientCacheEntry struct {
	body     []byte
	fetchURI time.Time
}

// client is a minimal GitHub REST client: PAT bearer auth, a per-process TTL
// cache, and best-effort rate-limit backoff with a single retry. It contains
// no retries beyond that, no telemetry, and no background work.
type client struct {
	http    *http.Client
	token   string
	baseURL string
	ttl     time.Duration

	mu    sync.Mutex
	cache map[string]clientCacheEntry
}

func newClient(token, baseURL string, ttl time.Duration) *client {
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &client{
		http:    &http.Client{Timeout: 30 * time.Second},
		token:   strings.TrimSpace(token),
		baseURL: baseURL,
		ttl:     ttl,
		cache:   make(map[string]clientCacheEntry),
	}
}

// authenticated reports whether a token is configured.
func (c *client) authenticated() bool { return c.token != "" }

// getJSON performs a cached GET, unmarshaling the response into out.
// A 404 returns (nil, nil) — a legitimate "does not exist" the adapters
// turn into empty results, never into link cleanup.
func (c *client) getJSON(ctx context.Context, path string, out any) error {
	body, err := c.cached(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if body == nil {
		return nil // 404: leave out untouched
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("github-lite: decoding %s: %w", path, err)
		}
	}
	return nil
}

// postJSON performs an uncached POST (mutations are never cached) and
// unmarshals the response into out.
func (c *client) postJSON(ctx context.Context, path string, payload, out any) error {
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("github-lite: encoding %s payload: %w", path, err)
		}
		body = encoded
	}
	response, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if out != nil && response != nil {
		if err := json.Unmarshal(response, out); err != nil {
			return fmt.Errorf("github-lite: decoding %s: %w", path, err)
		}
	}
	return nil
}

// cached serves a GET from the TTL cache when fresh, otherwise fetches once.
func (c *client) cached(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	c.mu.Lock()
	key := method + " " + path
	entry, hit := c.cache[key]
	c.mu.Unlock()
	if hit && time.Since(entry.fetchURI) < c.ttl {
		return entry.body, nil
	}
	response, err := c.do(ctx, method, path, body)
	if err != nil || response == nil {
		return response, err
	}
	c.mu.Lock()
	c.cache[key] = clientCacheEntry{body: response, fetchURI: time.Now()}
	c.mu.Unlock()
	return response, nil
}

// do performs one request with one rate-limit-aware retry. It returns a nil
// body (and nil error) for 404.
func (c *client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	response, limited, err := c.once(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if limited {
		// Single bounded backoff, then one retry. A still-limited second
		// response surfaces as the standard error.
		if err := sleepContext(ctx, RateLimitSleepCap); err != nil {
			return nil, err
		}
		return c.onceRetry(ctx, method, path, body)
	}
	return response, nil
}

// onceRetry is the single post-backoff retry; a second rate-limit answer
// is an error rather than another wait.
func (c *client) onceRetry(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	response, limited, err := c.once(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if limited {
		return nil, fmt.Errorf("github-lite: %s is rate-limited; retry shortly", path)
	}
	return response, nil
}

// once performs exactly one HTTP round trip. A rate-limited 403/429 returns
// (nil, true, nil): retryable, not yet an error.
func (c *client) once(ctx context.Context, method, path string, body []byte) ([]byte, bool, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("github-lite: building %s request: %w", path, err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("github-lite: requesting %s: %w", path, err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, response.Body)
		return nil, false, nil
	case response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests:
		_, _ = io.Copy(io.Discard, response.Body)
		if response.Header.Get("X-RateLimit-Remaining") == "0" || response.StatusCode == http.StatusTooManyRequests {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("github-lite: %s responded %d (forbidden); check the token's repository access", path, response.StatusCode)
	case response.StatusCode == http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, response.Body)
		return nil, false, fmt.Errorf("github-lite: %s responded 401 (unauthorized); check the configured GitHub access token", path)
	case response.StatusCode >= 400:
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, false, fmt.Errorf("github-lite: %s responded %d: %s", path, response.StatusCode, strings.TrimSpace(string(snippet)))
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, false, fmt.Errorf("github-lite: reading %s response: %w", path, err)
	}
	return content, false, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
