package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestMagicLinkStartSendsLoginLinkForExistingMember(t *testing.T) {
	store := newMagicLinkStartStore()
	store.loginUser = db.User{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DisplayName: "user"}
	mailer := &fakeMagicLinkEmailSender{}
	handler := newMagicLinkStartServerWithConfig(store, mailer, testServerConfig{MagicLinkDebugURLs: true})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"User@Example.Test","next":"/runs"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(mailer.messages) != 1 {
		t.Fatalf("messages = %+v", mailer.messages)
	}
	if got := mailer.messages[0]; got.Email != "user@example.test" || got.Purpose != db.MagicLinkPurposeLogin {
		t.Fatalf("message = %+v", got)
	}
	if store.created.Purpose != db.MagicLinkPurposeLogin || store.created.Email != "user@example.test" || store.created.RedirectAfter.String != "/runs" {
		t.Fatalf("created link = %+v", store.created)
	}
	if store.tx == nil || !store.tx.lockAcquired || !store.tx.markedSent || !store.tx.revoked || !store.tx.committed {
		t.Fatalf("tx = %+v", store.tx)
	}
}

func TestMagicLinkStartDeliveryFailureMarksLinkFailedAndKeepsOldLinks(t *testing.T) {
	store := newMagicLinkStartStore()
	store.loginUser = db.User{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DisplayName: "user"}
	mailer := &fakeMagicLinkEmailSender{err: errors.New("smtp failed")}
	handler := newMagicLinkStartServerWithConfig(store, mailer, testServerConfig{MagicLinkDebugURLs: true})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"user@example.test"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.tx == nil || store.tx.markedSent || store.tx.revoked || !store.tx.committed || !store.deliveryFailed {
		t.Fatalf("tx = %+v deliveryFailed=%v", store.tx, store.deliveryFailed)
	}
}

func TestMagicLinkStartWithoutMailerFailsInsteadOfLoggingByDefault(t *testing.T) {
	store := newMagicLinkStartStore()
	store.loginUser = db.User{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DisplayName: "user"}
	handler := newMagicLinkStartServerWithConfig(store, nil, testServerConfig{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"user@example.test"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.tx != nil {
		t.Fatalf("tx = %+v", store.tx)
	}
}

func TestMagicLinkStartAllowsUnknownEmail(t *testing.T) {
	known := newMagicLinkStartStore()
	known.loginUser = db.User{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DisplayName: "known"}
	knownMailer := &fakeMagicLinkEmailSender{sent: make(chan magicLinkMessage, 1)}
	knownRec := httptest.NewRecorder()
	newMagicLinkStartServer(known, knownMailer).ServeHTTP(
		knownRec,
		httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"known@example.test"}`)),
	)

	unknown := newMagicLinkStartStore()
	unknownMailer := &fakeMagicLinkEmailSender{sent: make(chan magicLinkMessage, 1)}
	unknownRec := httptest.NewRecorder()
	newMagicLinkStartServer(unknown, unknownMailer).ServeHTTP(
		unknownRec,
		httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"unknown@example.test"}`)),
	)

	if knownRec.Code != http.StatusOK || unknownRec.Code != http.StatusOK {
		t.Fatalf("known=%d %s unknown=%d %s", knownRec.Code, knownRec.Body.String(), unknownRec.Code, unknownRec.Body.String())
	}
	if knownRec.Body.String() != unknownRec.Body.String() {
		t.Fatalf("responses differ: known=%s unknown=%s", knownRec.Body.String(), unknownRec.Body.String())
	}
	select {
	case <-knownMailer.sent:
	case <-time.After(time.Second):
		t.Fatal("known magic link was not delivered")
	}
	select {
	case <-unknownMailer.sent:
	case <-time.After(time.Second):
		t.Fatal("unknown magic link was not delivered")
	}
	if len(knownMailer.messages) != 1 || len(unknownMailer.messages) != 1 {
		t.Fatalf("known messages=%d unknown messages=%d", len(knownMailer.messages), len(unknownMailer.messages))
	}
}

func TestMagicLinkStartSendsInviteAcceptLink(t *testing.T) {
	store := newMagicLinkStartStore()
	invitationID := uuid.Must(uuid.NewV7())
	store.invitation = db.GetActiveInvitationRow{
		ID:           pgvalue.UUID(invitationID),
		OrgID:        pgvalue.UUID(store.orgID),
		InviteeEmail: "invited@example.test",
		Role:         db.OrgMemberRoleDeveloper,
	}
	mailer := &fakeMagicLinkEmailSender{}
	handler := newMagicLinkStartServerWithConfig(store, mailer, testServerConfig{MagicLinkDebugURLs: true})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/invite/start", bytes.NewBufferString(`{"token":"invite-token"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.MagicLinkStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Email != "invited@example.test" || response.DebugURL == "" {
		t.Fatalf("response = %+v", response)
	}
	if len(mailer.messages) != 1 {
		t.Fatalf("messages = %+v", mailer.messages)
	}
	if store.created.Purpose != db.MagicLinkPurposeInviteAccept || store.created.InvitationID != pgvalue.UUID(invitationID) {
		t.Fatalf("created link = %+v", store.created)
	}
}

func TestMagicLinkFinishLoginIssuesSession(t *testing.T) {
	dbtx := newMagicLinkFinishDBTX(db.MagicLinkPurposeLogin)
	handler := newMagicLinkFinishServer(dbtx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, magicLinkFinishRequest("raw-token"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !dbtx.tx.consumed || !dbtx.tx.committed {
		t.Fatalf("consumed=%v committed=%v", dbtx.tx.consumed, dbtx.tx.committed)
	}
	if dbtx.tx.createdSession.UserID != pgvalue.UUID(dbtx.userID) {
		t.Fatalf("session = %+v", dbtx.tx.createdSession)
	}
	if !hasCookie(rec.Result().Cookies(), "helmr_session_dev") {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

func TestMagicLinkFinishRejectsWrongExpiredOrConsumedToken(t *testing.T) {
	for _, name := range []string{"wrong", "expired", "consumed"} {
		t.Run(name, func(t *testing.T) {
			dbtx := newMagicLinkFinishDBTX(db.MagicLinkPurposeLogin)
			dbtx.linkErr = pgx.ErrNoRows
			handler := newMagicLinkFinishServer(dbtx)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, magicLinkFinishRequest(name+"-token"))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"error_kind":"invalid_token"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

func TestMagicLinkFinishAcceptsInvitation(t *testing.T) {
	dbtx := newMagicLinkFinishDBTX(db.MagicLinkPurposeInviteAccept)
	handler := newMagicLinkFinishServer(dbtx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, magicLinkFinishRequest("invite-magic-token"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !dbtx.tx.acceptedInvitation || dbtx.tx.ensuredMember.Role != db.OrgMemberRoleDeveloper || !dbtx.tx.consumed {
		t.Fatalf("accepted=%v ensured=%+v consumed=%v", dbtx.tx.acceptedInvitation, dbtx.tx.ensuredMember, dbtx.tx.consumed)
	}
}

type fakeMagicLinkEmailSender struct {
	messages []magicLinkMessage
	err      error
	sent     chan magicLinkMessage
}

func (m *fakeMagicLinkEmailSender) SendEmail(_ context.Context, message email.Message) error {
	if m.err != nil {
		return m.err
	}
	if message.MagicLink == nil {
		return errors.New("expected magic link email")
	}
	magicLink := magicLinkMessage{
		Email:     message.MagicLink.Email,
		Purpose:   db.MagicLinkPurpose(message.MagicLink.Purpose),
		URL:       message.MagicLink.URL,
		ExpiresAt: message.MagicLink.ExpiresAt,
	}
	m.messages = append(m.messages, magicLink)
	if m.sent != nil {
		m.sent <- magicLink
	}
	return nil
}

type magicLinkStartStore struct {
	db.Querier
	orgID          uuid.UUID
	loginUser      db.User
	invitation     db.GetActiveInvitationRow
	created        db.CreateMagicLinkParams
	deliveryFailed bool
	tx             *magicLinkStartTx
}

func newMagicLinkStartStore() *magicLinkStartStore {
	return &magicLinkStartStore{orgID: uuid.Must(uuid.NewV7())}
}

func newMagicLinkStartServer(store *magicLinkStartStore, mailer *fakeMagicLinkEmailSender) http.Handler {
	return newMagicLinkStartServerWithConfig(store, mailer, testServerConfig{})
}

func newMagicLinkStartServerWithConfig(store *magicLinkStartStore, mailer *fakeMagicLinkEmailSender, cfg testServerConfig) http.Handler {
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.DB = store
	cfg.TX = store
	cfg.AuthSecret = []byte(memberTestAuthSecret)
	cfg.PublicURL = mustParseTestURL("https://helmr.example.test")
	if mailer != nil {
		cfg.Mailer = mailer
	}
	return newTestServer(cfg)
}

func (s *magicLinkStartStore) GetMagicLinkLoginUser(context.Context, pgtype.Text) (db.User, error) {
	if !s.loginUser.ID.Valid {
		return db.User{}, pgx.ErrNoRows
	}
	return s.loginUser, nil
}

func (s *magicLinkStartStore) GetActiveInvitation(context.Context, []byte) (db.GetActiveInvitationRow, error) {
	if !s.invitation.ID.Valid {
		return db.GetActiveInvitationRow{}, pgx.ErrNoRows
	}
	return s.invitation, nil
}

func (s *magicLinkStartStore) CountRecentMagicLinks(context.Context, db.CountRecentMagicLinksParams) (int64, error) {
	return 0, nil
}

func (s *magicLinkStartStore) RevokeOpenMagicLinksForRecipient(context.Context, db.RevokeOpenMagicLinksForRecipientParams) (int64, error) {
	return 0, nil
}

func (s *magicLinkStartStore) MarkMagicLinkSent(context.Context, pgtype.UUID) (int64, error) {
	return 1, nil
}

func (s *magicLinkStartStore) MarkMagicLinkDeliveryFailed(context.Context, pgtype.UUID) (int64, error) {
	s.deliveryFailed = true
	return 1, nil
}

func (s *magicLinkStartStore) CreateMagicLink(_ context.Context, arg db.CreateMagicLinkParams) (db.MagicLink, error) {
	s.created = arg
	return db.MagicLink{
		ID:            arg.ID,
		Purpose:       arg.Purpose,
		TokenHash:     arg.TokenHash,
		Email:         arg.Email,
		OrgID:         arg.OrgID,
		InvitationID:  arg.InvitationID,
		RedirectAfter: arg.RedirectAfter,
		ExpiresAt:     arg.ExpiresAt,
	}, nil
}

func (s *magicLinkStartStore) Begin(context.Context) (pgx.Tx, error) {
	s.tx = &magicLinkStartTx{parent: s}
	return s.tx, nil
}

type magicLinkStartTx struct {
	parent       *magicLinkStartStore
	lockAcquired bool
	markedSent   bool
	revoked      bool
	committed    bool
	rolledBack   bool
}

func (tx *magicLinkStartTx) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected nested transaction")
}
func (tx *magicLinkStartTx) Commit(context.Context) error {
	tx.committed = true
	return nil
}
func (tx *magicLinkStartTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}
func (tx *magicLinkStartTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}
func (tx *magicLinkStartTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (tx *magicLinkStartTx) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }
func (tx *magicLinkStartTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}
func (tx *magicLinkStartTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}
func (tx *magicLinkStartTx) Conn() *pgx.Conn { return nil }

func (tx *magicLinkStartTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		tx.lockAcquired = true
		return pgconn.NewCommandTag("SELECT 1"), nil
	case strings.Contains(sql, "SET sent_at"):
		tx.markedSent = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "SET revoked_at"):
		tx.revoked = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	default:
		panic("unexpected Exec: " + sql)
	}
}

func (tx *magicLinkStartTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT count(*)") && strings.Contains(sql, "FROM magic_links"):
		return scanRow{values: []any{int64(0)}}
	case strings.Contains(sql, "INSERT INTO magic_links"):
		arg := db.CreateMagicLinkParams{
			ID:            args[0].(pgtype.UUID),
			Purpose:       args[1].(db.MagicLinkPurpose),
			TokenHash:     args[2].([]byte),
			Email:         args[3].(string),
			OrgID:         args[4].(pgtype.UUID),
			InvitationID:  args[5].(pgtype.UUID),
			RedirectAfter: args[6].(pgtype.Text),
			ExpiresAt:     args[7].(pgtype.Timestamptz),
		}
		tx.parent.created = arg
		return scanRow{values: []any{
			arg.ID,
			arg.Purpose,
			arg.TokenHash,
			arg.Email,
			arg.OrgID,
			arg.InvitationID,
			arg.RedirectAfter,
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			arg.ExpiresAt,
			pgtype.Timestamptz{},
			pgtype.UUID{},
			pgtype.Timestamptz{},
		}}
	default:
		return scanRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}
}

type magicLinkFinishDBTX struct {
	orgID   uuid.UUID
	userID  uuid.UUID
	link    db.GetActiveMagicLinkByTokenHashRow
	linkErr error
	tx      *magicLinkFinishTx
}

func newMagicLinkFinishDBTX(purpose db.MagicLinkPurpose) *magicLinkFinishDBTX {
	orgID := uuid.Must(uuid.NewV7())
	userID := uuid.Must(uuid.NewV7())
	invitationID := pgtype.UUID{}
	if purpose == db.MagicLinkPurposeInviteAccept {
		invitationID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	}
	return &magicLinkFinishDBTX{
		orgID:  orgID,
		userID: userID,
		link: db.GetActiveMagicLinkByTokenHashRow{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Purpose:       purpose,
			Email:         "invited@example.test",
			OrgID:         pgvalue.UUID(orgID),
			InvitationID:  invitationID,
			RedirectAfter: pgtype.Text{String: "/runs", Valid: true},
			ExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		},
	}
}

func newMagicLinkFinishServer(dbtx *magicLinkFinishDBTX) http.Handler {
	return newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DBTX: dbtx, AuthSecret: []byte(memberTestAuthSecret), PublicURL: mustParseTestURL("https://helmr.example.test")})
}

func magicLinkFinishRequest(token string) *http.Request {
	body, _ := json.Marshal(api.MagicLinkFinishRequest{Token: token})
	return httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/finish", bytes.NewReader(body))
}

func (dbtx *magicLinkFinishDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec outside transaction")
}

func (dbtx *magicLinkFinishDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (dbtx *magicLinkFinishDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return scanRow{err: errors.New("unexpected QueryRow outside transaction")}
}

func (dbtx *magicLinkFinishDBTX) Begin(context.Context) (pgx.Tx, error) {
	dbtx.tx = &magicLinkFinishTx{parent: dbtx, consumeRows: 1}
	return dbtx.tx, nil
}

type magicLinkFinishTx struct {
	parent             *magicLinkFinishDBTX
	consumeRows        int64
	lockAcquired       bool
	acceptedInvitation bool
	consumed           bool
	committed          bool
	ensuredMember      db.EnsureOrgMemberParams
	createdSession     db.CreateSessionParams
}

func (tx *magicLinkFinishTx) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected nested transaction")
}
func (tx *magicLinkFinishTx) Commit(context.Context) error {
	tx.committed = true
	return nil
}
func (tx *magicLinkFinishTx) Rollback(context.Context) error { return nil }
func (tx *magicLinkFinishTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}
func (tx *magicLinkFinishTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (tx *magicLinkFinishTx) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }
func (tx *magicLinkFinishTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}
func (tx *magicLinkFinishTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}
func (tx *magicLinkFinishTx) Conn() *pgx.Conn { return nil }

func (tx *magicLinkFinishTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		tx.lockAcquired = true
		return pgconn.NewCommandTag("SELECT 1"), nil
	case strings.Contains(sql, "UPDATE magic_links"):
		tx.consumed = tx.consumeRows > 0
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", tx.consumeRows)), nil
	case strings.Contains(sql, "UPDATE invitations"):
		tx.acceptedInvitation = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "UPDATE sessions"):
		return pgconn.NewCommandTag("UPDATE 0"), nil
	default:
		panic("unexpected Exec: " + sql)
	}
}

func (tx *magicLinkFinishTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	parent := tx.parent
	switch {
	case strings.Contains(sql, "FROM magic_links"):
		if parent.linkErr != nil {
			return scanRow{err: parent.linkErr}
		}
		return scanRow{values: []any{
			parent.link.ID,
			parent.link.Purpose,
			parent.link.Email,
			parent.link.OrgID,
			parent.link.InvitationID,
			parent.link.RedirectAfter,
			parent.link.ExpiresAt,
		}}
	case strings.Contains(sql, "WITH upserted_user") && strings.Contains(sql, "INSERT INTO auth_identities"):
		return scanRow{values: []any{
			pgvalue.UUID(parent.userID),
			args[1].(string),
			args[2].(pgtype.Text),
			args[3].(pgtype.Text),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
		}}
	case strings.Contains(sql, "SELECT EXISTS"):
		return scanRow{values: []any{false}}
	case strings.Contains(sql, "FROM invitations"):
		return scanRow{values: []any{
			parent.link.InvitationID,
			pgvalue.UUID(parent.orgID),
			parent.link.Email,
			db.OrgMemberRoleDeveloper,
		}}
	case strings.Contains(sql, "GetOrgMemberForManagement") || strings.Contains(sql, "FROM org_members") && strings.Contains(sql, "users.primary_email"):
		return scanRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "auth_identities.user_id"):
		return scanRow{values: []any{
			pgvalue.UUID(parent.userID),
			pgvalue.UUID(parent.orgID),
			db.OrgMemberRoleDeveloper,
		}}
	case strings.Contains(sql, "INSERT INTO org_members"):
		tx.ensuredMember = db.EnsureOrgMemberParams{
			OrgID:       args[0].(pgtype.UUID),
			UserID:      args[1].(pgtype.UUID),
			Role:        args[2].(db.OrgMemberRole),
			DisplayName: args[3].(pgtype.Text),
		}
		return scanRow{values: []any{
			args[0].(pgtype.UUID),
			args[1].(pgtype.UUID),
			args[2].(db.OrgMemberRole),
			args[3].(pgtype.Text),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
		}}
	case strings.Contains(sql, "INSERT INTO sessions"):
		tx.createdSession = db.CreateSessionParams{
			ID:        args[0].(pgtype.UUID),
			OrgID:     args[1].(pgtype.UUID),
			UserID:    args[2].(pgtype.UUID),
			TokenHash: args[3].([]byte),
			ExpiresAt: args[4].(pgtype.Timestamptz),
		}
		return scanRow{values: []any{
			args[0].(pgtype.UUID),
			args[1].(pgtype.UUID),
			args[2].(pgtype.UUID),
			args[3].([]byte),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			args[4].(pgtype.Timestamptz),
			pgtype.Timestamptz{},
		}}
	default:
		return scanRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}
}
