package bee

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// These values are replaced by release tooling through -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func VersionString() string {
	info, ok := debug.ReadBuildInfo()
	return versionString(Version, Commit, BuildDate, info, ok)
}

func versionString(version, commit, built string, info *debug.BuildInfo, hasBuildInfo bool) string {
	if hasBuildInfo && info != nil {
		settings := make(map[string]string, len(info.Settings))
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if commit == "unknown" {
			commit = shortRevision(settings["vcs.revision"])
			if commit == "" {
				commit = "unknown"
			}
		}
		if built == "unknown" && strings.TrimSpace(settings["vcs.time"]) != "" {
			built = settings["vcs.time"]
		}
		if version == "dev" {
			if info.Main.Version != "" && info.Main.Version != "(devel)" {
				version = info.Main.Version
			} else if commit != "unknown" {
				version = commit
			}
		}
		if settings["vcs.modified"] == "true" {
			if version != "dev" && !strings.HasSuffix(version, "dirty") {
				version += "-dirty"
			}
			if commit != "unknown" && !strings.HasSuffix(commit, "dirty") {
				commit += "-dirty"
			}
		}
	}
	return fmt.Sprintf("%s (commit %s, built %s)", version, commit, built)
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 7 {
		return revision[:7]
	}
	return revision
}
