// Package version holds the version of the running build.
package version

// Version is stamped at link time (see tools/version.sh):
//
//	go build -ldflags "-X github.com/tattsun/collie/internal/version.Version=0.3.0"
//
// It is the release tag without the "v" for releases, `git describe`
// output (e.g. 0.3.0-4-gabc1234) for builds between releases, and "dev"
// when nothing was stamped.
var Version = "dev"
