package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestSecretEnvNameContract(t *testing.T) {
	data, err := os.ReadFile("testdata/secret-env-names.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string
		Reserved bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if ReservedSecretEnv(c.Name) != c.Reserved {
			t.Errorf("%s: reserved mismatch", c.Name)
		}
		for _, mode := range []string{"raw", "protected"} {
			p := SecretPlacement{Name: "token", Kind: "env", Target: c.Name, Mode: mode}
			if mode == "protected" {
				p.AllowedOrigins = []string{"https://example.com"}
			}
			_, err := NormalizeSecretPlacements([]SecretPlacement{p})
			if (err != nil) != c.Reserved {
				t.Errorf("%s %s: %v", c.Name, mode, err)
			}
		}
	}
}
func TestSecretOriginAggregateContract(t *testing.T) {
	for _, variant := range []string{"distinct", "repeated", "ports", "dedup"} {
		t.Run(variant, func(t *testing.T) {
			placements := []SecretPlacement{}
			for i := 0; i < 16; i++ {
				p := SecretPlacement{Name: "token", Kind: "env", Target: fmt.Sprintf("TOKEN_%d", i), Mode: "protected"}
				for j := 0; j < 16; j++ {
					host := fmt.Sprintf("https://h%d.example.com", i*16+j)
					switch variant {
					case "repeated":
						host = fmt.Sprintf("https://h%d.example.com", j)
					case "ports":
						host = fmt.Sprintf("https://example.com:%d", 1000+j)
					case "dedup":
						host = "https://EXAMPLE.com:443/"
					}
					p.AllowedOrigins = append(p.AllowedOrigins, host)
				}
				placements = append(placements, p)
			}
			normalized, err := NormalizeSecretPlacements(placements)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "dedup" && (placements[0].AllowedOrigins[0] != "https://EXAMPLE.com:443/" || len(normalized[0].AllowedOrigins) != 1) {
				t.Fatal("dedup mutated input or failed")
			}
			placements = append(placements, SecretPlacement{Name: "token", Kind: "env", Target: "EXTRA", Mode: "protected", AllowedOrigins: []string{"https://extra.example.com"}})
			_, err = NormalizeSecretPlacements(placements)
			if (err == nil) != (variant == "dedup") {
				t.Fatalf("257 boundary: %v", err)
			}
		})
	}
}
