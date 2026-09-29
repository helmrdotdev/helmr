package artifact

import "testing"

func TestValidateEntryRejectsForbiddenForms(t *testing.T) {
	tests := []Entry{
		{
			Path:        ".",
			Kind:        EntrySymlink,
			Form:        SquashFSExtendedSymlinkForm,
			Mode:        0777,
			SizeBytes:   int64(len("target")),
			InodeNumber: 1,
			LinkCount:   1,
			XattrIndex:  SquashFSInvalidXattr,
			LinkTarget:  "target",
		},
		{Path: ".", Kind: EntryBlockDevice, Form: SquashFSBasicBlockDeviceForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntryCharacterDevice, Form: SquashFSBasicCharDeviceForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntryFIFO, Form: SquashFSBasicFIFOForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntrySocket, Form: SquashFSBasicSocketForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntryBlockDevice, Form: SquashFSExtendedBlockForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntryCharacterDevice, Form: SquashFSExtendedCharForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntryFIFO, Form: SquashFSExtendedFIFOForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
		{Path: ".", Kind: EntrySocket, Form: SquashFSExtendedSocketForm, InodeNumber: 1, XattrIndex: SquashFSInvalidXattr},
	}
	for _, entry := range tests {
		if err := validateEntry(entry, RoleProgram); err == nil {
			t.Fatalf("inode form %d was accepted", entry.Form)
		}
	}
}
