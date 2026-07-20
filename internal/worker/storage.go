package worker

type BuildStorageConfig struct {
	CacheRoot                     string
	ScratchRoot                   string
	WorkDir                       string
	JailerRoot                    string
	RequiredCacheBytes            uint64
	RequiredScratchBytes          uint64
	RequiredScratchAvailableBytes uint64
}

type BuildStorageMount struct {
	Root           string
	MountID        uint64
	Device         string
	Source         string
	CapacityBytes  uint64
	AvailableBytes uint64
}

type BuildStorageProof struct {
	Cache             BuildStorageMount
	Scratch           BuildStorageMount
	WorkDir           string
	JailerRoot        string
	BuildKitRoot      string
	SubstrateCacheDir string
	ArtifactCacheDir  string
}
