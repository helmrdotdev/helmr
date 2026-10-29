package auth

import "errors"

// PermissionRefusal travels with the owner's error after rollback. It records
// the evaluated principal and checks, not a later reconstruction of policy.
type PermissionRefusal struct {
	Principal Principal
	Checks    []PermissionEvaluation
}

func (*PermissionRefusal) Error() string { return "permission denied" }
func Require(p Principal, permission Permission, scope Scope, denied error) error {
	checks, ok := p.Evaluate(permission, scope)
	if ok {
		return RequireRecentFor(p, permission)
	}
	return errors.Join(denied, &PermissionRefusal{Principal: p, Checks: checks})
}
