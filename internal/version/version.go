// Package version holds the build version of the savior binary.
package version

// Version is overridden at build time:
//
//	go build -ldflags "-X github.com/platteration/ewastesavior/internal/version.Version=v0.1.0"
var Version = "dev"
