// Package foxess is a small read-only client for the FoxESS Cloud Open API.
package foxess

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the public FoxESS Cloud site.
const DefaultBaseURL = "https://www.foxesscloud.com"

// sigSep separates the parts of the signed string. FoxESS's own sample code
// writes it as a raw string, so this is the four characters \r\n (backslash,
// r, backslash, n), not CR LF bytes. Real CR LF is rejected as an illegal
// signature.
const sigSep = `\r\n`

const errnoRateLimited = 40400

// ErrNotAllowed is returned for any endpoint that is not a known read-only query.
var ErrNotAllowed = errors.New("endpoint is not on the read-only allowlist")

// readOnlyPaths is every endpoint this client will ever call. They are all
// queries, although the FoxESS API uses POST for some of them. Nothing here
// changes the plant's settings.
var readOnlyPaths = map[string]struct{}{
	"/op/v0/device/list":          {},
	"/op/v0/device/detail":        {},
	"/op/v0/device/real/query":    {},
	"/op/v0/device/report/query":  {},
	"/op/v0/device/history/query": {},
}

// APIError is a non-zero errno returned by FoxESS.
type APIError struct {
	Path  string
	Errno int
	Msg   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("foxess %s: errno %d: %s", e.Path, e.Errno, e.Msg)
}

type statusError struct {
	Path   string
	Status int
}

func (e *statusError) Error() string { return fmt.Sprintf("foxess %s: HTTP %d", e.Path, e.Status) }

type transportError struct{ err error }

func (e *transportError) Error() string { return "foxess request failed: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// Client performs signed requests against the query endpoints only.
type Client struct {
	base    *url.URL
	key     string
	hc      *http.Client
	retries int
	backoff time.Duration
	minGap  time.Duration // FoxESS allows one query per second per interface
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	mu   sync.Mutex
	last map[string]time.Time
}

// NewClient builds a client. The API key is sent only in the token header and
// is never included in errors or logs.
func NewClient(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("empty API key")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		base: u, key: apiKey, hc: hc,
		retries: 4, backoff: 2 * time.Second, minGap: 1100 * time.Millisecond,
		now: time.Now, sleep: sleepCtx, last: map[string]time.Time{},
	}, nil
}

// SetTiming overrides the minimum gap between calls to one endpoint and the
// retry backoff. It exists so tests don't wait in real time.
func (c *Client) SetTiming(minGap, backoff time.Duration) { c.minGap, c.backoff = minGap, backoff }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) sign(path, ts string) string {
	sum := md5.Sum([]byte(path + sigSep + c.key + sigSep + ts))
	return hex.EncodeToString(sum[:])
}

// pace reserves the next slot for this endpoint and waits for it.
func (c *Client) pace(ctx context.Context, path string) error {
	c.mu.Lock()
	var wait time.Duration
	if last, ok := c.last[path]; ok {
		if d := c.minGap - c.now().Sub(last); d > 0 {
			wait = d
		}
	}
	c.last[path] = c.now().Add(wait)
	c.mu.Unlock()
	if wait > 0 {
		return c.sleep(ctx, wait)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if _, ok := readOnlyPaths[path]; !ok {
		return fmt.Errorf("%w: %s", ErrNotAllowed, path)
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff<<(attempt-1)); err != nil {
				return err
			}
		}
		if err := c.pace(ctx, path); err != nil {
			return err
		}
		err := c.once(ctx, method, &u, path, payload, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		if !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("giving up after %d retries: %w", c.retries, lastErr)
}

func retryable(err error) bool {
	var ae *APIError
	var se *statusError
	var te *transportError
	switch {
	case errors.As(err, &ae):
		return ae.Errno == errnoRateLimited
	case errors.As(err, &se):
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	case errors.As(err, &te):
		return true
	}
	return false
}

func (c *Client) once(ctx context.Context, method string, u *url.URL, path string, payload []byte, out any) error {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(c.now().UnixMilli(), 10)
	req.Header.Set("token", c.key)
	req.Header.Set("timestamp", ts)
	req.Header.Set("signature", c.sign(path, ts))
	req.Header.Set("lang", "en")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &transportError{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{Path: path, Status: resp.StatusCode}
	}
	var env struct {
		Errno  int             `json:"errno"`
		Msg    string          `json:"msg"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("foxess %s: unreadable response: %w", path, err)
	}
	if env.Errno != 0 {
		return &APIError{Path: path, Errno: env.Errno, Msg: env.Msg}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("foxess %s: unexpected result shape: %w", path, err)
		}
	}
	return nil
}
