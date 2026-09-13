package origin

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCanonicalSharedVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/origins.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Input, Canonical string }
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Input, func(t *testing.T) {
			got, err := Canonical(v.Input)
			if v.Canonical == "" {
				if err == nil {
					t.Fatalf("accepted %q", got)
				}
			} else if err != nil || got != v.Canonical {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
