package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	Version, GitCommit, BuildDate = "dev", "unknown", "unknown"
	fromBuildInfo(&debug.BuildInfo{
		Main:     debug.Module{Version: "v0.3.0"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "769a09fabc"}, {Key: "vcs.time", Value: "2026-10-13T14:28:37Z"}},
	})
	if Version != "v0.3.0" || GitCommit != "769a09f" || BuildDate != "2026-10-13T14:28:37Z" {
		t.Fatalf("got %s %s %s", Version, GitCommit, BuildDate)
	}
	// Values stamped by the release build win.
	Version = "0.3.0"
	fromBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}})
	if Version != "0.3.0" {
		t.Fatalf("an ldflags version was overwritten: %s", Version)
	}
	// A local build of the source tree stays "dev".
	Version = "dev"
	fromBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	if Version != "dev" {
		t.Fatalf("a local build should report dev, got %s", Version)
	}
}
