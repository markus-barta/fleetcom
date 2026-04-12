package version

import "runtime"

var (
	Version   = "0.4.0"
	Commit    = "dev"
	BuildTime = "unknown"
)

func GoVersion() string {
	return runtime.Version()
}
