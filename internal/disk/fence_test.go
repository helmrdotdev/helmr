package disk

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
		InstanceID:       uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff"),
		ComputerID:       uuid.MustParse("ffeeddcc-bbaa-9988-7766-554433221100"),
		WriterGeneration: 11,
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
	if first.Token != "Wy4eA_Dw-lhmNsXZUdXir6qWdXc_2IaJUreutIuZ_JA" ||
		first.Hash != "sha256:d590391eb3ef1a11a76fc3341b251cc514cf8858f9aa2c43556d70d59fee952e" {
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
		{name: "missing instance", edit: func(input *FenceInput) { input.InstanceID = uuid.Nil() }, want: "instance ID"},
		{name: "missing computer", edit: func(input *FenceInput) { input.ComputerID = uuid.Nil() }, want: "computer ID"},
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
		InstanceID:       uuid.New(),
		ComputerID:       uuid.New(),
		WriterGeneration: 1,
	}
}

func TestFencingCapabilityCannotMoveBetweenInstancesOrGenerations(t *testing.T) {
	key, err := NewFencingKey(make([]byte, FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	original := validFenceInput()
	capability, err := key.Derive(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*FenceInput)
	}{
		{"replacement instance", func(input *FenceInput) { input.InstanceID = uuid.NewV7() }},
		{"another Computer", func(input *FenceInput) { input.ComputerID = uuid.NewV7() }},
		{"new writer generation", func(input *FenceInput) { input.WriterGeneration++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := original
			test.change(&target)
			changed, err := key.Derive(target)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Token == capability.Token || changed.Hash == capability.Hash {
				t.Fatal("write capability remained valid after changing physical authority")
			}
		})
	}
}
