package secretbinding

import (
	"errors"
	"uuid"
)

// Reference binds a stable Secret identity to an explicit placement. Resolving
// names belongs to authoring; registration and delivery never rebind a name.
type Reference struct {
	SecretID string        `json:"secretId"`
	Env      *ReferenceEnv `json:"env,omitempty"`
	File     *File         `json:"file,omitempty"`
}

type ReferenceEnv struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
}

// NormalizeReferences reuses the placement/origin constraints of ordinary
// Computer bindings. Placement.Name holds the already resolved stable identity.
func NormalizeReferences(refs []Reference) ([]Placement, error) {
	if refs == nil {
		return nil, errors.New("secret bindings must be an array")
	}
	placements := make([]Placement, 0, len(refs))
	for _, ref := range refs {
		if (ref.Env == nil) == (ref.File == nil) {
			return nil, errors.New("secret binding requires exactly one env or file placement")
		}
		p := Placement{Name: ref.SecretID}
		if ref.Env != nil {
			p.Kind, p.Target, p.Mode, p.AllowedOrigins = "env", ref.Env.Name, ref.Env.Mode, ref.Env.AllowedOrigins
		} else {
			p.Kind, p.Target, p.Mode = "file", ref.File.Path, "raw"
		}
		placements = append(placements, p)
	}
	return normalize(placements, func(value string) error {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil() || id.String() != value {
			return errors.New("secret binding requires a canonical stable Secret id")
		}
		return nil
	})
}

// CanonicalReferences returns the normalized target and origin ordering used in
// immutable preparation identity and stored declarations.
func CanonicalReferences(refs []Reference) ([]Reference, error) {
	placements, err := NormalizeReferences(refs)
	if err != nil {
		return nil, err
	}
	result := make([]Reference, 0, len(placements))
	for _, p := range placements {
		r := Reference{SecretID: p.Name}
		if p.Kind == "env" {
			r.Env = &ReferenceEnv{Name: p.Target, Mode: p.Mode, AllowedOrigins: p.AllowedOrigins}
		} else {
			r.File = &File{Path: p.Target}
		}
		result = append(result, r)
	}
	return result, nil
}

func CloneReferences(refs []Reference) []Reference {
	if refs == nil {
		return nil
	}
	result := make([]Reference, len(refs))
	for i, r := range refs {
		if r.Env != nil {
			env := *r.Env
			env.AllowedOrigins = append([]string(nil), env.AllowedOrigins...)
			r.Env = &env
		}
		if r.File != nil {
			file := *r.File
			r.File = &file
		}
		result[i] = r
	}
	return result
}
