package artifact

import "math"

// SquashFS facts every Program, Node runtime and build tree artifact carries.
// Artifacts use one exact SquashFS 4.0 profile: zstd compression, fixed data
// block size, no xattrs, and a physical size padded to the alignment.
const (
	SquashFSMagic               = 0x73717368
	SquashFSZstandardCompressor = 6
	SquashFSV0Flags             = 0x0210
	SquashFSSuperblockSize      = 96
	SquashFSPhysicalAlign       = 4096
	SquashFSDataBlockSize       = 131072
	SquashFSInvalidXattr        = math.MaxUint32
)

// SquashFS inode forms recorded in artifact entries.
const (
	SquashFSBasicDirectoryForm    = 1
	SquashFSBasicRegularForm      = 2
	SquashFSBasicSymlinkForm      = 3
	SquashFSBasicBlockDeviceForm  = 4
	SquashFSBasicCharDeviceForm   = 5
	SquashFSBasicFIFOForm         = 6
	SquashFSBasicSocketForm       = 7
	SquashFSExtendedDirectoryForm = 8
	SquashFSExtendedRegularForm   = 9
	SquashFSExtendedSymlinkForm   = 10
	SquashFSExtendedBlockForm     = 11
	SquashFSExtendedCharForm      = 12
	SquashFSExtendedFIFOForm      = 13
	SquashFSExtendedSocketForm    = 14
)

func RoundUpSquashFSSize(value, alignment uint64) (uint64, bool) {
	if alignment == 0 {
		return 0, false
	}
	remainder := value % alignment
	if remainder == 0 {
		return value, true
	}
	increment := alignment - remainder
	if value > ^uint64(0)-increment {
		return 0, false
	}
	return value + increment, true
}
