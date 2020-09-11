package version

import "strings"

var (
	Program      = "k8e"
	ProgramUpper = strings.ToUpper(Program)
	Version      = "dev"
	GitCommit    = "HEAD"
)
