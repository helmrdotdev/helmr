package builder

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

func TestRequiredBuildSecretsValidatesSecretMounts(t *testing.T) {
	tests := []struct {
		name   string
		mount  *bundlev0.SecretMountBinding
		errMsg string
	}{
		{
			name:   "missing ref",
			mount:  &bundlev0.SecretMountBinding{Dst: "/run/secrets/TOKEN"},
			errMsg: "missing secret_ref",
		},
		{
			name: "invalid ref",
			mount: &bundlev0.SecretMountBinding{
				Dst:       "/run/secrets/TOKEN",
				SecretRef: &bundlev0.SecretRef{Name: "../TOKEN"},
			},
			errMsg: "invalid build secret_ref",
		},
		{
			name: "missing dst",
			mount: &bundlev0.SecretMountBinding{
				SecretRef: &bundlev0.SecretRef{Name: "TOKEN"},
			},
			errMsg: "dst is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RequiredBuildSecrets(&bundlev0.Bundle{
				Image: &bundlev0.ImageSpec{
					Steps: []*bundlev0.ImageStep{{
						Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
							Argv:         []string{"true"},
							SecretMounts: []*bundlev0.SecretMountBinding{tt.mount},
						}},
					}},
				},
			})
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}
}

func TestBuildSecretValuesCopiesOnlyRequiredSecrets(t *testing.T) {
	value := []byte("secret")
	filtered, err := BuildSecretValues(&bundlev0.Bundle{
		Image: &bundlev0.ImageSpec{
			Steps: []*bundlev0.ImageStep{{
				Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
					Argv: []string{"true"},
					SecretMounts: []*bundlev0.SecretMountBinding{{
						Dst:       "/run/secrets/TOKEN",
						SecretRef: &bundlev0.SecretRef{Name: "TOKEN"},
					}},
				}},
			}},
		},
	}, map[string][]byte{
		"TOKEN": value,
		"OTHER": []byte("ignored"),
	})
	if err != nil {
		t.Fatal(err)
	}
	value[0] = 'X'
	if string(filtered["TOKEN"]) != "secret" {
		t.Fatalf("filtered TOKEN = %q", filtered["TOKEN"])
	}
	if _, ok := filtered["OTHER"]; ok {
		t.Fatalf("filtered includes unrequired secret: %+v", filtered)
	}
}
