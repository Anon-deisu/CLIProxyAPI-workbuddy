package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolveUsesEmbeddedVCSMetadataForDevelopmentBuild(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-08-12T12:34:56Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, buildDate := Resolve("dev", "none", "unknown", info)
	if version != "dev-0123456789ab-dirty" {
		t.Fatalf("version=%q", version)
	}
	if commit != "0123456789abcdef-dirty" {
		t.Fatalf("commit=%q", commit)
	}
	if buildDate != "unknown" {
		t.Fatalf("buildDate=%q", buildDate)
	}
}

func TestResolvePreservesReleaseMetadata(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "embedded"},
			{Key: "vcs.time", Value: "2026-08-12T12:34:56Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, buildDate := Resolve("v1.2.3", "release-sha", "release-date", info)
	if version != "v1.2.3" || commit != "release-sha" || buildDate != "release-date" {
		t.Fatalf("got %q %q %q", version, commit, buildDate)
	}
}
