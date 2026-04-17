package integration_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSharedDigestRetagAndDelete reproduces the scenario from
// https://github.com/fraunhoferfokus/deckschrubber/pull/49.
//
// When two tags share a single manifest digest and only one of them is
// selected for deletion, Docker Distribution has no Untag operation, so a
// naive manifestService.Delete(digest) would wipe out the image the preserved
// tag still needs. deckschrubber's Phase 1 solves this by retagging the
// doomed tag onto a different "disposable" digest (taken from another
// deletable-only image) and then deleting that disposable digest.
//
// Setup:
//   - keep-shared : non-deletable (fails -tag regex) pointing at digest A.
//   - old-shared  : deletable (matches -tag regex, past age deadline), also
//     pointing at digest A via remote.Tag.
//   - old-unique  : deletable, its own distinct digest B. Phase 1 will
//     consume this as the replacement digest.
//
// Because both tags on digest A share a single config blob, they also share
// one creation time. Both will be past the age deadline; the deletable vs
// non-deletable split comes entirely from the -tag regex. keep-shared is
// rescued from deletion by not matching ^old-, which is precisely the
// real-world scenario (e.g. "keep :latest while pruning :pr-123" when both
// happen to share a digest).
func TestSharedDigestRetagAndDelete(t *testing.T) {
	r := startRegistry(t)
	repo := "app/shared"

	ancient := time.Now().AddDate(0, 0, -30)

	sharedImg := r.pushImage(t, repo, "keep-shared", ancient)
	r.retag(t, repo, "old-shared", sharedImg)

	uniqueImg := r.pushImage(t, repo, "old-unique", ancient)

	sharedDigest, err := sharedImg.Digest()
	require.NoError(t, err)
	uniqueDigest, err := uniqueImg.Digest()
	require.NoError(t, err)
	require.NotEqual(t, sharedDigest, uniqueDigest,
		"setup sanity: the two images must have distinct digests")

	before := r.listTags(t, repo)
	require.ElementsMatch(t, []string{"keep-shared", "old-shared", "old-unique"}, before)

	output := r.runDeckschrubber(t,
		"-repos", "10",
		"-repo", "^app/",
		"-tag", "^old-",
		"-day", "7",
		"-latest", "0",
	)

	require.Contains(t, output, "Digest is shared with non-deletable tags",
		"expected Phase 1 (shared digest) log line; got output:\n%s", output)

	r.requireTagExists(t, repo, "keep-shared")
	r.requireDigestPullable(t, repo, sharedDigest)
	r.requireDigestGone(t, repo, uniqueDigest)
	r.requireTagGone(t, repo, "old-shared")
	r.requireTagGone(t, repo, "old-unique")
}

// TestSharedDigestNoReplacementAvailable exercises Phase 1's safety branch:
// when a deletable tag shares its digest with a non-deletable tag AND no
// other deletable-only digest exists to serve as a disposable replacement,
// deckschrubber must refuse to delete and log the fact. A regression here
// would manifest as the shared digest being wiped out, taking "keep-shared"
// down with it.
func TestSharedDigestNoReplacementAvailable(t *testing.T) {
	r := startRegistry(t)
	repo := "app/safe"

	sharedImg := r.pushImage(t, repo, "keep-shared", time.Now().AddDate(0, 0, -30))
	r.retag(t, repo, "old-shared", sharedImg)

	output := r.runDeckschrubber(t,
		"-repos", "10",
		"-repo", "^app/",
		"-tag", "^old-",
		"-day", "7",
		"-latest", "0",
	)

	require.Contains(t, output,
		"no disposable replacement digest is available",
		"expected Phase 1 safety-skip log; got output:\n%s", output)

	r.requireTagExists(t, repo, "keep-shared")
	r.requireTagExists(t, repo, "old-shared")

	sharedDigest, err := sharedImg.Digest()
	require.NoError(t, err)
	r.requireDigestPullable(t, repo, sharedDigest)
}

// TestSharedDigestAllDeletable is the plain PR #49 reproduction: multiple
// tags at the same digest with NO non-deletable tags to protect. Before the
// fix, the tool would fail to delete both tags because it could not safely
// untag. After the fix, Phase 2 deletes the shared digest once and both
// tags are gone.
func TestSharedDigestAllDeletable(t *testing.T) {
	r := startRegistry(t)
	repo := "app/all"

	ancient := time.Now().AddDate(0, 0, -30)
	img := r.pushImage(t, repo, "alpha-123", ancient)
	r.retag(t, repo, "beta-456", img)

	uniqueImg := r.pushImage(t, repo, "beta-with-unique-digest", ancient)

	r.runDeckschrubber(t,
		"-repos", "10",
		"-repo", "^app/",
		"-tag", "^(alpha-|beta-)",
		"-day", "1",
		"-latest", "0",
	)

	r.requireTagGone(t, repo, "alpha-123")
	r.requireTagGone(t, repo, "beta-456")
	r.requireTagGone(t, repo, "beta-with-unique-digest")

	sharedDigest, err := img.Digest()
	require.NoError(t, err)
	uniqueDigest, err := uniqueImg.Digest()
	require.NoError(t, err)
	r.requireDigestGone(t, repo, sharedDigest)
	r.requireDigestGone(t, repo, uniqueDigest)
}
