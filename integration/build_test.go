package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// We build the deckschrubber binary once per `go test` invocation and reuse
// it across every test. Building takes ~1s and produces a ~10MB artifact; we
// don't want to pay that cost per test.
//
// We do NOT use TestMain because that would force us to duplicate test
// setup/teardown semantics the stdlib already handles. Instead we build
// lazily the first time any test asks for binaryPath, and cache via sync.Once.
// If the build fails the test that triggered it fails; other tests running
// in parallel will observe the same cached error.

var (
	buildOnce    sync.Once
	builtBinPath string
	buildErr     error
)

// binaryPath returns the absolute path to the freshly-built deckschrubber
// binary, building it on first call. Subsequent calls reuse the same binary.
// The binary is left in a temp directory and cleaned up by the OS (we
// intentionally don't t.Cleanup it — multiple tests share it).
func binaryPath(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		builtBinPath, buildErr = buildDeckschrubber()
	})
	if buildErr != nil {
		t.Fatalf("failed to build deckschrubber binary: %v", buildErr)
	}
	return builtBinPath
}

func buildDeckschrubber() (string, error) {
	// Locate the module root. runtime.Caller gives us the path to this test
	// file, and the module root is one directory up from integration/.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	moduleRoot := filepath.Dir(filepath.Dir(thisFile))

	binDir, err := os.MkdirTemp("", "deckschrubber-bin-")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	binName := "deckschrubber"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(binDir, binName)

	// Use `go build` from the module root. We rely on the ambient GOFLAGS/
	// GOPROXY/GOCACHE so this works both in developer environments and in
	// GitHub Actions runners.
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = moduleRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build in %s failed: %v\noutput:\n%s", moduleRoot, err, string(out))
	}

	return binPath, nil
}
