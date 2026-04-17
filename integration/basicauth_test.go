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
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// authRegistry wraps an in-memory registry in an HTTP Basic auth middleware
// that enforces a fixed (user, pass) tuple. Any request missing or carrying
// the wrong credentials gets a 401 with a WWW-Authenticate challenge, which
// is what real-world registry:2 deployments do behind htpasswd.
type authRegistry struct {
	addr string
	url  string
	user string
	pass string
	srv  *httptest.Server
}

func startAuthRegistry(t *testing.T, user, pass string) *authRegistry {
	t.Helper()

	inner := registry.New()
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))

	mw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})

	srv := httptest.NewServer(mw)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse httptest URL")

	return &authRegistry{
		addr: u.Host,
		url:  srv.URL,
		user: user,
		pass: pass,
		srv:  srv,
	}
}

// pushEmpty pushes an empty.Image under repo:tag. We don't use the regular
// testRegistry.pushImage here because this helper needs to present the same
// credentials the middleware expects, and the crane remote.* calls take
// their auth via remote.Option rather than an env hook.
func (r *authRegistry) pushEmpty(t *testing.T, repo, tag string) {
	t.Helper()
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", r.addr, repo, tag), name.StrictValidation)
	require.NoError(t, err, "parse reference")

	auth := remote.WithAuth(&authn.Basic{Username: r.user, Password: r.pass})

	require.NoError(t, remote.Write(ref, empty.Image, auth), "push %s", ref)
}

// runDeckschrubberWithFlags is a minimal runner that does NOT inject -registry
// or -debug; the caller owns the full flag set. Used for the auth cases so we
// can pass -user/-password and control expected exit behaviour.
func runDeckschrubberWithFlags(t *testing.T, expectSuccess bool, flags ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, binaryPath(t), flags...)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := combined.String()

	if expectSuccess && err != nil {
		t.Fatalf("deckschrubber unexpectedly failed: %v\nflags: %v\noutput:\n%s", err, flags, output)
	}
	if !expectSuccess && err == nil {
		t.Fatalf("deckschrubber unexpectedly succeeded\nflags: %v\noutput:\n%s", flags, output)
	}

	return output
}

// TestBasicAuthCorrectCredentials proves util.BasicAuthTransport injects
// credentials on requests to the configured -registry URL and that
// remote.WithTransport wires the transport through go-containerregistry's
// remote.* calls. The registry is wrapped in a middleware that 401s any
// request without the exact (user, pass) tuple, so a successful catalog
// fetch is only possible if basic auth actually made it on the wire.
func TestBasicAuthCorrectCredentials(t *testing.T) {
	r := startAuthRegistry(t, "testuser", "testpw")
	r.pushEmpty(t, "app/svc", "v1")

	output := runDeckschrubberWithFlags(t, true,
		"-registry", r.url,
		"-user", "testuser",
		"-password", "testpw",
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
		"-debug",
	)

	require.Contains(t, output, "Successfully fetched repositories",
		"expected catalog fetch to succeed under basic auth; output:\n%s", output)
}

// TestBasicAuthWrongPassword proves credentials are actually checked: wrong
// password -> 401 from the middleware -> deckschrubber exits non-zero and
// the 401 surfaces in the output. This is the "credentials are wired but the
// value is wrong" path (vs. "credentials not wired at all", which would still
// 401 but is not what we are measuring here).
func TestBasicAuthWrongPassword(t *testing.T) {
	r := startAuthRegistry(t, "testuser", "testpw")

	output := runDeckschrubberWithFlags(t, false,
		"-registry", r.url,
		"-user", "testuser",
		"-password", "wrongpassword",
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
		"-debug",
	)

	require.Contains(t, output, "401",
		"expected 401 to surface in combined output on bad password; output:\n%s", output)
}

// TestBasicAuthNoCredentials proves deckschrubber does not silently succeed
// against an auth-protected registry when the operator forgets -user/-password.
// Without the transport's URL-prefix-gated SetBasicAuth call, every request
// is anonymous and the middleware 401s, which is the desired failure mode.
func TestBasicAuthNoCredentials(t *testing.T) {
	r := startAuthRegistry(t, "testuser", "testpw")

	output := runDeckschrubberWithFlags(t, false,
		"-registry", r.url,
		"-repos", "10",
		"-repo", "^app/",
		"-dry",
		"-debug",
	)

	require.Contains(t, output, "401",
		"expected 401 when no credentials are provided; output:\n%s", output)
}
