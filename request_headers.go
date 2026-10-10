package loom

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"golang.org/x/net/http/httpguts"
)

// ErrInvalidRequestHeaders identifies malformed, conflicting, or reserved
// extra request headers. Configuration errors do not expose header values.
var ErrInvalidRequestHeaders = errors.New("loom: invalid request headers")

// SnapshotRequestHeaders validates and copies a provider's extra HTTP headers.
// Names are case-insensitive: equal duplicates collapse, conflicting duplicates
// fail. Authentication, HTTP framing and SDK format headers are reserved; use
// APIKey and HTTPClient for their respective configuration instead. Nil or empty
// maps mean no extra headers. This common boundary is used by every chat provider.
//
// The result is independent of the caller's map. Applications may reuse their
// construction configuration, but must not mutate it concurrently with New/Build.
func SnapshotRequestHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	normalized := make(map[string]string, len(headers))
	for _, key := range slices.Sorted(maps.Keys(headers)) {
		value := headers[key]
		if !httpguts.ValidHeaderFieldName(key) {
			return nil, fmt.Errorf("%w: invalid header name", ErrInvalidRequestHeaders)
		}
		name := http.CanonicalHeaderKey(key)
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("%w: invalid value for %s", ErrInvalidRequestHeaders, name)
		}
		switch name {
		case "Authorization", "Proxy-Authorization", "Host", "Content-Type", "Accept",
			"Content-Length", "Transfer-Encoding", "Connection", "Trailer":
			return nil, fmt.Errorf("%w: %s is owned by the SDK or HTTP transport", ErrInvalidRequestHeaders, name)
		}
		if existing, ok := normalized[name]; ok && existing != value {
			return nil, fmt.Errorf("%w: conflicting values for %s", ErrInvalidRequestHeaders, name)
		}
		normalized[name] = value
	}
	return normalized, nil
}
