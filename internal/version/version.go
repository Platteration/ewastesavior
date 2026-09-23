// Package version holds the build version and build time of the savior binary.
package version

import (
	"strconv"
	"time"
)

// Set at build time:
//
//	go build -ldflags "-X github.com/platteration/ewastesavior/internal/version.Version=v0.1.0 \
//	                   -X github.com/platteration/ewastesavior/internal/version.BuildUnix=1790000000"
var (
	Version   = "dev"
	BuildUnix = "0"
)

// BuildTime returns the build time, or the zero time when unknown. Nodes use
// it as a floor for the system clock (machines with dead CMOS batteries boot
// in the past).
func BuildTime() time.Time {
	n, err := strconv.ParseInt(BuildUnix, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
