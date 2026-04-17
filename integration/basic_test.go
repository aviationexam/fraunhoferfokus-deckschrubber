package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLatestPreservesNewestTags confirms that -latest N keeps the N most
// recently created tags and deletes everything older that matches. This
// exercises Phase 2 (non-shared digests).
//
// After a manifest delete, both the OCI v2 spec and real-world registry:2
// behavior leave the tag→digest alias in place (the alias 404s on pull).
// Assertions therefore verify pullability (requireTagGone / requireTagExists)
// rather than tag-list membership.
func TestLatestPreservesNewestTags(t *testing.T) {
	r := startRegistry(t)
	repo := "app/svc"

	now := time.Now()
	tagTimes := map[string]time.Time{
		"v1": now.AddDate(0, 0, -5),
		"v2": now.AddDate(0, 0, -4),
		"v3": now.AddDate(0, 0, -3),
		"v4": now.AddDate(0, 0, -2),
		"v5": now.AddDate(0, 0, -1),
	}
	for tag, created := range tagTimes {
		r.pushImage(t, repo, tag, created)
	}

	r.runDeckschrubber(t, "-repos", "10", "-repo", "^app/", "-day", "1", "-latest", "2")

	r.requireTagExists(t, repo, "v4")
	r.requireTagExists(t, repo, "v5")
	r.requireTagGone(t, repo, "v1")
	r.requireTagGone(t, repo, "v2")
	r.requireTagGone(t, repo, "v3")
}

// TestDeadlinePreservesYoungTags confirms tags created AFTER the deadline
// are preserved regardless of -latest: the age filter is applied first.
func TestDeadlinePreservesYoungTags(t *testing.T) {
	r := startRegistry(t)
	repo := "app/api"

	now := time.Now()
	r.pushImage(t, repo, "stale-1", now.AddDate(0, 0, -30))
	r.pushImage(t, repo, "stale-2", now.AddDate(0, 0, -20))
	r.pushImage(t, repo, "fresh-1", now.AddDate(0, 0, -2))
	r.pushImage(t, repo, "fresh-2", now.AddDate(0, 0, -1))

	r.runDeckschrubber(t, "-repos", "10", "-repo", "^app/", "-day", "7", "-latest", "0")

	r.requireTagExists(t, repo, "fresh-1")
	r.requireTagExists(t, repo, "fresh-2")
	r.requireTagGone(t, repo, "stale-1")
	r.requireTagGone(t, repo, "stale-2")
}

// TestDryRunDeletesNothing verifies -dry short-circuits both deletion phases
// and every tag remains pullable afterwards.
func TestDryRunDeletesNothing(t *testing.T) {
	r := startRegistry(t)
	repo := "app/dry"

	now := time.Now()
	r.pushImage(t, repo, "old-1", now.AddDate(0, 0, -30))
	r.pushImage(t, repo, "old-2", now.AddDate(0, 0, -20))
	r.pushImage(t, repo, "old-3", now.AddDate(0, 0, -10))

	output := r.runDeckschrubber(t, "-repos", "10", "-repo", "^app/", "-day", "1", "-latest", "0", "-dry")

	for _, tag := range []string{"old-1", "old-2", "old-3"} {
		require.True(t,
			logLineMatches(output, "Not actually deleting image (-dry=true)", tag),
			"dry-run should log 'Not actually deleting' for %s; output:\n%s", tag, output)
	}

	r.requireTagExists(t, repo, "old-1")
	r.requireTagExists(t, repo, "old-2")
	r.requireTagExists(t, repo, "old-3")
}

// TestRepoRegexScopesDeletion confirms -repo limits deletion to matching
// repositories; tags in non-matching repos must be left untouched.
func TestRepoRegexScopesDeletion(t *testing.T) {
	r := startRegistry(t)

	ancient := time.Now().AddDate(0, 0, -30)
	r.pushImage(t, "team-a/svc", "v1", ancient)
	r.pushImage(t, "team-b/svc", "v1", ancient)

	r.runDeckschrubber(t, "-repos", "10", "-repo", "^team-a/", "-day", "1", "-latest", "0")

	r.requireTagGone(t, "team-a/svc", "v1")
	r.requireTagExists(t, "team-b/svc", "v1")
}

// TestTagRegexFilter verifies -tag and -ntag combine to select which tags
// are considered for deletion. release-1/release-2 match -tag and miss
// -ntag → deletable. release-latest matches -tag but also matches -ntag →
// excluded. snapshot-1 does not match -tag → excluded.
func TestTagRegexFilter(t *testing.T) {
	r := startRegistry(t)
	repo := "app/multi"

	ancient := time.Now().AddDate(0, 0, -30)
	r.pushImage(t, repo, "release-1", ancient)
	r.pushImage(t, repo, "release-2", ancient)
	r.pushImage(t, repo, "release-latest", ancient)
	r.pushImage(t, repo, "snapshot-1", ancient)

	r.runDeckschrubber(t,
		"-repos", "10",
		"-repo", "^app/",
		"-tag", "^release",
		"-ntag", "^release-latest$",
		"-day", "1",
		"-latest", "0",
	)

	r.requireTagGone(t, repo, "release-1")
	r.requireTagGone(t, repo, "release-2")
	r.requireTagExists(t, repo, "release-latest")
	r.requireTagExists(t, repo, "snapshot-1")
}

// TestNoOpWhenNothingOldEnough confirms the tool exits cleanly with no
// deletions when every tag is younger than the deadline.
func TestNoOpWhenNothingOldEnough(t *testing.T) {
	r := startRegistry(t)
	repo := "app/noop"

	now := time.Now()
	r.pushImage(t, repo, "recent-1", now.AddDate(0, 0, -1))
	r.pushImage(t, repo, "recent-2", now.Add(-2*time.Hour))

	output := r.runDeckschrubber(t, "-repos", "10", "-repo", "^app/", "-day", "30", "-latest", "0")

	r.requireTagExists(t, repo, "recent-1")
	r.requireTagExists(t, repo, "recent-2")

	require.False(t,
		strings.Contains(output, `level=info msg="Deleting image`),
		"no 'Deleting image' log line should appear when nothing is outdated\noutput:\n%s", output)
}
