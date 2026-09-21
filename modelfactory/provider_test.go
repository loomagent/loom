package modelfactory

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"testing"
)

func TestProviderValues(t *testing.T) {
	want := []Provider{ProviderArk, ProviderDeepSeek, ProviderOpenRouter, ProviderZhipuAI}
	got := ProviderValues()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderValues() = %v, want %v", got, want)
	}

	got[0] = "changed"
	if next := ProviderValues(); !reflect.DeepEqual(next, want) {
		t.Fatalf("ProviderValues returned shared storage: %v", next)
	}
}

func TestParseProvider(t *testing.T) {
	for _, provider := range ProviderValues() {
		t.Run(provider.String(), func(t *testing.T) {
			got, err := ParseProvider(provider.String())
			if err != nil {
				t.Fatalf("ParseProvider(%q): %v", provider, err)
			}
			if got != provider || !got.Valid() {
				t.Fatalf("ParseProvider(%q) = %q, valid=%v", provider, got, got.Valid())
			}
		})
	}

	for _, value := range []string{"", "ARK", " ark", "ark ", "unknown"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			got, err := ParseProvider(value)
			if got != "" {
				t.Fatalf("ParseProvider(%q) = %q, want empty", value, got)
			}
			if !errors.Is(err, ErrInvalidProvider) {
				t.Fatalf("ParseProvider(%q) error = %v", value, err)
			}
		})
	}
}

func TestProviderTextRoundTrip(t *testing.T) {
	data, err := ProviderOpenRouter.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "openrouter" {
		t.Fatalf("MarshalText() = %q", data)
	}

	var got Provider
	if err := got.UnmarshalText(data); err != nil {
		t.Fatal(err)
	}
	if got != ProviderOpenRouter {
		t.Fatalf("UnmarshalText() = %q", got)
	}
}

func TestProviderUnmarshalTextDoesNotMutateOnError(t *testing.T) {
	got := ProviderArk
	err := got.UnmarshalText([]byte("invalid"))
	if !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("UnmarshalText error = %v", err)
	}
	if got != ProviderArk {
		t.Fatalf("UnmarshalText mutated receiver to %q", got)
	}
}

func TestProviderJSON(t *testing.T) {
	type document struct {
		Provider Provider `json:"provider"`
	}

	data, err := jsonv2.Marshal(document{Provider: ProviderDeepSeek})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"provider":"deepseek"}` {
		t.Fatalf("Marshal() = %s", data)
	}

	var got document
	if err := jsonv2.Unmarshal([]byte(`{"provider":"invalid"}`), &got); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("Unmarshal() error = %v", err)
	}
}

// The wire representation is exact: a value is accepted only as itself, so an invalid
// configuration is reported rather than quietly normalized.
func TestProviderTextIsExact(t *testing.T) {
	// An unsupported value fails rather than marshaling to something a configuration file
	// would accept.
	if _, err := Provider("nope").MarshalText(); err == nil {
		t.Fatal("an unsupported provider must not marshal")
	}
	var target Provider
	if err := target.UnmarshalText([]byte("DeepSeek")); err == nil {
		t.Fatal("case must not be folded")
	}
	if err := target.UnmarshalText([]byte(" deepseek")); err == nil {
		t.Fatal("whitespace must not be trimmed")
	}
	var absent *Provider
	if err := absent.UnmarshalText([]byte("deepseek")); err == nil {
		t.Fatal("a nil receiver must be refused")
	}
	// Every provider survives a round trip.
	for _, provider := range ProviderValues() {
		text, err := provider.MarshalText()
		if err != nil || string(text) != provider.String() {
			t.Errorf("MarshalText(%q) = %q, %v", provider, text, err)
		}
	}
}
