package version

import "runtime"

var (
	Version   = "0.2.0"
	Commit    = "dev"
	BuildTime = "unknown"
)

func GoVersion() string {
	return runtime.Version()
}
