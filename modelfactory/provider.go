package modelfactory

import (
	"encoding"
	"errors"
	"fmt"
)

// ErrInvalidProvider reports an empty or unsupported Provider value.
var ErrInvalidProvider = errors.New("modelfactory: invalid provider")

// Provider identifies a built-in model provider.
//
// Provider is a string so values remain readable in configuration files and
// wire formats. Treat values outside the constants below as invalid; use
// ParseProvider when converting untrusted strings.
type Provider string

const (
	ProviderArk        Provider = "ark"
	ProviderDeepSeek   Provider = "deepseek"
	ProviderOpenRouter Provider = "openrouter"
)

var providerValues = [...]Provider{
	ProviderArk,
	ProviderDeepSeek,
	ProviderOpenRouter,
}

var (
	_ encoding.TextMarshaler   = Provider("")
	_ encoding.TextUnmarshaler = (*Provider)(nil)
)

// String returns the provider's wire representation.
func (p Provider) String() string {
	return string(p)
}

// Valid reports whether p is one of the providers supported by Build.
func (p Provider) Valid() bool {
	switch p {
	case ProviderArk, ProviderDeepSeek, ProviderOpenRouter:
		return true
	default:
		return false
	}
}

// ProviderValues returns all supported providers in stable order.
// The returned slice is a copy and may be modified by the caller.
func ProviderValues() []Provider {
	values := make([]Provider, len(providerValues))
	copy(values, providerValues[:])
	return values
}

// ParseProvider parses the exact, case-sensitive wire representation of a
// Provider. It intentionally performs no trimming or case folding so invalid
// configuration is reported instead of silently normalized.
func ParseProvider(value string) (Provider, error) {
	p := Provider(value)
	if !p.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidProvider, value)
	}
	return p, nil
}

// MarshalText implements encoding.TextMarshaler.
func (p Provider) MarshalText() ([]byte, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidProvider, p)
	}
	return []byte(p), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *Provider) UnmarshalText(text []byte) error {
	if p == nil {
		return fmt.Errorf("%w: nil receiver", ErrInvalidProvider)
	}
	parsed, err := ParseProvider(string(text))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}
