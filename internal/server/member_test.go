package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const memberTestAuthSecret = "abcdefghijabcdefghijabcdefghij12"

func TestCreateInvitationReturnsInviteURL(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPost, "/api/invitations", `{"email":"Invited@Example.Test","role":"developer","expires_in_days":3}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createdInvitation.InviteeEmail != "invited@example.test" {
		t.Fatalf("created invitation email = %q", store.createdInvitation.InviteeEmail)
	}
	if store.createdInvitation.Role != db.OrgMemberRoleDeveloper {
		t.Fatalf("created invitation role = %q", store.createdInvitation.Role)
	}
	if len(store.createdInvitation.TokenHash) == 0 {
		t.Fatal("token hash was not stored")
	}
	var response api.CreateInvitationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Email != "invited@example.test" || response.Role != "developer" || response.Status != api.InvitationStatusPending {
		t.Fatalf("response = %+v", response)
	}
	if !strings.HasPrefix(response.InviteURL, "https://helmr.example.test/invite?token=") {
		t.Fatalf("invite_url = %q", response.InviteURL)
	}
}

func TestCreateInvitationRejectsExistingActiveMemberEmail(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	store.createInvitationErr = pgx.ErrNoRows
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPost, "/api/invitations", `{"email":"member@example.test","role":"viewer"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvitationRejectsAdminReplacingOwnerInvite(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleAdmin)
	store.pendingInvitation = db.GetPendingInvitationByEmailRow{
		ID:           ids.ToPG(ids.New()),
		OrgID:        ids.ToPG(store.orgID),
		InviteeEmail: "owner@example.test",
		Role:         db.OrgMemberRoleOwner,
	}
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPost, "/api/invitations", `{"email":"owner@example.test","role":"developer"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createdInvitation.ID.Valid {
		t.Fatalf("invitation was created: %+v", store.createdInvitation)
	}
}

func TestRevokeInvitationRejectsAdminRevokingOwnerInvite(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleAdmin)
	invitationID := ids.New()
	store.revocableInvitation = db.GetRevocableInvitationRow{
		ID:           ids.ToPG(invitationID),
		OrgID:        ids.ToPG(store.orgID),
		InviteeEmail: "owner@example.test",
		Role:         db.OrgMemberRoleOwner,
	}
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodDelete, "/api/invitations/"+invitationID.String(), ``)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.revokedInvitationID.Valid {
		t.Fatalf("invitation was revoked: %+v", store.revokedInvitationID)
	}
}

func TestUpdateMemberRoleRejectsLastOwnerDemotion(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	store.targetMember = managedMember(store.orgID, ids.New(), db.OrgMemberRoleOwner)
	store.activeOwners = 1
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPatch, "/api/members/"+ids.MustFromPG(store.targetMember.UserID).String(), `{"role":"admin","expected_role":"owner"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedRole.Valid {
		t.Fatalf("member role was updated to %q", store.updatedRole.OrgMemberRole)
	}
}

func TestUpdateMemberRoleRejectsSelfDemotionBelowAdmin(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleAdmin)
	store.targetMember = managedMember(store.orgID, store.userID, db.OrgMemberRoleAdmin)
	store.activeOwners = 1
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPatch, "/api/members/"+store.userID.String(), `{"role":"developer","expected_role":"admin"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedRole.Valid {
		t.Fatalf("member role was updated to %q", store.updatedRole.OrgMemberRole)
	}
}

func TestUpdateMemberRoleRejectsStaleExpectedRole(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	targetUserID := ids.New()
	store.targetMember = managedMember(store.orgID, targetUserID, db.OrgMemberRoleDeveloper)
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPatch, "/api/members/"+targetUserID.String(), `{"role":"viewer","expected_role":"admin"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedRole.Valid {
		t.Fatalf("member role was updated to %q", store.updatedRole.OrgMemberRole)
	}
}

func TestUpdateMemberRoleRejectsAdminOwnerPromotion(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleAdmin)
	targetUserID := ids.New()
	store.targetMember = managedMember(store.orgID, targetUserID, db.OrgMemberRoleAdmin)
	store.activeOwners = 1
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodPatch, "/api/members/"+targetUserID.String(), `{"role":"owner","expected_role":"admin"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedRole.Valid {
		t.Fatalf("member role was updated to %q", store.updatedRole.OrgMemberRole)
	}
}

func TestRemoveMemberRejectsSelf(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	store.targetMember = managedMember(store.orgID, store.userID, db.OrgMemberRoleOwner)
	store.activeOwners = 2
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodDelete, "/api/members/"+store.userID.String(), ``)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.disabledMember.UserID.Valid {
		t.Fatalf("member was disabled: %+v", store.disabledMember)
	}
}

func TestRemoveMemberRejectsAdminRemovingOwner(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleAdmin)
	targetUserID := ids.New()
	store.targetMember = managedMember(store.orgID, targetUserID, db.OrgMemberRoleOwner)
	store.activeOwners = 2
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodDelete, "/api/members/"+targetUserID.String(), ``)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.disabledMember.UserID.Valid {
		t.Fatalf("member was disabled: %+v", store.disabledMember)
	}
}

func TestRemoveMemberRevokesSessions(t *testing.T) {
	store := newMemberManagementStore(db.OrgMemberRoleOwner)
	targetUserID := ids.New()
	store.targetMember = managedMember(store.orgID, targetUserID, db.OrgMemberRoleDeveloper)
	store.activeOwners = 1
	server := newMemberManagementServer(store)
	req := memberManagementRequest(http.MethodDelete, "/api/members/"+targetUserID.String(), ``)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.disabledMember.UserID != ids.ToPG(targetUserID) {
		t.Fatalf("disabled member = %+v", store.disabledMember)
	}
	if store.revokedSessionUserID != ids.ToPG(targetUserID) {
		t.Fatalf("revoked session user = %+v", store.revokedSessionUserID)
	}
}

func TestInvitationEmailMatchRequiresVerifiedEmail(t *testing.T) {
	identity := authIdentity{
		Email:          "invited@example.test",
		EmailVerified:  false,
		VerifiedEmails: []string{"Other@Example.Test"},
	}
	if identityMatchesInvitationEmail(identity, "invited@example.test") {
		t.Fatal("unverified primary email matched invitation")
	}
	if !identityMatchesInvitationEmail(identity, "other@example.test") {
		t.Fatal("verified email list did not match invitation")
	}
}

type memberManagementStore struct {
	db.Querier
	orgID                uuid.UUID
	userID               uuid.UUID
	sessionID            uuid.UUID
	role                 db.OrgMemberRole
	pendingInvitation    db.GetPendingInvitationByEmailRow
	revocableInvitation  db.GetRevocableInvitationRow
	revokedInvitationID  pgtype.UUID
	createdInvitation    db.CreateInvitationParams
	createInvitationErr  error
	targetMember         db.GetOrgMemberForManagementRow
	activeOwners         int64
	updatedRole          db.NullOrgMemberRole
	disabledMember       db.OrgMember
	revokedSessionUserID pgtype.UUID
}

func newMemberManagementStore(role db.OrgMemberRole) *memberManagementStore {
	return &memberManagementStore{
		orgID:     ids.New(),
		userID:    ids.New(),
		sessionID: ids.New(),
		role:      role,
	}
}

func newMemberManagementServer(store *memberManagementStore) http.Handler {
	return New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth(memberTestAuthSecret, "https://helmr.example.test"),
	)
}

func memberManagementRequest(method string, path string, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: "helmr_session_dev", Value: "member-test-session"})
	return req
}

func (s *memberManagementStore) GetSessionByTokenHash(context.Context, []byte) (db.GetSessionByTokenHashRow, error) {
	return db.GetSessionByTokenHashRow{
		ID:        ids.ToPG(s.sessionID),
		OrgID:     ids.ToPG(s.orgID),
		UserID:    ids.ToPG(s.userID),
		Role:      string(s.role),
		ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
	}, nil
}

func (s *memberManagementStore) RefreshSession(context.Context, db.RefreshSessionParams) error {
	return nil
}

func (s *memberManagementStore) GetPendingInvitationByEmail(context.Context, db.GetPendingInvitationByEmailParams) (db.GetPendingInvitationByEmailRow, error) {
	if !s.pendingInvitation.ID.Valid {
		return db.GetPendingInvitationByEmailRow{}, pgx.ErrNoRows
	}
	return s.pendingInvitation, nil
}

func (s *memberManagementStore) RevokeExpiredInvitationsByEmail(context.Context, db.RevokeExpiredInvitationsByEmailParams) (int64, error) {
	return 0, nil
}

func (s *memberManagementStore) GetRevocableInvitation(context.Context, db.GetRevocableInvitationParams) (db.GetRevocableInvitationRow, error) {
	if !s.revocableInvitation.ID.Valid {
		return db.GetRevocableInvitationRow{}, pgx.ErrNoRows
	}
	return s.revocableInvitation, nil
}

func (s *memberManagementStore) CreateInvitation(_ context.Context, arg db.CreateInvitationParams) (db.Invitation, error) {
	s.createdInvitation = arg
	if s.createInvitationErr != nil {
		return db.Invitation{}, s.createInvitationErr
	}
	return db.Invitation{
		ID:              arg.ID,
		OrgID:           arg.OrgID,
		InviteeEmail:    arg.InviteeEmail,
		Role:            arg.Role,
		InvitedByUserID: arg.InvitedByUserID,
		TokenHash:       arg.TokenHash,
		CreatedAt:       memberPGTime(),
		ExpiresAt:       arg.ExpiresAt,
	}, nil
}

func (s *memberManagementStore) RevokeInvitation(_ context.Context, arg db.RevokeInvitationParams) (int64, error) {
	s.revokedInvitationID = arg.ID
	return 1, nil
}

func (s *memberManagementStore) GetOrgMemberForManagement(context.Context, db.GetOrgMemberForManagementParams) (db.GetOrgMemberForManagementRow, error) {
	if !s.targetMember.UserID.Valid {
		return db.GetOrgMemberForManagementRow{}, pgx.ErrNoRows
	}
	return s.targetMember, nil
}

func (s *memberManagementStore) UpdateOrgMemberRole(_ context.Context, arg db.UpdateOrgMemberRoleParams) (db.OrgMember, error) {
	if s.targetMember.Role != arg.ExpectedRole {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	if !arg.ActorIsOwner && (s.targetMember.Role == db.OrgMemberRoleOwner || arg.Role == db.OrgMemberRoleOwner) {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	if s.targetMember.Role == db.OrgMemberRoleOwner && arg.Role != db.OrgMemberRoleOwner && s.activeOwners <= 1 {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	s.updatedRole = db.NullOrgMemberRole{OrgMemberRole: arg.Role, Valid: true}
	return db.OrgMember{
		OrgID:     arg.OrgID,
		UserID:    arg.UserID,
		Role:      arg.Role,
		CreatedAt: memberPGTime(),
		UpdatedAt: memberPGTime(),
	}, nil
}

func (s *memberManagementStore) DisableOrgMember(_ context.Context, arg db.DisableOrgMemberParams) (db.OrgMember, error) {
	if s.targetMember.Role != arg.ExpectedRole {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	if !arg.ActorIsOwner && s.targetMember.Role == db.OrgMemberRoleOwner {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	if s.targetMember.Role == db.OrgMemberRoleOwner && s.activeOwners <= 1 {
		return db.OrgMember{}, pgx.ErrNoRows
	}
	s.disabledMember = db.OrgMember{
		OrgID:      arg.OrgID,
		UserID:     arg.UserID,
		Role:       s.targetMember.Role,
		DisabledAt: memberPGTime(),
		CreatedAt:  memberPGTime(),
		UpdatedAt:  memberPGTime(),
	}
	return s.disabledMember, nil
}

func (s *memberManagementStore) RevokeSessionsForUser(_ context.Context, userID pgtype.UUID) (int64, error) {
	s.revokedSessionUserID = userID
	return 1, nil
}

func managedMember(orgID uuid.UUID, userID uuid.UUID, role db.OrgMemberRole) db.GetOrgMemberForManagementRow {
	return db.GetOrgMemberForManagementRow{
		OrgID:        ids.ToPG(orgID),
		UserID:       ids.ToPG(userID),
		Role:         role,
		DisplayName:  pgtype.Text{String: "Member", Valid: true},
		PrimaryEmail: pgtype.Text{String: "member@example.test", Valid: true},
		CreatedAt:    memberPGTime(),
		UpdatedAt:    memberPGTime(),
	}
}

func memberPGTime() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC), Valid: true}
}
