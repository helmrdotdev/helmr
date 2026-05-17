package builder

import (
	"fmt"
	"strings"

	bundlev0 "github.com/helmrdotdev/helmr/internal/gen/helmr/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func BuildSecretValues(bundle *bundlev0.Bundle, values map[string][]byte) (map[string][]byte, error) {
	required, err := RequiredBuildSecrets(bundle)
	if err != nil {
		return nil, err
	}
	if len(required) == 0 {
		return nil, nil
	}
	filtered := make(map[string][]byte, len(required))
	for name := range required {
		value, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("build secret %q is required by image build", name)
		}
		filtered[name] = append([]byte(nil), value...)
	}
	return filtered, nil
}

func RequiredBuildSecrets(bundle *bundlev0.Bundle) (map[string]struct{}, error) {
	required := map[string]struct{}{}
	if bundle == nil {
		return required, nil
	}
	if err := collectBuildSecrets(bundle.Image, bundle.SubImages, nil, required); err != nil {
		return nil, err
	}
	return required, nil
}

func collectBuildSecrets(image *bundlev0.ImageSpec, subImages map[string]*bundlev0.ImageSpec, stack []string, required map[string]struct{}) error {
	if image == nil {
		return nil
	}
	for _, step := range image.Steps {
		if step == nil {
			continue
		}
		switch value := step.GetKind().(type) {
		case *bundlev0.ImageStep_Run:
			if value.Run == nil {
				continue
			}
			for _, mount := range value.Run.SecretMounts {
				if mount.SecretRef == nil || strings.TrimSpace(mount.SecretRef.Name) == "" {
					return fmt.Errorf("secret mount is missing secret_ref")
				}
				if err := secret.ValidateName(mount.SecretRef.Name); err != nil {
					return fmt.Errorf("invalid build secret_ref: %w", err)
				}
				if strings.TrimSpace(mount.Dst) == "" {
					return fmt.Errorf("secret mount dst is required")
				}
				required[mount.SecretRef.Name] = struct{}{}
			}
		case *bundlev0.ImageStep_CopyFromImage:
			if value.CopyFromImage == nil {
				continue
			}
			key := value.CopyFromImage.SrcImageKey
			for _, current := range stack {
				if current == key {
					return fmt.Errorf("copy_from_image sub-image graph contains a cycle at %s", key)
				}
			}
			if subImages[key] == nil {
				return fmt.Errorf("copy_from_image sub-image ImageSpec is missing for %s", key)
			}
			if err := collectBuildSecrets(subImages[key], subImages, append(stack, key), required); err != nil {
				return err
			}
		}
	}
	return nil
}
