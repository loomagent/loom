package serper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loomagent/loom/tools/web"
)

func TestSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "secret" {
			t.Fatalf("API key header = %q", r.Header.Get("X-API-KEY"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["q"] != "loom agents" || request["num"] != float64(3) {
			t.Fatalf("request = %#v", request)
		}
		_, _ = w.Write([]byte(`{"organic":[{"title":"Loom","link":"https://example.com","snippet":"Agent runtime","date":"May 30, 2025","position":1}]}`))
	}))
	defer server.Close()

	client := New("secret", WithEndpoint(server.URL))
	response, err := client.Search(context.Background(), web.SearchRequest{Query: "loom agents", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Metadata["date_source"] != "serper.organic.date" {
		t.Fatalf("response = %+v", response)
	}
}

func TestSearchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := New("", WithEndpoint(server.URL)).Search(context.Background(), web.SearchRequest{Query: "x"})
	if _, ok := err.(HTTPError); !ok {
		t.Fatalf("error = %T %v", err, err)
	}
}
