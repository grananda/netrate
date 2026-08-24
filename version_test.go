package main

import (
	"regexp"
	"strings"
	"testing"
)

// semver is the same shape the release workflow accepts. If the two ever drift,
// a version that passes here would stall the pipeline instead of publishing.
var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// The embedded VERSION file is what the pipeline turns into a tag, so a stray
// "v" prefix or a trailing edit would break the release rather than the build.
func TestDeclaredVersionIsSemver(t *testing.T) {
	got := strings.TrimSpace(declaredVersion)

	if got == "" {
		t.Fatal("VERSION is empty")
	}
	if !semver.MatchString(got) {
		t.Fatalf("VERSION is %q, want a bare semantic version such as 0.1.0 (no leading v)", got)
	}
	if got != declaredVersion && strings.TrimSpace(declaredVersion) != strings.Trim(declaredVersion, "\n") {
		t.Errorf("VERSION is %q: it should hold the number and a single newline", declaredVersion)
	}
}

func TestVersionString(t *testing.T) {
	declared := strings.TrimSpace(declaredVersion)

	t.Run("a build with no stamps is marked as a development one", func(t *testing.T) {
		// version/commit/date are empty unless a release build stamped them,
		// which is exactly the state of `go build` and `go test`.
		got := versionString()

		if !strings.Contains(got, appName) {
			t.Errorf("%q does not name the tool", got)
		}
		if !strings.Contains(got, declared+"-dev") {
			t.Errorf("%q should mark itself %s-dev: an untagged working copy must not "+
				"claim to be the released version", got, declared)
		}
	})

	t.Run("a release build reports the stamped version", func(t *testing.T) {
		version, commit, date = "1.2.3", "abcdef1234567890", "2026-01-01T00:00:00Z"
		t.Cleanup(func() { version, commit, date = "", "", "" })

		got := versionString()

		if !strings.Contains(got, "1.2.3") || strings.Contains(got, "-dev") {
			t.Errorf("%q should report the stamped 1.2.3, not a dev version", got)
		}
		if !strings.Contains(got, "abcdef123456") {
			t.Errorf("%q should carry the short commit", got)
		}
		if strings.Contains(got, "abcdef1234567890") {
			t.Errorf("%q should shorten the commit hash", got)
		}
	})
}
