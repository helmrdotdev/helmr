package filepack

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"syscall"
	"testing"
)

type failingInput struct{}

func (failingInput) Read([]byte) (int, error) { return 0, syscall.EIO }
func testEmptyPack(t *testing.T) []byte {
	t.Helper()
	header, err := json.Marshal(filepackHeader{Version: 0, Role: MemoryRole, LogicalSize: 0, ChunkSize: filepackChunkSize, Codec: filepackCodecZstd})
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(filepackMagic), binary.BigEndian.AppendUint32(nil, uint32(len(header)))...)
	return append(append(raw, header...), filepackRecordEnd)
}
func TestFilepackClassifiesContentsWithoutClassifyingIO(t *testing.T) {
	valid := testEmptyPack(t)
	for _, test := range []struct {
		name    string
		source  io.Reader
		invalid bool
		want    error
	}{
		{"valid", bytes.NewReader(valid), false, nil},
		{"truncated", bytes.NewReader(valid[:len(valid)-1]), true, io.EOF},
		{"bad magic", bytes.NewReader(bytes.Repeat([]byte{'x'}, len(filepackMagic))), true, ErrInvalidContent},
		{"bad record", bytes.NewReader(append(append([]byte{}, valid[:len(valid)-1]...), 0)), true, ErrInvalidContent},
		{"trailing", bytes.NewReader(append(append([]byte{}, valid...), 0)), true, ErrInvalidContent},
		{"header io", failingInput{}, false, syscall.EIO},
		{"record io", io.MultiReader(bytes.NewReader(valid[:len(valid)-1]), failingInput{}), false, syscall.EIO},
		{"end io", io.MultiReader(bytes.NewReader(valid), failingInput{}), false, syscall.EIO},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnpackFrom(t.Context(), test.source, filepath.Join(t.TempDir(), "out"), MemoryRole, 0)
			if errors.Is(err, ErrInvalidContent) != test.invalid || (test.want != nil && !errors.Is(err, test.want)) || (test.want == nil && err != nil) {
				t.Fatalf("classification: %v", err)
			}
		})
	}
}
