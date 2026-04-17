// Package integration_test exercises the deckschrubber binary end-to-end against
// an in-memory Docker Distribution v2 registry.
//
// Design:
//
//   - We do NOT import deckschrubber as a library. The tool is a monolithic
//     main() today, so we test it by exec'ing the built binary. TestMain in
//     build_test.go builds the binary once and exposes its path via
//     binaryPath.
//
//   - The registry is the in-memory implementation from
//     github.com/google/go-containerregistry/pkg/registry, wrapped in
//     httptest.Server. It supports manifest deletion out of the box (no
//     REGISTRY_STORAGE_DELETE_ENABLED knob), so it exercises the same code
//     paths the production registry:2 image would when configured with
//     delete.enabled: true.
//
//   - Test images are synthetic: empty.Image + mutate.AppendLayers with tiny
//     random layers, plus an explicit Created timestamp set via
//     mutate.CreatedAt. deckschrubber reads the `created` field from the
//     image config blob (see main.go BlobInfo + json.Unmarshal), so setting
//     CreatedAt is what lets us age tags for -day/-month/-year/-latest
//     assertions.
//
//   - The shared-digest test case (reproduction of aviationexam/
//     deckschrubber#2) works by pushing image X under one tag, then using
//     remote.Tag to re-point a second tag at the SAME manifest digest. That
//     mirrors `docker tag ubuntu foo:a && docker tag ubuntu foo:b && docker
//     push foo:a && docker push foo:b` exactly.
package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/require"
)

// testRegistry bundles an in-memory OCI v2 registry bound to an ephemeral
// port. addr is the "host:port" form (without scheme) suitable for
// name.NewRegistry / name.ParseReference. url is the "http://host:port" form
// suitable for passing to deckschrubber's -registry flag.
type testRegistry struct {
	addr string
	url  string
	srv  *httptest.Server
}

// startRegistry spins up an in-memory registry and registers a Cleanup hook
// to shut it down at the end of the test. The registry persists only for the
// lifetime of the test; each test that calls startRegistry gets a fresh one.
func startRegistry(t *testing.T) *testRegistry {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse httptest URL")

	return &testRegistry{
		addr: u.Host,
		url:  srv.URL,
		srv:  srv,
	}
}

// ref builds a fully-qualified reference (host/repo:tag) for this registry.
func (r *testRegistry) ref(t *testing.T, repo, tag string) name.Reference {
	t.Helper()
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", r.addr, repo, tag), name.StrictValidation)
	require.NoError(t, err, "parse reference %s/%s:%s", r.addr, repo, tag)
	return ref
}

// pushImage creates a tiny synthetic image with a unique layer and the given
// Created timestamp, pushes it under the given repo:tag, and returns the
// pushed image (useful for its Digest).
//
// The image is constructed from empty.Image + a single random 32-byte layer
// so every call produces a unique manifest digest (distinct content =>
// distinct digest). The Created timestamp is embedded in the image config,
// which is what deckschrubber reads via BlobInfo.Created.
func (r *testRegistry) pushImage(t *testing.T, repo, tag string, created time.Time) v1.Image {
	t.Helper()

	// Random 32-byte layer so every image has a unique content digest, even
	// when callers pass the same repo/tag/created combination.
	buf := make([]byte, 32)
	//nolint:gosec // math/rand is fine; we only need uniqueness, not secrecy.
	_, err := rand.New(rand.NewSource(nextSeed())).Read(buf)
	require.NoError(t, err, "generate random layer bytes")

	layer := static.NewLayer(buf, types.DockerLayer)

	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err, "append layer")

	img, err = mutate.CreatedAt(img, v1.Time{Time: created.UTC()})
	require.NoError(t, err, "set CreatedAt on image")

	ref := r.ref(t, repo, tag)
	require.NoError(t, remote.Write(ref, img), "push image %s", ref)

	return img
}

// retag points `tag` at the same manifest digest as the given image, WITHOUT
// re-uploading the manifest. This reproduces the `docker tag src dst && docker
// push dst` pattern that produces two tags sharing a single digest — the
// exact scenario aviationexam/deckschrubber#2 fixes.
func (r *testRegistry) retag(t *testing.T, repo, tag string, img v1.Image) {
	t.Helper()
	ref := r.ref(t, repo, tag)
	require.NoError(t, remote.Tag(ref.Context().Tag(tag), img), "retag %s to same digest", ref)
}

// listTags returns the current tag list for a repository, sorted by the
// registry (alphabetical in the in-memory implementation). Useful for
// asserting post-GC state.
func (r *testRegistry) listTags(t *testing.T, repo string) []string {
	t.Helper()
	repoRef, err := name.NewRepository(fmt.Sprintf("%s/%s", r.addr, repo), name.StrictValidation)
	require.NoError(t, err, "parse repo %s", repo)
	tags, err := remote.List(repoRef)
	require.NoError(t, err, "list tags for %s", repo)
	return tags
}

// requireTagExists asserts that `repo:tag` resolves to a manifest that is
// still pullable. This is stronger than "tag is in the tag list" because it
// verifies the underlying manifest wasn't orphaned.
func (r *testRegistry) requireTagExists(t *testing.T, repo, tag string) v1.Image {
	t.Helper()
	ref := r.ref(t, repo, tag)
	img, err := remote.Image(ref)
	require.NoError(t, err, "expected %s to still exist and be pullable", ref)
	// Force a manifest fetch to prove the whole chain resolves.
	_, err = img.Manifest()
	require.NoError(t, err, "fetch manifest for %s", ref)
	return img
}

// requireTagGone asserts that the manifest a tag points at has been deleted.
// This is verified at the digest level, not the tag level: ggcr's in-memory
// registry (and real registry:2 in some configurations) keeps a dangling
// tag→manifest entry even after the underlying digest is deleted, so
// checking the tag alone would produce false positives. We first resolve
// the tag (if it still resolves) and then verify its digest is unreachable
// directly, which matches the tool's actual delete-by-digest contract.
func (r *testRegistry) requireTagGone(t *testing.T, repo, tag string) {
	t.Helper()
	ref := r.ref(t, repo, tag)
	img, tagErr := remote.Image(ref)
	if tagErr != nil {
		return
	}
	digest, err := img.Digest()
	require.NoError(t, err, "compute digest of %s (tag still resolves, so we need its digest)", ref)
	digestRef, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", r.addr, repo, digest.String()), name.StrictValidation)
	require.NoError(t, err, "build digest reference for %s", ref)
	_, err = remote.Image(digestRef)
	require.Error(t, err,
		"expected %s (tag=%s, digest=%s) to be deleted, but digest still resolves",
		ref, tag, digest)
}

// requireDigestPullable asserts that a specific manifest digest is still
// present in the registry (independent of any tag pointing at it). Used to
// confirm that phase-1 retagging preserved the digest the non-deletable tag
// points to.
func (r *testRegistry) requireDigestPullable(t *testing.T, repo string, digest v1.Hash) {
	t.Helper()
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", r.addr, repo, digest.String()), name.StrictValidation)
	require.NoError(t, err, "parse digest reference")
	img, err := remote.Image(ref)
	require.NoError(t, err, "expected digest %s in %s to be pullable", digest, repo)
	_, err = img.Manifest()
	require.NoError(t, err, "fetch manifest for %s@%s", repo, digest)
}

// requireDigestGone asserts that a specific manifest digest is NO LONGER
// present in the registry.
func (r *testRegistry) requireDigestGone(t *testing.T, repo string, digest v1.Hash) {
	t.Helper()
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", r.addr, repo, digest.String()), name.StrictValidation)
	require.NoError(t, err, "parse digest reference")
	_, err = remote.Image(ref)
	require.Error(t, err, "expected digest %s in %s to be deleted, but it still resolves", digest, repo)
}

// runDeckschrubber invokes the built binary with the given flags against this
// registry. The registry URL is injected automatically; callers pass only the
// additional flags. Returns combined stdout+stderr so tests can assert on
// structured logrus lines (e.g. "Digest is shared with non-deletable tags").
//
// Timeout: 30s per invocation. The in-memory registry is fast; anything slower
// indicates a bug (likely deletion hanging on a retry loop).
func (r *testRegistry) runDeckschrubber(t *testing.T, extraFlags ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	args := append([]string{"-registry", r.url, "-debug"}, extraFlags...)
	cmd := exec.CommandContext(ctx, binaryPath(t), args...)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := combined.String()

	// deckschrubber exits 0 on success; non-zero is a test failure because
	// the tool's expected behavior is to log per-tag errors and continue.
	// Any non-zero exit means something we don't tolerate (panic, bad flag).
	if err != nil {
		t.Fatalf("deckschrubber failed: %v\nargs: %v\noutput:\n%s", err, args, output)
	}

	return output
}

// seedCounter is seeded from wall-clock at package init and monotonically
// incremented per pushImage call, producing a distinct int64 per image even
// under t.Parallel. Uniqueness (not secrecy) is the goal.
var seedCounter atomic.Int64

func init() {
	seedCounter.Store(time.Now().UnixNano())
}

func nextSeed() int64 {
	return seedCounter.Add(1)
}

// logLineMatches reports whether `output` contains a single logrus logfmt
// line that has BOTH the given msg and the given tag field. The logger
// writes lines shaped like
//
//	time="..." level=info msg="Tag not outdated" repo=... tag=<tag>
//
// Two independent strings.Contains calls on msg and tag would both succeed
// even when msg belongs to a different tag on a different line, so the
// invariant "msg X was logged FOR tag Y" would not actually be pinned.
// Anchoring both conditions to one line via `(?m)^...$` is what proves msg
// is associated with tag. Inputs are regex-escaped, so test sites can pass
// plain literal strings.
func logLineMatches(output, msg, tag string) bool {
	pattern := `(?m)^.*\bmsg="` + regexp.QuoteMeta(msg) + `".*\btag=` + regexp.QuoteMeta(tag) + `\b.*$`
	return regexp.MustCompile(pattern).MatchString(output)
}
