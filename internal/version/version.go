package version

var (
	Version      = "dev"
	SourceCommit = "0000000000000000000000000000000000000000"
)

func String() string {
	return Version + " (" + SourceCommit + ")"
}
