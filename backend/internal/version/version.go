package version

import "runtime"

var (
	Version   = "0.3.1"
	Commit    = "dev"
	BuildTime = "unknown"
)

func GoVersion() string {
	return runtime.Version()
}
