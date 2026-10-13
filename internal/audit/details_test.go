package audit

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDetailsPreserveActionFacts(t *testing.T) {
	expires := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	seconds := int64(86400)
	cases := []struct {
		action  string
		details Details
		want    string
	}{
		{"member.role_changed", RoleChangedDetails{OldRole: "developer", NewRole: "viewer"}, `{"old_role":"developer","new_role":"viewer"}`},
		{"member.granted", RoleGrantedDetails{NewRole: "owner"}, `{"new_role":"owner"}`},
		{"installation_admin.granted", RoleGrantedDetails{NewRole: "admin"}, `{"new_role":"admin"}`},
		{"invitation.accepted", RoleGrantedDetails{NewRole: "viewer"}, `{"new_role":"viewer"}`},
		{"member.removed", RoleRemovedDetails{OldRole: "developer"}, `{"old_role":"developer"}`},
		{"invitation.revoked", RoleRemovedDetails{OldRole: "viewer"}, `{"old_role":"viewer"}`},
		{"invitation.expired", RoleRemovedDetails{OldRole: "viewer"}, `{"old_role":"viewer"}`},
		{"invitation.created", InvitationCreatedDetails{NewRole: "viewer", ExpiresAt: &expires}, `{"new_role":"viewer","expires_at":"2026-10-14T12:00:00Z"}`},
		{"api_key.issued", APIKeyIssuedDetails{NewRole: "owner", Permissions: []string{"sessions.read"}}, `{"new_role":"owner","permissions":["sessions.read"],"expires_at":null}`},
		{"api_key.issued", APIKeyIssuedDetails{NewRole: "viewer", Permissions: []string{"sessions.read"}, ExpiresAt: &expires}, `{"new_role":"viewer","permissions":["sessions.read"],"expires_at":"2026-10-14T12:00:00Z"}`},
		{"slack_connection.authorized", PermissionsDetails{Permissions: []string{"chat:write"}}, `{"permissions":["chat:write"]}`},
		{"project.retention_changed", RetentionChangedDetails{OldRetentionSeconds: &seconds}, `{"old_retention_seconds":86400,"new_retention_seconds":null}`},
		{"secret.rotated", nil, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			got, err := marshalDetails(tc.action, tc.details)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if err := json.Unmarshal(got, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("details=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestDetailsRejectInvalidActionFacts(t *testing.T) {
	negative := int64(-1)
	zero := time.Time{}
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	var typedNil *RoleChangedDetails
	cases := []struct {
		name, action string
		details      Details
	}{
		{"missing details", "member.role_changed", nil},
		{"typed nil", "member.role_changed", typedNil},
		{"wrong shape", "member.role_changed", RoleGrantedDetails{NewRole: "viewer"}},
		{"missing before", "member.role_changed", RoleChangedDetails{NewRole: "viewer"}},
		{"missing grant", "member.granted", RoleGrantedDetails{}},
		{"missing revoked role", "member.removed", RoleRemovedDetails{}},
		{"missing invitation expiry", "invitation.created", InvitationCreatedDetails{NewRole: "viewer"}},
		{"zero invitation expiry", "invitation.created", InvitationCreatedDetails{NewRole: "viewer", ExpiresAt: &zero}},
		{"missing key role", "api_key.issued", APIKeyIssuedDetails{}},
		{"missing key permissions", "api_key.issued", APIKeyIssuedDetails{NewRole: "owner"}},
		{"empty permission", "slack_connection.authorized", PermissionsDetails{Permissions: []string{""}}},
		{"missing permissions object", "slack_connection.authorized", nil},
		{"negative retention duration", "project.retention_changed", RetentionChangedDetails{OldRetentionSeconds: &negative}},
		{"unexpected retention duration", "project.retention_changed", RetentionChangedDetails{NewRetentionSeconds: &negative}},
		{"unapproved facts", "secret.rotated", RoleGrantedDetails{NewRole: "owner"}},
		{"invalid timestamp encoding", "api_key.issued", APIKeyIssuedDetails{NewRole: "owner", Permissions: []string{"sessions.read"}, ExpiresAt: &unencodable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := marshalDetails(tc.action, tc.details); err == nil {
				t.Fatal("invalid details accepted")
			}
		})
	}
}

func TestDetailsNormalizeExpiryToUTC(t *testing.T) {
	expires := time.Date(2026, 10, 14, 21, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	for _, tc := range []struct {
		action  string
		details Details
	}{
		{"invitation.created", InvitationCreatedDetails{NewRole: "viewer", ExpiresAt: &expires}},
		{"api_key.issued", APIKeyIssuedDetails{NewRole: "viewer", Permissions: []string{"sessions.read"}, ExpiresAt: &expires}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			data, err := marshalDetails(tc.action, tc.details)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ExpiresAt string `json:"expires_at"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.ExpiresAt != "2026-10-14T12:00:00Z" {
				t.Fatalf("expiry=%s", got.ExpiresAt)
			}
		})
	}
	if _, offset := expires.Zone(); offset != 9*60*60 {
		t.Fatal("serialization mutated source timestamp")
	}
}
