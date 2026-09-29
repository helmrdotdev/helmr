package secretbinding

import "errors"

// Binding is the public declaration of one Secret placed into a
// Computer, either as an environment variable or as a file.
type Binding struct {
	Name string `json:"secret"`
	Env  *Env   `json:"env,omitempty"`
	File *File  `json:"file,omitempty"`
}

type Env struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
}

type File struct {
	Path string `json:"path"`
}

func ValidateBinding(secret Binding) error {
	if secret.Name == "" {
		return errors.New("computer secret name is required")
	}
	hasEnv := secret.Env != nil
	hasFile := secret.File != nil
	if hasEnv == hasFile {
		return errors.New("computer secret must contain exactly one of env or file")
	}
	if hasEnv {
		if secret.Env.Name == "" || (secret.Env.Mode != "raw" && secret.Env.Mode != "protected") {
			return errors.New("computer secret env requires name and explicit raw or protected mode")
		}
		if secret.Env.Mode == "protected" && len(secret.Env.AllowedOrigins) == 0 || secret.Env.Mode == "raw" && secret.Env.AllowedOrigins != nil {
			return errors.New("allowed_origins is required only for protected env")
		}
	} else if secret.File.Path == "" {
		return errors.New("computer secret file path is required")
	}
	return nil
}

// Placements maps structurally valid declarations to unnormalized
// placements in declaration order. File declarations are always raw.
// Callers validate each declaration first and normalize the result with
// Normalize, as NormalizedPlacements does.
func Placements(declarations []Binding) []Placement {
	placements := make([]Placement, 0, len(declarations))
	for _, declaration := range declarations {
		placement := Placement{Name: declaration.Name}
		if declaration.Env != nil {
			placement.Kind, placement.Target, placement.Mode, placement.AllowedOrigins = "env", declaration.Env.Name, declaration.Env.Mode, declaration.Env.AllowedOrigins
		} else {
			placement.Kind, placement.Target, placement.Mode = "file", declaration.File.Path, "raw"
		}
		placements = append(placements, placement)
	}
	return placements
}

// NormalizedPlacements validates each declaration and returns the normalized
// placements they declare.
func NormalizedPlacements(declarations []Binding) ([]Placement, error) {
	for _, declaration := range declarations {
		if err := ValidateBinding(declaration); err != nil {
			return nil, err
		}
	}
	return Normalize(Placements(declarations))
}
