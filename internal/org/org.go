// Package org owns organizations and what they contain: members, invitations,
// projects and environments. Operations take domain inputs, own their
// transactions and return the errors declared here; callers map them to their
// transport.
package org

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

var (
	ErrOrganizationExists       = errors.New("organization already exists")
	ErrOrganizationSlugInUse    = errors.New("organization slug is already in use")
	ErrProjectNotFound          = errors.New("project not found")
	ErrProjectSlugInUse         = errors.New("project slug is already in use")
	ErrEnvironmentNotFound      = errors.New("environment not found")
	ErrEnvironmentSlugInUse     = errors.New("environment slug is already in use")
	ErrNoRegion                 = errors.New("no region configured")
	ErrDefaultRegionNotFound    = errors.New("default region not found")
	ErrMemberNotFound           = errors.New("member not found")
	ErrMemberRoleChanged        = errors.New("member role changed")
	ErrMemberManagementRequired = errors.New("member management permission is required")
	ErrOwnerRoleRequired        = errors.New("owner role is required to manage owners")
	ErrLastActiveOwner          = errors.New("cannot remove or demote the last active owner")
	ErrSelfMemberManagement     = errors.New("cannot remove your own member management access")
	ErrSelfMemberRemoval        = errors.New("cannot remove your own member access")
	ErrInvitationNotFound       = errors.New("invitation not found")
	ErrInvitationPending        = errors.New("pending invitation already exists for email")
	ErrInvitationActiveMember   = errors.New("active member already exists for email")
	ErrAlreadyMember            = errors.New("user is already an active member of the organization")
	ErrUserDisabled             = errors.New("user is disabled")
)

// InputError reports a caller-supplied value that the organization domain
// rejects, such as a malformed slug or a reference to a missing project.
type InputError struct {
	message string
}

func (e InputError) Error() string {
	return e.message
}

func invalidInput(format string, args ...any) error {
	return InputError{message: fmt.Sprintf(format, args...)}
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// normalizeSlugName canonicalizes the slug and display name shared by
// organizations, projects and environments. An empty name takes the slug.
func normalizeSlugName(slug string, name string) (string, string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	name = strings.TrimSpace(name)
	if !slugPattern.MatchString(slug) {
		return "", "", invalidInput("slug must match %s", slugPattern.String())
	}
	if name == "" {
		name = slug
	}
	if len(name) > 80 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", "", invalidInput("name must be 1-80 characters and contain no control characters")
	}
	return slug, name, nil
}

// normalizeProjectSlugName also rejects UUID slugs, which would make a project
// reference ambiguous.
func normalizeProjectSlugName(slug string, name string) (string, string, error) {
	slug, name, err := normalizeSlugName(slug, name)
	if err != nil {
		return "", "", err
	}
	if _, err := ids.Parse(slug); err == nil {
		return "", "", invalidInput("project slug must not be a UUID")
	}
	return slug, name, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
