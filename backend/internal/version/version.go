package version

import "runtime"

var (
	Version      = "0.4.1"
	Commit       = "dev"
	BuildTime    = "unknown"
	AgentVersion = "0.1.0"
)

func GoVersion() string {
	return runtime.Version()
}
