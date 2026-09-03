// Package version carries build identity, stamped at link time.
package version

import "runtime/debug"

// Version is set with -ldflags "-X .../internal/version.Version=v0.1.0" at
// release time. Development builds fall back to the VCS revision Go embeds.
var Version = ""

// String returns a human-readable build identifier.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	return "dev-" + rev + dirty
}

// UserAgent is stamped into the inventory so a blueprint can name the build that
// produced it.
func UserAgent() string { return "cloud-echo/" + String() }
