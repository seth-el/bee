package bee

import (
	"runtime/debug"
	"testing"
)

func TestShortRevision(t *testing.T) {
	if got := shortRevision("2d1370209619c0f3854a763a8d446b215effcc77"); got != "2d13702" {
		t.Fatalf("short revision = %q", got)
	}
	if got := shortRevision(" short "); got != "short" {
		t.Fatalf("short revision trim = %q", got)
	}
}

func TestVersionStringFallsBackToBuildInfo(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "2d1370209619c0f3854a763a8d446b215effcc77"},
			{Key: "vcs.time", Value: "2026-10-23T12:34:56Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	want := "2d13702-dirty (commit 2d13702-dirty, built 2026-10-23T12:34:56Z)"
	if got := versionString("dev", "unknown", "unknown", info, true); got != want {
		t.Fatalf("version string = %q, want %q", got, want)
	}
}

func TestVersionStringPreservesReleaseMetadata(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	want := "v1.2.3 (commit abc1234, built 2026-10-23T12:34:56Z)"
	if got := versionString("v1.2.3", "abc1234", "2026-10-23T12:34:56Z", info, true); got != want {
		t.Fatalf("version string = %q, want %q", got, want)
	}
}

func TestVersionStringDoesNotDuplicateDirtyMarker(t *testing.T) {
	info := &debug.BuildInfo{
		Main:     debug.Module{Version: "v1.2.3+dirty"},
		Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
	}
	want := "v1.2.3+dirty (commit abc1234-dirty, built 2026-10-23T12:34:56Z)"
	if got := versionString("dev", "abc1234", "2026-10-23T12:34:56Z", info, true); got != want {
		t.Fatalf("version string = %q, want %q", got, want)
	}
}
