package util

import (
	"net/http"
	"strings"
)

// PathPrefixTransport rewrites request URLs to inject a base path in front
// of the Registry v2 endpoints, enabling deckschrubber to talk to registries
// served under a URL sub-path (e.g. https://example.com/docker/v2/...
// behind a reverse proxy). go-containerregistry always emits requests with
// `/v2/...` rooted at the host (see pkg/v1/remote/catalog.go: URL.Path is
// hardcoded to `/v2/_catalog`, and every other remote.* call follows the
// same shape), so without this transport a -registry URL carrying a path
// has its path silently dropped on the wire.
//
// The rewrite only fires for requests whose Path starts with `/v2/` — auth
// token redirects and other sidecar URLs are left alone. Idempotent: if the
// prefix is already present (e.g. the server rewrote the URL during a
// redirect), the path is passed through unchanged.
//
// Prefix is stored without leading/trailing slashes; the Rewrite always
// inserts `/<prefix>` in front of the existing `/v2/...`.
type PathPrefixTransport struct {
	prefix    string
	transport http.RoundTripper
}

func (t *PathPrefixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.prefix != "" && strings.HasPrefix(req.URL.Path, "/v2/") {
		prefixed := "/" + t.prefix + req.URL.Path
		if !strings.HasPrefix(req.URL.Path, "/"+t.prefix+"/") {
			req2 := req.Clone(req.Context())
			req2.URL.Path = prefixed
			if req2.URL.RawPath != "" {
				req2.URL.RawPath = "/" + t.prefix + req2.URL.RawPath
			}
			return t.transport.RoundTrip(req2)
		}
	}
	return t.transport.RoundTrip(req)
}

// NewPathPrefixTransport wraps inner so that every `/v2/...` request gets
// the given prefix prepended. An empty prefix turns the wrapper into a
// pass-through, which is the expected state when the -registry URL has no
// path component.
func NewPathPrefixTransport(prefix string, inner http.RoundTripper) *PathPrefixTransport {
	return &PathPrefixTransport{
		prefix:    strings.Trim(prefix, "/"),
		transport: inner,
	}
}
