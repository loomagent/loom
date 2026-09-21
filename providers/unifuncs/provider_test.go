package unifuncs

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/loomagent/loom/tools/web"
)

// testClient points a client at an in-memory test server, which keeps HTTP traffic on the
// memory network so a synctest bubble can drive the retry and throttle logic without
// waiting on a real clock. The API key is a placeholder: the fake server checks the header
// it receives, not the value.
//
// The caller's options come last, so a test that cares about a limit sets it itself.
func testClient(t *testing.T, handler http.HandlerFunc, options ...Option) *Client {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	options = append([]Option{
		WithEndpoint(server.URL),
		WithHTTPClient(server.Client()),
		WithRequestInterval(0),
	}, options...)
	return New("secret", options...)
}

func TestReadMapsDocument(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := jsonv2.UnmarshalDecode(jsontext.NewDecoder(r.Body), &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// The read timeout travels in the body as well as in the request context, because
		// the endpoint enforces its own.
		if body["url"] != "https://example.com/report" || body["format"] != "md" ||
			body["readTimeout"] != float64(defaultTimeout.Milliseconds()) {
			t.Errorf("request = %#v", body)
		}
		_, _ = w.Write([]byte("# Report\n\nPublished: May 30, 2025\n\nBody"))
	})

	document, err := client.Read(context.Background(), web.ReadRequest{URL: "https://example.com/report"})
	if err != nil {
		t.Fatal(err)
	}
	if document.URL != "https://example.com/report" || document.ContentType != "text/markdown" ||
		!strings.Contains(document.Markdown, "Body") {
		t.Fatalf("document = %+v", document)
	}
	if document.Metadata[web.MetadataProvider] != "unifuncs" || document.Metadata["attempts"] != "1" ||
		document.Metadata["duration_ms"] == "" {
		t.Fatalf("metadata = %+v", document.Metadata)
	}
	if document.PublishedAt == nil || document.Metadata[web.DocumentMetadataPublishedDateConfidence] != "high" {
		t.Fatalf("publication date = %+v", document)
	}
}

// The endpoint takes a URL, so anything that is not an absolute HTTP(S) one is refused
// before a request is made.
func TestReadRejectsAnythingButAnAbsoluteURL(t *testing.T) {
	client := testClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an invalid URL must not reach the endpoint")
	})
	for _, target := range []string{"", "   ", "/report", "example.com/report", "ftp://example.com", "https://"} {
		if _, _, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: target}); err == nil ||
			!strings.Contains(err.Error(), "absolute HTTP(S) URL is required") {
			t.Fatalf("ReadWithStats(%q) error = %v", target, err)
		}
	}
}

// A refused account cannot be retried into working, so the client gives up after one
// attempt however many it is allowed.
func TestReadGivesUpOnAccountFailures(t *testing.T) {
	attempts := 0
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte("insufficient balance"))
	}, WithMaxRetries(3))

	_, stats, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: "https://example.com"})
	if _, ok := errors.AsType[HTTPError](err); !ok {
		t.Fatalf("error = %T %v", err, err)
	}
	if !IsAccountFatal(err) {
		t.Fatal("402 must be account fatal")
	}
	if attempts != 1 || stats.Attempts != 1 || stats.MaxRetries != 3 {
		t.Fatalf("attempts=%d stats=%+v", attempts, stats)
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("error = %v", err)
	}
}

// A 5xx is retried, and the wait before each try is the backoff, which doubles.
func TestReadBacksOffBeforeRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if attempts < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}, WithMaxRetries(2), WithRetryDelays(time.Second, time.Minute))

		markdown, stats, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: "https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if markdown != "ok" || attempts != 3 || stats.Attempts != 3 {
			t.Fatalf("markdown=%q attempts=%d stats=%+v", markdown, attempts, stats)
		}
		// One second before the second attempt, two more before the third.
		if stats.Duration != 3*time.Second {
			t.Fatalf("duration = %s, want 3s of virtual time", stats.Duration)
		}
	})
}

// A Retry-After header is the endpoint telling us when to come back, so it wins over the
// backoff.
func TestReadHonoursRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if attempts == 1 {
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}, WithMaxRetries(1), WithRetryDelays(time.Second, time.Minute))

		_, stats, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: "https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if attempts != 2 || stats.Attempts != 2 {
			t.Fatalf("attempts=%d stats=%+v", attempts, stats)
		}
		if stats.Duration != 5*time.Second {
			t.Fatalf("duration = %s, want the header's 5s", stats.Duration)
		}
	})
}

// Cancelling the call ends it even in the middle of a backoff wait, rather than making the
// caller wait for a timer it no longer wants.
func TestReadStopsWhenCancelledDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, WithMaxRetries(3), WithRetryDelays(time.Hour, time.Hour))

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			synctest.Sleep(time.Second)
			cancel()
		}()
		_, stats, err := client.ReadWithStats(ctx, web.ReadRequest{URL: "https://example.com"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		if stats.Attempts != 1 {
			t.Fatalf("stats = %+v", stats)
		}
	})
}

// The read timeout bounds one attempt, and the endpoint is told about it.
func TestReadTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			// Answer far too late: the caller's own deadline fires first.
			synctest.Sleep(5 * time.Second)
			_, _ = w.Write([]byte("late"))
		}, WithReadTimeout(time.Second), WithMaxRetries(0))

		_, stats, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: "https://example.com"})
		if err == nil {
			t.Fatal("a read that outlives its timeout must fail")
		}
		if !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("error = %v", err)
		}
		if stats.Attempts != 1 || stats.Timeout != time.Second {
			t.Fatalf("stats = %+v", stats)
		}
	})
}

// Requests are spaced by the interval, so a caller can stay inside an endpoint's rate
// limit without a sleep of its own.
func TestReadSpacesRequestsByTheInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}, WithRequestInterval(time.Second))

		start := time.Now()
		for range 2 {
			if _, err := client.Read(context.Background(), web.ReadRequest{URL: "https://example.com"}); err != nil {
				t.Fatal(err)
			}
		}
		// The first request goes out at once; the second waits out the interval.
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("elapsed = %s, want one interval", elapsed)
		}
	})
}

func TestParseRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		tests := map[string]time.Duration{
			"":     0,
			"  ":   0,
			"5":    5 * time.Second,
			"0":    0,
			"-3":   0,
			"soon": 0,
			// An HTTP date is the other form the header may take, and one already past
			// means "now", not a negative wait.
			now.Add(10 * time.Second).UTC().Format(http.TimeFormat): 10 * time.Second,
			now.Add(-time.Minute).UTC().Format(http.TimeFormat):     0,
		}
		for value, want := range tests {
			if got := parseRetryAfter(value); got != want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", value, got, want)
			}
		}
	})
}

func TestHTTPErrorTruncatesTheBody(t *testing.T) {
	long := HTTPError{StatusCode: http.StatusBadGateway, Body: strings.Repeat("b", 501)}
	if got := long.Error(); strings.Count(got, "b") != 500 || !strings.Contains(got, "HTTP 502") {
		t.Fatalf("message = %q", got)
	}
	if got := (HTTPError{StatusCode: http.StatusForbidden, Body: "forbidden"}).Error(); got != "unifuncs: HTTP 403: forbidden" {
		t.Fatalf("message = %q", got)
	}
}

func TestIsAccountFatal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "unauthorized", err: HTTPError{StatusCode: http.StatusUnauthorized}, want: true},
		{name: "payment required", err: HTTPError{StatusCode: http.StatusPaymentRequired}, want: true},
		{name: "wrapped", err: errors.Join(errors.New("read"), HTTPError{StatusCode: http.StatusUnauthorized}), want: true},
		{name: "rate limited", err: HTTPError{StatusCode: http.StatusTooManyRequests}},
		{name: "transport failure", err: &net.OpError{Op: "dial"}},
		{name: "nil", err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAccountFatal(tt.err); got != tt.want {
				t.Fatalf("IsAccountFatal(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

// A retry budget large enough to overflow a doubling must still wait. A negative delay
// fires a timer at once, so the retry schedule would become a hot loop against an endpoint
// that already asked us to slow down.
func TestBackoffClampsInsteadOfOverflowing(t *testing.T) {
	client := New("", WithRetryDelays(time.Second, 30*time.Second))
	for _, attempt := range []int{0, 1, 5, 10, 33, 34, 40, 63, 1000} {
		delay := client.backoff(attempt)
		if delay <= 0 || delay > 30*time.Second {
			t.Fatalf("backoff(%d) = %s", attempt, delay)
		}
	}
	if got := client.backoff(-1); got != time.Second {
		t.Fatalf("backoff(-1) = %s, want the initial delay", got)
	}
	// The schedule still doubles while it is below the ceiling.
	client = New("", WithRetryDelays(time.Second, time.Minute))
	if got := client.backoff(2); got != 4*time.Second {
		t.Fatalf("backoff(2) = %s, want 4s", got)
	}
}
