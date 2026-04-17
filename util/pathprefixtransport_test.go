package util

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingTransport captures the URL.Path of every request it is asked to
// round-trip so tests can assert on the rewrite behaviour without spinning
// up an httptest.Server.
type recordingTransport struct {
	paths []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.paths = append(r.paths, req.URL.Path)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func roundTripGet(t *testing.T, rt http.RoundTripper, rawURL string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	_, err = rt.RoundTrip(req)
	require.NoError(t, err)
}

// TestPathPrefixTransport_RewritesV2 proves the transport prepends the
// configured prefix to /v2/ requests. This is the primary contract.
func TestPathPrefixTransport_RewritesV2(t *testing.T) {
	inner := &recordingTransport{}
	rt := NewPathPrefixTransport("docker", inner)

	roundTripGet(t, rt, "https://example.com/v2/")
	roundTripGet(t, rt, "https://example.com/v2/_catalog")
	roundTripGet(t, rt, "https://example.com/v2/app/svc/tags/list")

	require.Equal(t, []string{
		"/docker/v2/",
		"/docker/v2/_catalog",
		"/docker/v2/app/svc/tags/list",
	}, inner.paths)
}

// TestPathPrefixTransport_EmptyPrefix proves the transport is a pass-through
// when no prefix is configured. Every non-prefixed deployment must go through
// this transport unchanged so the chain can be wired unconditionally.
func TestPathPrefixTransport_EmptyPrefix(t *testing.T) {
	inner := &recordingTransport{}
	rt := NewPathPrefixTransport("", inner)

	roundTripGet(t, rt, "https://example.com/v2/_catalog")

	require.Equal(t, []string{"/v2/_catalog"}, inner.paths)
}

// TestPathPrefixTransport_SlashNormalisation proves leading/trailing slashes
// in the configured prefix are trimmed. Users passing -registry
// "http://host/docker/" or "http://host//docker" must not produce double
// slashes like /docker//v2/.
func TestPathPrefixTransport_SlashNormalisation(t *testing.T) {
	for _, prefix := range []string{"docker", "/docker", "docker/", "/docker/", "//docker//"} {
		inner := &recordingTransport{}
		rt := NewPathPrefixTransport(prefix, inner)

		roundTripGet(t, rt, "https://example.com/v2/_catalog")

		require.Equal(t, []string{"/docker/v2/_catalog"}, inner.paths,
			"prefix %q normalised wrong", prefix)
	}
}

// TestPathPrefixTransport_Idempotent proves the transport does not
// double-prepend when a request URL already carries the prefix. This matters
// for HTTP redirects: if the server responds with `Location: /docker/v2/foo`
// and the stdlib follows the redirect through this transport, we must not
// rewrite that to `/docker/docker/v2/foo`.
func TestPathPrefixTransport_Idempotent(t *testing.T) {
	inner := &recordingTransport{}
	rt := NewPathPrefixTransport("docker", inner)

	roundTripGet(t, rt, "https://example.com/docker/v2/_catalog")
	roundTripGet(t, rt, "https://example.com/docker/v2/app/svc/tags/list")

	require.Equal(t, []string{
		"/docker/v2/_catalog",
		"/docker/v2/app/svc/tags/list",
	}, inner.paths)
}

// TestPathPrefixTransport_LeavesNonV2Alone proves the transport only
// rewrites paths starting with /v2/. Auth token endpoints, /health, and
// any other sidecar URL must pass through unchanged to avoid breaking
// registries that host auth at a fixed path outside the sub-mount.
func TestPathPrefixTransport_LeavesNonV2Alone(t *testing.T) {
	inner := &recordingTransport{}
	rt := NewPathPrefixTransport("docker", inner)

	roundTripGet(t, rt, "https://auth.example.com/token")
	roundTripGet(t, rt, "https://example.com/healthz")
	roundTripGet(t, rt, "https://example.com/v1/something-old")

	require.Equal(t, []string{
		"/token",
		"/healthz",
		"/v1/something-old",
	}, inner.paths)
}

// TestPathPrefixTransport_PreservesRequest proves the rewrite happens on a
// clone of the request, not the caller's request. This matters because
// go-containerregistry and the stdlib retry logic may hold a pointer to
// the original request and resend it; mutating the Path in place would
// cause the second send to produce /docker/docker/v2/... (the test above
// proves the Idempotent guard would catch it, but defense in depth is
// cheap).
func TestPathPrefixTransport_PreservesRequest(t *testing.T) {
	inner := &recordingTransport{}
	rt := NewPathPrefixTransport("docker", inner)

	u, err := url.Parse("https://example.com/v2/_catalog")
	require.NoError(t, err)
	req := &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}

	_, err = rt.RoundTrip(req)
	require.NoError(t, err)

	require.Equal(t, "/v2/_catalog", req.URL.Path,
		"caller's request must not be mutated")
	require.Equal(t, []string{"/docker/v2/_catalog"}, inner.paths)
}

// TestPathPrefixTransport_EndToEnd spins up a real httptest.Server and proves
// a GET through the transport chain actually reaches the server at the
// prefixed path. This catches bugs that unit-level recordingTransport
// cannot see, like URL escaping or Host header handling.
func TestPathPrefixTransport_EndToEnd(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := NewPathPrefixTransport("docker", http.DefaultTransport)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/docker/v2/_catalog", gotPath)
}
