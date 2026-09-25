package computer

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"golang.org/x/sys/unix"
)

// walkInitialDiskData skips only filesystem-declared holes. Allocated zero data
// still needs inspection. Extent boundaries are rounded to complete encryption
// blocks, and each block is visited at most once in batches of at most 4 MiB.
// The source must remain quiescent and exclusively owned, including its cursor.
// Unsupported extent seeking is an error, not permission to omit source bytes.
func walkInitialDiskData(ctx context.Context, disk *os.File, capacity int64, visit func(offset, size int64) error) error {
	const block = int64(blockformat.BlockSize)
	const batch = int64(blockformat.MaxChangedBlocks) * block
	for offset := int64(0); offset < capacity; {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := unix.Seek(int(disk.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("seek initial disk data: %w", err)
		}
		if data < offset || data >= capacity {
			return errors.New("initial disk data extent outside capacity")
		}
		hole, err := unix.Seek(int(disk.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek initial disk hole: %w", err)
		}
		if hole <= data || hole > capacity {
			return errors.New("initial disk hole extent outside capacity")
		}
		start := data - data%block
		end := hole
		if remainder := end % block; remainder != 0 {
			end += block - remainder
		}
		for start < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			size := min(batch, end-start)
			if err := visit(start, size); err != nil {
				return err
			}
			start += size
		}
		offset = end
	}
	return ctx.Err()
}
