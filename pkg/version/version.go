package version

var (
	Version   = "dev"
	GitCommit = "unknown"
)

func Full() string {
	return Version + " (" + GitCommit + ")"
}
