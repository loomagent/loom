package sourceregistry

import (
	"errors"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct{ input, want string }{
		{" HTTPS://Example.COM:443/#section ", "https://example.com"},
		{"http://Example.com:80/path?q=2&q=1#x", "http://example.com/path?q=2&q=1"},
		{"https://[2001:db8::1]:443/", "https://[2001:db8::1]"},
		{"favorite://Item%3A123", "favorite://Item%3A123"},
		{"https://example.com?", "https://example.com?"},
		{"https://example.com/a%2Fb", "https://example.com/a%2Fb"},
	}
	for _, test := range tests {
		got, err := NormalizeURL(test.input)
		if err != nil || got != test.want {
			t.Fatalf("NormalizeURL(%q)=%q,%v want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"relative/path", "https://user:pass@example.com/a", "http://example.com:bad/a"} {
		if _, err := NormalizeURL(input); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("NormalizeURL(%q) error=%v", input, err)
		}
	}
}

func TestDomain(t *testing.T) {
	if got := Domain("https://www.Example.com/a"); got != "example.com" {
		t.Fatalf("Domain=%q", got)
	}
	if got := Domain("favorite://Item%3A123"); got != "favorite" {
		t.Fatalf("custom Domain=%q", got)
	}
}
