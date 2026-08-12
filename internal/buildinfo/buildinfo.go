// Package buildinfo exposes compile-time metadata shared across the server.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// The following variables are overridden via ldflags during release builds.
// Defaults cover local development builds.
var (
	// Version is the semantic version or git describe output of the binary.
	Version = "dev"

	// Commit is the git commit SHA baked into the binary.
	Commit = "none"

	// BuildDate records when the binary was built in UTC.
	BuildDate = "unknown"
)

// Resolve fills development defaults from Go's embedded VCS metadata while
// preserving values explicitly injected by release ldflags.
func Resolve(version, commit, buildDate string, info *debug.BuildInfo) (string, string, string) {
	version = strings.TrimSpace(version)
	commit = strings.TrimSpace(commit)
	buildDate = strings.TrimSpace(buildDate)
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "none"
	}
	if buildDate == "" {
		buildDate = "unknown"
	}
	if info == nil {
		return version, commit, buildDate
	}

	revision := ""
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
		}
	}
	if version == "dev" {
		switch {
		case info.Main.Version != "" && info.Main.Version != "(devel)":
			version = info.Main.Version
		case revision != "":
			shortRevision := revision
			if len(shortRevision) > 12 {
				shortRevision = shortRevision[:12]
			}
			version = "dev-" + shortRevision
			if modified {
				version += "-dirty"
			}
		}
	}
	if commit == "none" && revision != "" {
		commit = revision
		if modified {
			commit += "-dirty"
		}
	}
	return version, commit, buildDate
}

// Initialize resolves and publishes build metadata for the running binary.
func Initialize(version, commit, buildDate string) {
	info, _ := debug.ReadBuildInfo()
	Version, Commit, BuildDate = Resolve(version, commit, buildDate, info)
}
