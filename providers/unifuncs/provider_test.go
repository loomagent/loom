package unifuncs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loomagent/loom/tools/web"
)

func TestReadExtractsPublicationDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("# Report\n\nPublished: May 30, 2025\n\nBody"))
	}))
	defer server.Close()
	client := New("secret", WithEndpoint(server.URL), WithRequestInterval(0))
	document, err := client.Read(context.Background(), web.ReadRequest{URL: "https://example.com/report"})
	if err != nil {
		t.Fatal(err)
	}
	if document.PublishedAt == nil || document.Metadata["published_date_confidence"] != "high" {
		t.Fatalf("document = %+v", document)
	}
}

func TestReadRetriesTransientFailure(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "later", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	client := New("", WithEndpoint(server.URL), WithRequestInterval(0), WithMaxRetries(1), WithRetryDelays(time.Millisecond, time.Millisecond))
	_, stats, err := client.ReadWithStats(context.Background(), web.ReadRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || stats.Attempts != 2 {
		t.Fatalf("attempts=%d stats=%+v", attempts, stats)
	}
}

func TestAccountFatal(t *testing.T) {
	if !IsAccountFatal(HTTPError{StatusCode: http.StatusPaymentRequired}) {
		t.Fatal("402 should be account fatal")
	}
}
