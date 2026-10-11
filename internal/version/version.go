// Package version reports which ntctl build is running.
package version

// Set at build time with -ldflags -X.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)
