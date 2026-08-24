package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// VERSION is the single source of truth for what this code calls itself, and
// the only thing that triggers a release: pushing it to main with a value that
// has no tag yet is what makes the pipeline cut one.
//
// It is embedded rather than duplicated as a constant so the file CI reads and
// the string the binary reports cannot drift apart.
//
//go:embed VERSION
var declaredVersion string

// Stamped by the release build through -ldflags. Any other build — go build,
// go install, go run — leaves them empty and falls back to the embedded
// version and to the build info the Go toolchain records in the binary.
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionString describes the binary as precisely as the way it was built
// allows.
func versionString() string {
	v, c, d := version, commit, date
	dirty := false

	if v == "" {
		// Not a release build: say so rather than let a working copy three
		// commits past the tag claim to be the released version.
		v = strings.TrimSpace(declaredVersion) + "-dev"
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "" {
					c = setting.Value
				}
			case "vcs.time":
				if d == "" {
					d = setting.Value
				}
			case "vcs.modified":
				dirty = dirty || setting.Value == "true"
			}
		}
	}

	if len(c) > 12 {
		c = c[:12] // a short hash is enough to find the commit
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", appName, v)
	if c != "" {
		b.WriteString(" (" + c)
		if dirty {
			b.WriteString("-dirty")
		}
		b.WriteString(")")
	}
	if d != "" {
		b.WriteString(" built " + d)
	}
	fmt.Fprintf(&b, "\n%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return b.String()
}
