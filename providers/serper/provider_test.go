package serper

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loomagent/loom/tools/web"
)

// testClient points a client at an in-memory test server. The API key is a placeholder:
// the fake server checks the header it receives, not the value.
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	return New("secret", WithEndpoint(server.URL), WithHTTPClient(server.Client()))
}

func TestSearchMapsResults(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "secret" {
			t.Errorf("API key header = %q", r.Header.Get("X-API-KEY"))
		}
		var request map[string]any
		if err := jsonv2.UnmarshalDecode(jsontext.NewDecoder(r.Body), &request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request["q"] != "loom agents" || request["num"] != float64(3) {
			t.Errorf("request = %#v", request)
		}
		_, _ = w.Write([]byte(`{"organic":[
			{"title":"Loom","link":"https://example.com","snippet":"Agent runtime","date":"May 30, 2025","position":1},
			{"title":"Other","link":"https://example.org","snippet":"No metadata"}
		]}`))
	})

	response, err := client.Search(context.Background(), web.SearchRequest{Query: "  loom agents  ", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if response.Metadata[web.MetadataProvider] != "serper" || len(response.Results) != 2 {
		t.Fatalf("response = %+v", response)
	}
	first := response.Results[0]
	if first.Title != "Loom" || first.URL != "https://example.com" || first.Date != "May 30, 2025" ||
		first.DateSource != "serper.organic.date" || first.Position != 1 {
		t.Fatalf("first result = %+v", first)
	}
	if first.Metadata[web.SearchMetadataDate] != "May 30, 2025" ||
		first.Metadata[web.SearchMetadataDateSource] != "serper.organic.date" ||
		first.Metadata[web.SearchMetadataPosition] != "1" {
		t.Fatalf("first metadata = %+v", first.Metadata)
	}
	// A result the provider gave nothing extra for carries no metadata at all, rather than
	// an empty map a consumer would have to check.
	if second := response.Results[1]; second.Metadata != nil || second.DateSource != "" || second.Position != 0 {
		t.Fatalf("second result = %+v", second)
	}
}

func TestSearchRejectsABlankQuery(t *testing.T) {
	_, err := New("secret").Search(context.Background(), web.SearchRequest{Query: "   "})
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("error = %v", err)
	}
}

// The body of a failed response reaches the caller, so a long one is bounded first.
func TestSearchReportsHTTPError(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("b", 501)))
	})

	_, err := client.Search(context.Background(), web.SearchRequest{Query: "x"})
	var httpErr HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized || httpErr.Body != strings.Repeat("b", 501) {
		t.Fatalf("HTTPError = %+v", httpErr)
	}
	message := httpErr.Error()
	if !strings.Contains(message, "serper: HTTP 401") {
		t.Fatalf("message = %q", message)
	}
	if strings.Count(message, "b") != 500 {
		t.Fatalf("message carries %d body runes, want 500", strings.Count(message, "b"))
	}
}

// Serper answers with a 200 and a message when it refuses a query, so the message is the
// only signal that the search failed.
func TestSearchReportsAnAPIMessage(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Query is too short","organic":[]}`))
	})
	_, err := client.Search(context.Background(), web.SearchRequest{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "Query is too short") {
		t.Fatalf("error = %v", err)
	}
}

func TestSearchReportsUndecodableResponse(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	_, err := client.Search(context.Background(), web.SearchRequest{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("error = %v", err)
	}
}

func TestSearchReportsTransportFailure(t *testing.T) {
	client := New("secret", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}))
	_, err := client.Search(context.Background(), web.SearchRequest{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("error = %v", err)
	}
}

// A nil option and a nil client are both no-ops, so a caller can pass a client it did not
// manage to build without losing the defaults.
func TestNewKeepsDefaultsForNilOptions(t *testing.T) {
	client := New("secret", nil, WithHTTPClient(nil), WithEndpoint("   "))
	if client.http == nil || client.endpoint != defaultEndpoint {
		t.Fatalf("client = %+v", client)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// A body shorter than the limit is reported as it arrived.
func TestHTTPErrorKeepsAShortBody(t *testing.T) {
	message := HTTPError{StatusCode: http.StatusForbidden, Body: "forbidden"}.Error()
	if message != "serper: HTTP 403: forbidden" {
		t.Fatalf("message = %q", message)
	}
}
