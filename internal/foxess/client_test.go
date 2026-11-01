package foxess

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "test-key-NOT-REAL-1234"

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTiming(0, time.Millisecond)
	return c
}

func TestSignatureUsesLiteralBackslashSeparators(t *testing.T) {
	c, _ := NewClient("https://example.test", testKey, nil)
	// md5 of: /op/v0/device/list \r\n <key> \r\n 1700000000000 (separators are
	// the four characters backslash r backslash n, as in FoxESS's own sample).
	sum := md5.Sum([]byte(`/op/v0/device/list\r\n` + testKey + `\r\n1700000000000`))
	want := hex.EncodeToString(sum[:])
	if got := c.sign("/op/v0/device/list", "1700000000000"); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
	crlf := md5.Sum([]byte("/op/v0/device/list\r\n" + testKey + "\r\n1700000000000"))
	if c.sign("/op/v0/device/list", "1700000000000") == hex.EncodeToString(crlf[:]) {
		t.Fatal("must not sign with real CR LF bytes")
	}
}

func TestRequestHeadersAndSignature(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ts := r.Header.Get("timestamp")
		sum := md5.Sum([]byte(r.URL.Path + `\r\n` + testKey + `\r\n` + ts))
		if r.Header.Get("token") != testKey || r.Header.Get("signature") != hex.EncodeToString(sum[:]) || ts == "" {
			t.Errorf("bad auth headers: %v", r.Header)
		}
		if r.Header.Get("lang") == "" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing lang/content-type: %v", r.Header)
		}
		w.Write([]byte(`{"errno":0,"result":{"total":0,"data":[]}}`))
	})
	if _, err := c.DeviceList(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRetriesAndErrors(t *testing.T) {
	tests := []struct {
		name      string
		responses []string // status|body
		wantErr   string
		wantCalls int32
	}{
		{"rate limited then ok", []string{`200|{"errno":40400,"msg":"too fast"}`, `200|{"errno":0,"result":{"total":0,"data":[]}}`}, "", 2},
		{"http 500 then ok", []string{`500|oops`, `200|{"errno":0,"result":{"total":0,"data":[]}}`}, "", 2},
		{"bad params is not retried", []string{`200|{"errno":40257,"msg":"invalid body"}`}, "errno 40257", 1},
		{"missing headers is not retried", []string{`200|{"errno":40256,"msg":"missing"}`}, "errno 40256", 1},
		{"gives up after retries", []string{`200|{"errno":40400}`}, "giving up", 5},
		{"garbage body", []string{`200|<html>`}, "unreadable response", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				i := int(atomic.AddInt32(&calls, 1)) - 1
				if i >= len(tc.responses) {
					i = len(tc.responses) - 1
				}
				status, body, _ := strings.Cut(tc.responses[i], "|")
				if status == "500" {
					w.WriteHeader(500)
				}
				w.Write([]byte(body))
			})
			_, err := c.DeviceList(context.Background())
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), testKey) {
					t.Fatalf("error leaks the API key: %v", err)
				}
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestOnlyReadOnlyEndpointsAllowed(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) })
	for _, p := range []string{"/op/v0/device/setting/set", "/op/v0/device/scheduler/enable", "/op/v0/anything", "/op/v0/device/list/../setting/set"} {
		err := c.do(context.Background(), http.MethodPost, p, nil, map[string]string{}, nil)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: err = %v, want ErrNotAllowed", p, err)
		}
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("a disallowed endpoint reached the network")
	}
}

func TestPacingSpacesCallsToOneEndpoint(t *testing.T) {
	c, _ := NewClient("https://example.test", testKey, nil)
	var waits []time.Duration
	now := time.Unix(1700000000, 0)
	c.now = func() time.Time { return now }
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	c.SetTiming(time.Second, time.Second)
	for i := 0; i < 3; i++ {
		if err := c.pace(context.Background(), "/op/v0/device/list"); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Errorf("waits = %v, want [1s 2s] (each call reserves the next slot)", waits)
	}
	waits = nil
	if err := c.pace(context.Background(), "/op/v0/device/real/query"); err != nil || len(waits) != 0 {
		t.Errorf("a different endpoint must not wait: %v %v", waits, err)
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("https://example.test", "", nil); err == nil {
		t.Error("empty key must be rejected")
	}
	if _, err := NewClient("not a url", "k", nil); err == nil {
		t.Error("bad base URL must be rejected")
	}
}
