// Package version reports which ntctl build is running.
package version

import "runtime/debug"

// Set at build time with -ldflags -X. A binary built with `go install` has no
// ldflags, so init falls back to what Go recorded about the build.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	fromBuildInfo(info)
}

func fromBuildInfo(info *debug.BuildInfo) {
	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && GitCommit == "unknown" && len(s.Value) >= 7:
			GitCommit = s.Value[:7]
		case s.Key == "vcs.time" && BuildDate == "unknown":
			BuildDate = s.Value
		}
	}
}
