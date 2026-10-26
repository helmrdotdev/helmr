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
		{"member.granted", MembershipDetails{MembershipID: "member", IsOwner: true}, `{"membership_id":"member","is_owner":true}`},
		{"platform_admin.granted", PlatformAdminGrantedDetails{Basis: "initial_setup"}, `{"basis":"initial_setup"}`},
		{"invitation.accepted", MembershipDetails{MembershipID: "member"}, `{"membership_id":"member","is_owner":false}`},
		{"member.removed", MembershipDetails{MembershipID: "member"}, `{"membership_id":"member","is_owner":false}`},
		{"invitation.revoked", InvitationStateDetails{}, `{}`},
		{"invitation.expired", InvitationStateDetails{}, `{}`},
		{"invitation.created", InvitationCreatedDetails{ExpiresAt: &expires}, `{"expires_at":"2026-10-14T12:00:00Z"}`},
		{"api_key.issued", APIKeyIssuedDetails{Name: "automation"}, `{"name":"automation","expires_at":null}`},
		{"api_key.issued", APIKeyIssuedDetails{Name: "automation", ExpiresAt: &expires}, `{"name":"automation","expires_at":"2026-10-14T12:00:00Z"}`},
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
	var typedNil *APIKeyChangedDetails
	cases := []struct {
		name, action string
		details      Details
	}{
		{"missing details", "api_key.updated", nil},
		{"typed nil", "api_key.updated", typedNil},
		{"wrong shape", "api_key.updated", PlatformAdminGrantedDetails{Basis: "member"}},
		{"missing before", "api_key.updated", APIKeyChangedDetails{After: APIKeyPolicy{Name: "member"}}},
		{"missing grant", "member.granted", PlatformAdminGrantedDetails{}},
		{"missing revoked role", "member.removed", MembershipDetails{}},
		{"missing invitation expiry", "invitation.created", InvitationCreatedDetails{}},
		{"zero invitation expiry", "invitation.created", InvitationCreatedDetails{ExpiresAt: &zero}},
		{"missing key name", "api_key.issued", APIKeyIssuedDetails{}},
		{"empty permission", "slack_connection.authorized", PermissionsDetails{Permissions: []string{""}}},
		{"missing permissions object", "slack_connection.authorized", nil},
		{"negative retention duration", "project.retention_changed", RetentionChangedDetails{OldRetentionSeconds: &negative}},
		{"unexpected retention duration", "project.retention_changed", RetentionChangedDetails{NewRetentionSeconds: &negative}},
		{"unapproved facts", "secret.rotated", PlatformAdminGrantedDetails{Basis: "owner"}},
		{"invalid timestamp encoding", "api_key.issued", APIKeyIssuedDetails{Name: "automation", ExpiresAt: &unencodable}},
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
		{"invitation.created", InvitationCreatedDetails{ExpiresAt: &expires}},
		{"api_key.issued", APIKeyIssuedDetails{Name: "automation", ExpiresAt: &expires}},
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

func TestDetailsKeepBoundedFailureDiscriminators(t *testing.T) {
	for _, tc := range []struct {
		action, outcome, body string
		valid                 bool
	}{
		{"mcp.refused", "denied", `{"reason":"stale_authority","tool":"list_agents"}`, true},
		{"mcp.refused", "failed", `{"reason":"operation_failed","tool":"send_turn"}`, true},
		{"mcp.refused", "failed", `{"reason":"operation_failed","tool":"untrusted request content"}`, false},
		{"role.created", "failed", `{"reason":"operation_failed","tool":"list_agents"}`, false},
		{"platform_admin.granted", "succeeded", `{"basis":"configured_admin_email"}`, true},
		{"platform_admin.granted", "succeeded", `{"basis":"arbitrary"}`, false},
		{"identity.session_revoked", "succeeded", `{"reason":"membership_removed"}`, true},
		{"identity.session_revoked", "succeeded", `{"method":"membership_removed"}`, false},
		{"message.rejected", "succeeded", `{"reason":"callback_failed"}`, true},
		{"message.rejected", "succeeded", `{"reason":"raw runtime error"}`, false},
	} {
		t.Run(tc.action+"/"+tc.body, func(t *testing.T) {
			_, err := DecodeDetails(tc.action, tc.outcome, []byte(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
