package workspace

import (
	"strings"
	"testing"

	"uuid"
)

func TestFencingKeyDerivesStableCapability(t *testing.T) {
	key := make([]byte, FencingKeySize)
	for index := range key {
		key[index] = byte(index)
	}
	fencingKey, err := NewFencingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	input := FenceInput{
		LeaseID:                uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff"),
		WorkspaceID:            uuid.MustParse("ffeeddcc-bbaa-9988-7766-554433221100"),
		OwnershipGeneration:    7,
		WriterGeneration:       11,
		MountFencingGeneration: 13,
	}
	first, err := fencingKey.Derive(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fencingKey.Derive(input)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("replayed capability = %+v, want %+v", second, first)
	}
	if first.Token != "PVPhpFfbVfNqF-ACb681yKthwdUcPeXlUq-dnSq6EM4" ||
		first.Hash != "sha256:6afbab08c7bdefa85ff618f8793eece21d2968f2cd7aa825020b979d567a354f" {
		t.Fatalf("capability = %+v", first)
	}
}

func TestFencingKeyRejectsInvalidKey(t *testing.T) {
	for _, size := range []int{0, FencingKeySize - 1, FencingKeySize + 1} {
		if _, err := NewFencingKey(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d-byte key", size)
		}
	}
	if _, err := (FencingKey{}).Derive(validFenceInput()); err == nil {
		t.Fatal("zero-value key was accepted")
	}
}

func TestFencingKeyRejectsInvalidInput(t *testing.T) {
	key, err := NewFencingKey(make([]byte, FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*FenceInput)
		want string
	}{
		{name: "missing lease", edit: func(input *FenceInput) { input.LeaseID = uuid.Nil() }, want: "lease ID"},
		{name: "missing workspace", edit: func(input *FenceInput) { input.WorkspaceID = uuid.Nil() }, want: "workspace ID"},
		{name: "invalid generation", edit: func(input *FenceInput) { input.WriterGeneration = 0 }, want: "must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := validFenceInput()
			test.edit(&input)
			_, err := key.Derive(input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func validFenceInput() FenceInput {
	return FenceInput{
		LeaseID:                uuid.New(),
		WorkspaceID:            uuid.New(),
		OwnershipGeneration:    1,
		WriterGeneration:       1,
		MountFencingGeneration: 1,
	}
}
