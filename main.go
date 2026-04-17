package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/writer"
	"golang.org/x/term"
)

var (
	/** CLI flags */
	// Base URL of registry
	registryURL *string
	// Regexps for filtering repositories and tags
	repoRegexpStr, tagRegexpStr, negTagRegexpStr *string
	// Maximum age of image to consider for deletion
	day, month, year *int
	// Max number of repositories to be fetched from registry
	repoCount *int
	// Number of the latest n matching images of an repository that will be ignored
	latest *int
	// If true, application runs in debug mode
	debug *bool
	// If true, no actual deletion is done
	dry *bool
	// If true, version is shown and program quits
	ver *bool

	// Compiled regexps
	repoRegexp, tagRegexp, negTagRegexp *regexp.Regexp
	// Skip insecure TLS
	insecure *bool
	// Username and password
	uname, passwd *string
	// Paging options
	paginate *bool
	pageSize *int
)

const (
	version string = "0.10.0"
)

// initLogging routes logrus output so that informational log levels go to
// stdout and problem-level log levels (>= WarnLevel) go to stderr. Logrus
// sends every level to stderr by default, which conflates normal
// operational output ("Deleting image", "Tag not outdated") with genuine
// diagnostics ("Could not delete image!") and makes it painful to pipe
// deckschrubber's output into anything that distinguishes the two streams
// (cron wrappers that treat stderr as "something went wrong", log shippers
// that separate application logs from error logs).
//
// Logrus has no built-in "split by level" sink, so the default writer is
// redirected to io.Discard and two writer hooks — one per stream — each
// handle a disjoint set of levels. This follows logrus/hooks/writer's own
// README; the hook's Fire() renders via entry.Bytes() so formatting
// (field order, timestamp, logfmt) stays identical to the default output.
// TraceLevel is included in the stdout set even though the tool doesn't
// currently emit Trace logs, so future additions route correctly without
// a follow-up change.
//
// Fatal/Panic still go to stderr and still call os.Exit(1)/panic after
// hooks fire — logrus runs the hook chain before termination, so
// log.Fatalf in main() continues to behave exactly as before, just via
// stderr through a hook instead of the default writer.
func initLogging() {
	log.SetOutput(io.Discard)
	log.AddHook(&writer.Hook{
		Writer: os.Stderr,
		LogLevels: []log.Level{
			log.PanicLevel,
			log.FatalLevel,
			log.ErrorLevel,
			log.WarnLevel,
		},
	})
	log.AddHook(&writer.Hook{
		Writer: os.Stdout,
		LogLevels: []log.Level{
			log.InfoLevel,
			log.DebugLevel,
			log.TraceLevel,
		},
	})
}

func init() {
	/** CLI flags */
	// Max number of repositories to fetch from registry (default = 5)
	repoCount = flag.Int("repos", 5, "number of repositories to garbage collect (before filtering, lexographically sorted by server)")
	// Base URL of registry (default = http://localhost:5000)
	registryURL = flag.String("registry", "http://localhost:5000", "URL of registry")
	// Maximum age of iamges to consider for deletion in days (default = 0)
	day = flag.Int("day", 0, "max age in days")
	// Maximum age of months to consider for deletion in days (default = 0)
	month = flag.Int("month", 0, "max age in months")
	// Maximum age of iamges to consider for deletion in years (default = 0)
	year = flag.Int("year", 0, "max age in days")
	// Regexp for images (default = .*)
	repoRegexpStr = flag.String("repo", ".*", "matching repositories (allows regexp)")
	// Regexp for tags (default = .*)
	tagRegexpStr = flag.String("tag", ".*", "matching tags (allows regexp)")
	// Negative regexp for tags (default = empty)
	negTagRegexpStr = flag.String("ntag", "", "non matching tags (allows regexp)")
	// The number of the latest matching images of an repository that won't be deleted
	latest = flag.Int("latest", 1, "number of the latest matching images of an repository that won't be deleted")
	// Dry run option (doesn't actually delete)
	debug = flag.Bool("debug", false, "run in debug mode")
	// Dry run option (doesn't actually delete)
	dry = flag.Bool("dry", false, "does not actually deletes")
	// Shows version
	ver = flag.Bool("v", false, "shows version and quits")
	// Skip insecure TLS
	insecure = flag.Bool("insecure", false, "Skip insecure TLS verification")
	// Username and password
	uname = flag.String("user", "", "Username for basic authentication")
	passwd = flag.String("password", "", "Password for basic authentication")
	// Paging options
	paginate = flag.Bool("paginate", false, "Set to use pagination when fetching repositories (default = false)")
	pageSize = flag.Int("page-size", 100, "Number of entries to fetch upon each request (default = 100)")
}

// parseRegistryHost extracts the host[:port] portion from a -registry URL
// (e.g. http://localhost:5000) and returns it alongside the name.Option set
// used to construct references: name.Insecure when the scheme is http so
// go-containerregistry talks plain HTTP, and name.StrictValidation so we
// fail loudly on malformed repo names rather than silently normalising.
//
// Paths are rejected. go-containerregistry's name.NewRegistry validates
// its input as an RFC 3986 authority (see pkg/name/registry.go
// checkRegistry) and every remote.* call hardcodes /v2/ at the host root,
// so a -registry URL like https://example.com/docker cannot be honoured
// without either a workaround transport or upstream library changes.
// Rather than ship a half-working workaround, fail at flag-parse time and
// point the operator at go-containerregistry. If you need path-prefixed
// registry support, patch go-containerregistry upstream.
func parseRegistryHost(registryURL string) (string, []name.Option, error) {
	u, err := url.Parse(registryURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse registry URL %q: %w", registryURL, err)
	}
	if u.Host == "" {
		return "", nil, fmt.Errorf("registry URL %q has no host", registryURL)
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		return "", nil, fmt.Errorf("registry URL %q has a path (%q); path-prefixed registries are not supported because go-containerregistry hardcodes /v2/ at the host root (see https://github.com/google/go-containerregistry/blob/main/pkg/name/registry.go checkRegistry). Use a scheme://host[:port] URL", registryURL, "/"+path)
	}
	opts := []name.Option{name.StrictValidation}
	if u.Scheme == "http" {
		opts = append(opts, name.Insecure)
	}
	return u.Host, opts, nil
}

func main() {
	flag.Parse()

	if len(os.Args) <= 1 {
		flag.Usage()
		return
	}

	// Compile regular expressions
	repoRegexp = regexp.MustCompile(*repoRegexpStr)
	tagRegexp = regexp.MustCompile(*tagRegexpStr)
	if *negTagRegexpStr != "" {
		negTagRegexp = regexp.MustCompile(*negTagRegexpStr)
	}

	if *ver {
		fmt.Printf("Version: %s\n", version)
		os.Exit(0)
	}

	// Must run before any log call or the first line would still hit
	// logrus's default stderr sink. `-v` short-circuits above without
	// logging, so calling here (not in init()) keeps the version path
	// free of side effects.
	initLogging()

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	// Add basic auth if user/pass is provided
	if *uname != "" && *passwd == "" {
		fmt.Println("Password:")
		bytePassword, err := term.ReadPassword(int(syscall.Stdin))
		if err == nil {
			stringPassword := string(bytePassword[:])
			passwd = &stringPassword
		} else {
			fmt.Println("Could not read password. Quitting!")
			os.Exit(1)
		}
	}
	host, nameOpts, err := parseRegistryHost(*registryURL)
	if err != nil {
		log.Fatalf("Could not parse registry URL! (err: %v)", err)
	}

	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
	}
	var auth authn.Authenticator = authn.Anonymous
	if *uname != "" || *passwd != "" {
		auth = &authn.Basic{Username: *uname, Password: *passwd}
	}

	registry, err := name.NewRegistry(host, nameOpts...)
	if err != nil {
		log.Fatalf("Could not create registry object! (err: %v)", err)
	}

	ctx := context.Background()

	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithTransport(transport),
		remote.WithAuth(auth),
	}

	// List of all repositories fetched from the registry. The number
	// of fetched repositories depends on the number provided by the
	// user ('-repos' flag) and pagination settings.
	windowSize := *repoCount
	if *paginate {
		if *repoCount > *pageSize {
			windowSize = *pageSize
		} else {
			log.Warnf("Pagination enabled but page size is larger than repo count ('-repo' %d < %d '-page-size)", *repoCount, *pageSize)
		}
	}
	var entries []string
	// Empty string denotes that the query is being made for the first
	// time, so the server starts with the first set of repositories
	// (sorted lexigraphically).
	var last string
	for {
		page, err := remote.CatalogPage(registry, last, windowSize, remoteOpts...)
		if err != nil && err != io.EOF {
			log.Fatalf("Error while fetching repositories! (err: %v)", err)
		}

		log.WithFields(log.Fields{"count": len(page), "entries": page}).Info("Successfully fetched repositories.")
		entries = append(entries, page...)

		// Stop the query if:
		// - the server returned fewer than a full page (implies end of catalog)
		// - we already have more than requested entries
		// - an io.EOF was signalled
		if err == io.EOF || len(page) < windowSize || len(entries) >= *repoCount {
			break
		}

		// Advance the cursor to the last repo we saw.
		last = page[len(page)-1]
	}

	// Deadline defines the youngest creation date for an image
	// to be considered for deletion
	deadline := time.Now().AddDate(*year/-1, *month/-1, *day/-1)

	// Fetch information about images belonging to each repository
	for _, entry := range entries {
		logger := log.WithField("repo", entry)

		matched := repoRegexp.MatchString(entry)

		if !matched {
			logger.WithFields(log.Fields{"entry": entry}).Debug("Ignore non matching repository (-repo=", *repoRegexpStr, ")")
			continue
		}

		repoRef, err := name.NewRepository(fmt.Sprintf("%s/%s", host, entry), nameOpts...)
		if err != nil {
			logger.WithFields(log.Fields{"entry": entry}).Fatalf("Could not create repo from name! (err: %v)", err)
		}
		logger.Debug("Successfully created repository object.")

		tagsData, err := remote.List(repoRef, remoteOpts...)
		if err != nil {
			logger.Fatalf("Couldn't fetch tags! (err: %v)", err)
		}

		var tags []Image

		// Fetch information about each tag of a repository
		// This involves fetching the descriptor (resolves tag -> digest)
		// and the image config blob so we can read its Created timestamp.
		tagFetchDataErrors := false
		for _, tag := range tagsData {
			tagLogger := logger.WithField("tag", tag)

			tagLogger.Debug("Fetching tag descriptor...")
			tagRef := repoRef.Tag(tag)
			desc, err := remote.Get(tagRef, remoteOpts...)
			if err != nil {
				tagLogger.WithField("err", err).Error("Could not fetch tag!")
				tagFetchDataErrors = true
				break
			}

			// Multi-arch manifest lists don't have a single Created
			// timestamp; skip the whole repo the same way we skip on
			// any other per-tag data error, so we never act on partial
			// information. This matches the pre-migration behaviour
			// where schema2 deserialization of a manifest list would
			// leave Created zero and every tag would look "outdated".
			if desc.MediaType.IsIndex() {
				tagLogger.WithField("mediaType", string(desc.MediaType)).Error("Tag refers to a manifest list (multi-arch); deckschrubber cannot compute a single Created timestamp for it - skipping repo to avoid acting on partial data")
				tagFetchDataErrors = true
				break
			}

			tagLogger.Debug("Fetching image config...")
			img, err := desc.Image()
			if err != nil {
				tagLogger.WithField("err", err).Error("Could not resolve manifest to image!")
				tagFetchDataErrors = true
				break
			}

			cfg, err := img.ConfigFile()
			if err != nil {
				tagLogger.WithField("err", err).Error("Could not fetch image config!")
				tagFetchDataErrors = true
				break
			}

			tags = append(tags, Image{entry, tag, cfg.Created.Time, desc.Digest})
		}

		if tagFetchDataErrors {
			// In case of error at any one tag, skip entire repo
			// (avoid acting on incomplete data, which migth lead to
			// deleting images that are actually in use)
			logger.Error("Error obtaining tag data - skipping this repo")
			continue
		}

		sort.Sort(ImageByDate(tags))

		logger.Debug("Analyzing tags...")

		tagCount := len(tags)

		if tagCount == 0 {
			logger.Debug("Ignore repository with no matching tags")
			continue
		}

		deletableTags := make(map[int]Image)
		nonDeletableTags := make(map[int]Image)

		ignoredTags := 0

		for tagIndex := len(tags) - 1; tagIndex >= 0; tagIndex-- {
			tag := tags[tagIndex]
			markForDeletion := false
			tagLogger := logger.WithField("tag", tag.Tag)

			// Provides a text which is followed by the tag and ntag flag values. The
			// latter iff defined.
			withTagParens := func(text string) string {
				xs := []string{fmt.Sprintf("-tag=%s", *tagRegexpStr)}
				if *negTagRegexpStr != "" {
					xs = append(xs, fmt.Sprintf("-ntag=%s", *negTagRegexpStr))
				}
				return fmt.Sprintf("%s (%s)", text, strings.Join(xs, ", "))
			}

			// Check whether the tag matches. If that's the case, don't stop there, and
			// check for the negative regexp as well.
			matched := tagRegexp.MatchString(tag.Tag)
			if matched && negTagRegexp != nil {
				negTagMatch := negTagRegexp.MatchString(tag.Tag)
				matched = !negTagMatch
			}

			if matched {
				tagLogger.Debug(withTagParens("Tag matches, considering for deletion"))
				if tag.Time.Before(deadline) {
					if ignoredTags < *latest {
						tagLogger.WithField("time", tag.Time).Infof("Ignore %d latest matching tags (-latest=%d)", *latest, *latest)
						ignoredTags++
					} else {
						tagLogger.WithField("tag", tag.Tag).WithField("time", tag.Time).Infof("Marking tag as outdated")
						markForDeletion = true
					}
				} else {
					tagLogger.Info("Tag not outdated")
					ignoredTags++
				}
			} else {
				tagLogger.Info(withTagParens("Ignore non matching tag"))
			}

			if markForDeletion {
				deletableTags[tagIndex] = tag
			} else {
				nonDeletableTags[tagIndex] = tag
			}
		}

		// This approach is actually a workaround for the problem that Docker
		// Distribution doesn't implement TagService.Untag operation at the time of
		// this writing.
		// We delete image digests for outdated tags. If a digest is shared with tags
		// we must preserve, we retag the outdated tag to a disposable digest and then
		// delete that disposable digest.
		nonDeletableDigests := make(map[string]string)
		for _, tag := range nonDeletableTags {
			digest := tag.Digest.String()
			if existingTags, exists := nonDeletableDigests[digest]; !exists {
				nonDeletableDigests[digest] = tag.Tag
			} else {
				nonDeletableDigests[digest] = existingTags + ", " + tag.Tag
			}
		}

		replacementDigests := make(map[string]Image)
		for _, tag := range deletableTags {
			digest := tag.Digest.String()
			if _, exists := nonDeletableDigests[digest]; !exists {
				replacementDigests[digest] = tag
			}
		}

		digestsDeleted := make(map[string]bool)

		// Phase 1: shared digests first. Retag outdated tags to disposable
		// digests and delete those disposable digests.
		for _, tag := range deletableTags {
			digest := tag.Digest.String()

			if digestsDeleted[digest] {
				logger.WithField("tag", tag.Tag).Debug("Image under tag already deleted")
				continue
			}

			nonDeletableTagsForDigest, hasNonDeletableTags := nonDeletableDigests[digest]
			if !hasNonDeletableTags {
				logger.WithField("tag", tag.Tag).Debug("Digest is not shared - defer direct deletion to phase 2")
				continue
			}

			var replacementDigestStr string
			var replacementTag Image
			for candidateDigest, candidateTag := range replacementDigests {
				if candidateDigest != digest {
					replacementDigestStr = candidateDigest
					replacementTag = candidateTag
					break
				}
			}

			if replacementDigestStr == "" {
				logger.WithField("tag", tag.Tag).WithField("alsoUsedByTags", nonDeletableTagsForDigest).Info("The underlying image is also used by non-deletable tags and no disposable replacement digest is available - skipping deletion")
				continue
			}

			logger.
				WithField("tag", tag.Tag).
				WithField("sharedDigest", digest).
				WithField("alsoUsedByTags", nonDeletableTagsForDigest).
				WithField("replacementDigest", replacementTag.Digest.String()).
				Info("Digest is shared with non-deletable tags - retagging to disposable digest for safe untag")

			if *dry {
				logger.WithField("tag", tag.Tag).WithField("replacementDigest", replacementTag.Digest.String()).Infof("Not actually retagging/deleting digest (-dry=%v)", *dry)
				continue
			}

			// Resolve the replacement digest to a descriptor, then PUT that
			// descriptor under the outdated tag name. remote.Tag writes the
			// manifest bytes as-is under <tag>, so no blob re-upload
			// happens. This mirrors `docker tag src dst && docker push dst`
			// at the wire level and is the same sequence carvel-dev/kbld
			// uses for its registry retag helper.
			srcRef := repoRef.Digest(replacementTag.Digest.String())
			srcDesc, err := remote.Get(srcRef, remoteOpts...)
			if err != nil {
				logger.WithField("tag", tag.Tag).WithField("replacementDigest", replacementTag.Digest.String()).WithField("err", err).Error("Could not fetch replacement descriptor for retagging!")
				continue
			}

			dstRef := repoRef.Tag(tag.Tag)
			if err := remote.Tag(dstRef, srcDesc, remoteOpts...); err != nil {
				logger.WithField("tag", tag.Tag).WithField("replacementDigest", replacementTag.Digest.String()).WithField("err", err).Error("Could not retag image to disposable digest!")
				continue
			}

			replacementDigestRef := repoRef.Digest(replacementTag.Digest.String())
			if err := remote.Delete(replacementDigestRef, remoteOpts...); err != nil {
				logger.WithField("tag", tag.Tag).WithField("replacementDigest", replacementTag.Digest.String()).WithField("err", err).Error("Could not delete disposable digest after retagging!")
				continue
			}

			delete(replacementDigests, replacementDigestStr)
			digestsDeleted[replacementDigestStr] = true
		}

		// Phase 2: non-shared digests. Delete digest directly.
		for _, tag := range deletableTags {
			digest := tag.Digest.String()

			if digestsDeleted[digest] {
				logger.WithField("tag", tag.Tag).Debug("Image under tag already deleted")
				continue
			}

			if _, hasNonDeletableTags := nonDeletableDigests[digest]; hasNonDeletableTags {
				logger.WithField("tag", tag.Tag).Debug("Digest is shared - already handled in phase 1")
				continue
			}

			logger.WithField("tag", tag.Tag).Info("All tags for this image digest marked for deletion")

			if *dry {
				logger.WithField("tag", tag.Tag).WithField("time", tag.Time).Infof("Not actually deleting image (-dry=%v)", *dry)
				continue
			}

			logger.WithField("tag", tag.Tag).WithField("time", tag.Time).WithField("digest", tag.Digest).Infof("Deleting image (-dry=%v)", *dry)
			digestRef := repoRef.Digest(tag.Digest.String())
			err := remote.Delete(digestRef, remoteOpts...)
			if err != nil {
				logger.WithField("tag", tag.Tag).WithField("err", err).Error("Could not delete image!")
				continue
			}

			digestsDeleted[digest] = true
		}

	}
}
