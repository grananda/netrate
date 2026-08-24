package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Stamped by the release build through -ldflags. Any other build — go build,
// go install, go run — leaves them empty and falls back to the build info the
// Go toolchain records inside the binary itself, so a locally built netrate
// still reports something truthful instead of "unknown".
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

	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" {
			v = info.Main.Version // "(devel)" for a plain go build
		}
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

	if v == "" {
		v = "unknown"
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
