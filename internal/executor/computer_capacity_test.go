package executor

import "testing"

func TestComputerCapacityIncludesDiskProjection(t *testing.T) {
	request, err := runtimeCapacityVectorWithProjection(1000, 512, 1024, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1280 << 20); request.GuestEphemeralDiskBytes != want {
		t.Fatalf("arena disk reservation = %d, want %d", request.GuestEphemeralDiskBytes, want)
	}
}
