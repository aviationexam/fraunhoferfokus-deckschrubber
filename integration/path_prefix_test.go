package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aviationexam/deckschrubber/util"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// mountRegistryAt serves the in-memory registry under the given URL path
// prefix (e.g. "docker"), so requests to http://host/docker/v2/... reach
// the registry's /v2/... handler. This mirrors the common reverse-proxy
// deployment where a registry is exposed at host/registry or host/docker.
//
// The handler returns 404 for any request outside /prefix/, which makes
// misrouted requests (e.g. deckschrubber forgetting the prefix) loud.
func mountRegistryAt(t *testing.T, prefix string) (baseURL, addr string) {
	t.Helper()

	inner := registry.New()
	mux := http.NewServeMux()
	mux.Handle("/"+prefix+"/", http.StripPrefix("/"+prefix, inner))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse httptest URL")

	return srv.URL + "/" + prefix, u.Host
}

// pushEmptyAt pushes an empty.Image to host/repo:tag while routing the
// actual HTTP traffic through util.PathPrefixTransport so it lands at
// host/prefix/v2/... on the wire. The registry reference itself uses only
// host+repo (go-containerregistry's name.NewRegistry rejects paths, same
// constraint deckschrubber works around in main.go); the prefix is carried
// at the transport layer exactly like the binary does it.
func pushEmptyAt(t *testing.T, addr, prefix, repo, tag string, auth authn.Authenticator) {
	t.Helper()
	refStr := fmt.Sprintf("%s/%s:%s", addr, repo, tag)
	ref, err := name.ParseReference(refStr, name.Insecure, name.StrictValidation)
	require.NoError(t, err, "parse %s", refStr)

	transport := util.NewPathPrefixTransport(prefix, http.DefaultTransport)
	opts := []remote.Option{remote.WithTransport(transport)}
	if auth != nil {
		opts = append(opts, remote.WithAuth(auth))
	}
	require.NoError(t, remote.Write(ref, empty.Image, opts...), "push %s via /%s", refStr, prefix)
}

// runDeckschrubberAt is the path-prefix-aware sibling of the harness
// runDeckschrubber helper. It runs the binary with whatever -registry URL
// the caller provides (including a path prefix) plus -debug and the given
// extra flags. Returns combined stdout+stderr. Fails the test on non-zero
// exit unless expectFailure is true.
func runDeckschrubberAt(t *testing.T, registryURL string, expectFailure bool, extraFlags ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	args := append([]string{"-registry", registryURL, "-debug"}, extraFlags...)
	cmd := exec.CommandContext(ctx, binaryPath(t), args...)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := combined.String()

	if expectFailure && err == nil {
		t.Fatalf("deckschrubber unexpectedly succeeded\nargs: %v\noutput:\n%s", args, output)
	}
	if !expectFailure && err != nil {
		t.Fatalf("deckschrubber failed: %v\nargs: %v\noutput:\n%s", err, args, output)
	}

	return output
}

// TestPathPrefixedRegistry pins the path-prefix contract: a registry
// reachable at http://host/docker/v2/... must be usable by passing
// -registry http://host/docker. Without util.PathPrefixTransport,
// go-containerregistry hardcodes /v2/ at the host root and all requests
// 404, so this test fails if the transport chain in main() is wired wrong
// or the prefix is not threaded through parseRegistryHost -> main.
//
// The registry is mounted under /docker via http.ServeMux; anything that
// bypasses the prefix (hits http://host/v2/... directly) will hit the
// ServeMux's default 404 handler and deckschrubber's CatalogPage call
// will fail with a loud error.
func TestPathPrefixedRegistry(t *testing.T) {
	baseURL, addr := mountRegistryAt(t, "docker")
	pushEmptyAt(t, addr, "docker", "app/svc", "v1", nil)

	output := runDeckschrubberAt(t, baseURL, false,
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
	)

	require.Contains(t, output, "Successfully fetched repositories",
		"catalog fetch must succeed against a path-prefixed registry; output:\n%s", output)
	require.Contains(t, output, `entries="[app/svc]"`,
		"expected the pushed repo to be returned under the prefix; output:\n%s", output)
	require.NotContains(t, output, "404",
		"no request should 404 — a 404 means a /v2/... hit the server without the /docker prefix; output:\n%s", output)
}

// TestPathPrefixedRegistryTrailingSlash proves a -registry URL with a
// trailing slash (http://host/docker/) behaves the same as without. This
// is a common mistake when users copy URLs from a browser.
func TestPathPrefixedRegistryTrailingSlash(t *testing.T) {
	baseURL, addr := mountRegistryAt(t, "docker")
	pushEmptyAt(t, addr, "docker", "app/svc", "v1", nil)

	output := runDeckschrubberAt(t, baseURL+"/", false,
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
	)

	require.Contains(t, output, "Successfully fetched repositories",
		"trailing slash on -registry must not break the prefix; output:\n%s", output)
	require.NotContains(t, output, "404",
		"no request should 404 with trailing slash; output:\n%s", output)
}

// TestPathPrefixedRegistryWithBasicAuth is the scenario explicitly flagged
// as broken: -registry http://host/docker combined with -user/-password.
// util.BasicAuthTransport gates credentials on
// strings.HasPrefix(req.URL.String(), configured -registry URL). If
// PathPrefixTransport did not run first, BasicAuthTransport would see
// http://host/v2/... (without /docker), fail the prefix match, and send
// every request anonymously. The auth middleware would then 401 and the
// tool would exit non-zero.
//
// This test proves the transport chain ordering: path rewrite happens
// before the auth gate fires, so the gate sees /docker on the path and
// injects credentials.
func TestPathPrefixedRegistryWithBasicAuth(t *testing.T) {
	user, pass := "testuser", "testpw"
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))

	inner := registry.New()
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle("/docker/", http.StripPrefix("/docker", authed))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	baseURL := srv.URL + "/docker"

	pushEmptyAt(t, u.Host, "docker", "app/svc", "v1", &authn.Basic{Username: user, Password: pass})

	output := runDeckschrubberAt(t, baseURL, false,
		"-user", user,
		"-password", pass,
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
	)

	require.Contains(t, output, "Successfully fetched repositories",
		"path-prefixed + basic-auth registry must round-trip credentials correctly; output:\n%s", output)
	require.False(t, strings.Contains(output, "401"),
		"no 401 should appear — a 401 means BasicAuthTransport's URL-prefix gate did not match the rewritten URL; output:\n%s", output)
}

// TestPathPrefixedRegistryWrongPrefix is a negative-control test: if the
// caller configures the wrong prefix (or forgets it entirely), every
// request must hit the ServeMux's default 404 handler and deckschrubber
// must exit non-zero. Without this, a silent behavioural regression (e.g.
// the prefix being dropped in parseRegistryHost) could look like a green
// test suite.
func TestPathPrefixedRegistryWrongPrefix(t *testing.T) {
	_, addr := mountRegistryAt(t, "docker")

	output := runDeckschrubberAt(t, "http://"+addr, true,
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
	)

	require.Contains(t, output, "404",
		"forgetting the /docker prefix must surface a 404; output:\n%s", output)
}
