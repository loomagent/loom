package sourceregistry

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NormalizeURL returns a conservative canonical key. It removes fragments,
// lowercases scheme and host, removes HTTP default ports, and treats an empty
// HTTP path and "/" as equivalent. Query parameters are deliberately preserved
// in their original order because reordering can change application semantics.
// It does not remove tracking parameters, resolve dot segments, normalize
// percent encoding or IDNs, or equate HTTP with HTTPS. Use WithNormalizer for
// application-specific canonicalization.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		if scheme, rest, ok := splitNonHTTPURI(raw); ok {
			if fragment := strings.IndexByte(rest, '#'); fragment >= 0 {
				rest = rest[:fragment]
			}
			return strings.ToLower(scheme) + ":" + rest, nil
		}
		return "", fmt.Errorf("%w: %q", ErrInvalidURL, raw)
	}
	if strings.TrimSpace(u.Scheme) == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidURL, raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment = ""
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.Host == "" || u.Hostname() == "" || u.User != nil {
			return "", fmt.Errorf("%w: absolute HTTP(S) URL required: %q", ErrInvalidURL, raw)
		}
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
			port = ""
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if port != "" {
			host = net.JoinHostPort(strings.Trim(host, "[]"), port)
		}
		u.Host = host
		if u.Path == "/" {
			u.Path = ""
		}
	} else if u.Host != "" {
		u.Host = strings.ToLower(u.Host)
	}
	key := strings.TrimSpace(u.String())
	if key == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidURL, raw)
	}
	return key, nil
}

// Domain returns a display-oriented hostname for a normalized source URL.
func Domain(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		if scheme, _, ok := splitNonHTTPURI(strings.TrimSpace(raw)); ok {
			return strings.ToLower(scheme)
		}
		return ""
	}
	if host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."); host != "" {
		return host
	}
	return strings.ToLower(u.Scheme)
}

func splitNonHTTPURI(raw string) (scheme, rest string, ok bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 {
		return "", "", false
	}
	scheme = raw[:colon]
	if strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https") || !validScheme(scheme) {
		return "", "", false
	}
	return scheme, raw[colon+1:], true
}

func validScheme(value string) bool {
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(i > 0 && ((r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.')) {
			continue
		}
		return false
	}
	return value != ""
}
