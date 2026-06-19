package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicaccess"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestValidateChannelName(t *testing.T) {
	valid := []string{
		"approval",
		"release.approval",
		"release_approval",
		"release-approval",
		"a1",
	}
	for _, stream := range valid {
		if err := validateChannelName(stream); err != nil {
			t.Fatalf("validateChannelName(%q) returned error: %v", stream, err)
		}
	}

	invalid := []string{
		"",
		" release",
		".release",
		"_release",
		"-release",
		"release approval",
		"release/approval",
		"release%2Fapproval",
		"release:approval",
		"å",
	}
	for _, stream := range invalid {
		if err := validateChannelName(stream); err == nil {
			t.Fatalf("validateChannelName(%q) returned nil error", stream)
		}
	}
}

func TestNormalizeChannelWaitpointParamsRejectsUnsafeChannelNames(t *testing.T) {
	for _, stream := range []string{"release/approval", "release approval", "release%2Fapproval", ".approval", "_approval", "-approval", "å"} {
		if _, err := normalizeChannelWaitpointParams([]byte(`{"channel":"` + stream + `","after_sequence":0}`)); err == nil {
			t.Fatalf("normalizeChannelWaitpointParams(%q) returned nil error", stream)
		}
	}

	normalized, err := normalizeChannelWaitpointParams([]byte(`{"channel":" release.approval ","after_sequence":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized) != `{"channel":"release.approval","after_sequence":3}` {
		t.Fatalf("normalized params = %s", normalized)
	}
}

func TestPublicAccessTokenAllowsChannelRequiresSessionBoundScope(t *testing.T) {
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000321")
	channelID := uuid.MustParse("00000000-0000-0000-0000-000000000654")
	channel := db.Channel{
		ID:            pgvalue.UUID(channelID),
		TaskSessionID: pgvalue.UUID(sessionID),
		Name:          "approval",
		Direction:     db.ChannelDirectionInput,
	}
	scopes, err := json.Marshal([]publicAccessTokenScope{{
		Type:      "session.input.append",
		SessionID: sessionID.String(),
		Channel:   "approval",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !publicAccessTokenAllowsChannel(scopes, "session.input.append", channel, "") {
		t.Fatal("session-bound channel scope was rejected")
	}

	channelIDOnly := []byte(`[{"type":"channel.append","channelId":"` + channelID.String() + `"}]`)
	if publicAccessTokenAllowsChannel(channelIDOnly, "session.input.append", channel, "") {
		t.Fatal("channel id-only scope was accepted")
	}

}

func TestPublicAccessTokenAllowsChannelChecksCorrelationAcrossScopes(t *testing.T) {
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000321")
	channel := db.Channel{
		TaskSessionID: pgvalue.UUID(sessionID),
		Name:          "approval",
		Direction:     db.ChannelDirectionInput,
	}
	scopes, err := json.Marshal([]publicAccessTokenScope{
		{
			Type:          "session.input.append",
			SessionID:     sessionID.String(),
			Channel:       "approval",
			CorrelationID: "other",
		},
		{
			Type:          "session.input.append",
			SessionID:     sessionID.String(),
			Channel:       "approval",
			CorrelationID: "target",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !publicAccessTokenAllowsChannel(scopes, "session.input.append", channel, "target") {
		t.Fatal("matching later correlation-bound scope was rejected")
	}
	if publicAccessTokenAllowsChannel(scopes, "session.input.append", channel, "missing") {
		t.Fatal("non-matching correlation-bound scope was accepted")
	}
}

func TestCreateSessionChannelPublicAccessTokenStoresSessionInputScope(t *testing.T) {
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000321")
	channelID := uuid.MustParse("00000000-0000-0000-0000-000000000654")
	channel := db.Channel{
		ID:            pgvalue.UUID(channelID),
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		TaskSessionID: pgvalue.UUID(sessionID),
		Name:          "approval",
		Direction:     db.ChannelDirectionInput,
	}
	expiresAt := time.Now().Add(time.Hour)
	store := &fakeStore{}
	rawToken, row, err := createSessionChannelPublicAccessToken(context.Background(), store, []byte(waitpointTokenTestAuthSecret), sessionChannelPublicAccessTokenGrant{
		Channel:       channel,
		ScopeType:     "session.input.append",
		CorrelationID: "thread-1",
		ExpiresAt:     expiresAt,
		MaxUses:       pgtype.Int4{Int32: 1, Valid: true},
		CreatedBy:     json.RawMessage(`{"type":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !publicaccess.IsToken(rawToken) {
		t.Fatalf("raw token has unexpected prefix: %s", rawToken)
	}
	if len(row.TokenHash) == 0 || bytes.Contains(row.TokenHash, []byte(rawToken)) {
		t.Fatal("stored token hash is empty or contains the raw token")
	}
	if store.createPublicAccessToken.OrgID != channel.OrgID ||
		store.createPublicAccessToken.ProjectID != channel.ProjectID ||
		store.createPublicAccessToken.EnvironmentID != channel.EnvironmentID {
		t.Fatalf("token scope row = %+v", store.createPublicAccessToken)
	}
	if !store.createPublicAccessToken.MaxUses.Valid || store.createPublicAccessToken.MaxUses.Int32 != 1 {
		t.Fatalf("max uses = %+v", store.createPublicAccessToken.MaxUses)
	}
	if !publicAccessTokenAllowsChannel(row.AllowedScopes, "session.input.append", channel, "thread-1") {
		t.Fatal("created input append token does not authorize its session channel")
	}
	if publicAccessTokenAllowsChannel(row.AllowedScopes, "session.input.append", channel, "other") {
		t.Fatal("created correlation-bound token authorized a different correlation")
	}
	var scopes []publicAccessTokenScope
	if err := json.Unmarshal(row.AllowedScopes, &scopes); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].Type != "session.input.append" || scopes[0].SessionID != sessionID.String() || scopes[0].Channel != "approval" || scopes[0].CorrelationID != "thread-1" {
		t.Fatalf("allowed scopes = %+v", scopes)
	}
	var metadata struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		ChannelID string `json:"channelId"`
		Channel   string `json:"channel"`
		Direction string `json:"direction"`
	}
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Type != "sessionChannel" || metadata.SessionID != sessionID.String() || metadata.ChannelID != channelID.String() || metadata.Channel != "approval" || metadata.Direction != "input" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestCreateSessionChannelPublicAccessTokenValidatesDirectionAndSession(t *testing.T) {
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000321")
	channel := db.Channel{
		ID:            pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000654")),
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		TaskSessionID: pgvalue.UUID(sessionID),
		Name:          "events",
		Direction:     db.ChannelDirectionOutput,
	}
	store := &fakeStore{}
	if _, _, err := createSessionChannelPublicAccessToken(context.Background(), store, []byte(waitpointTokenTestAuthSecret), sessionChannelPublicAccessTokenGrant{
		Channel:   channel,
		ScopeType: "session.input.append",
	}); errorStatus(err) != http.StatusBadRequest {
		t.Fatalf("output channel append grant error = %v", err)
	}
	if store.createPublicAccessToken.ID.Valid {
		t.Fatalf("invalid direction created token: %+v", store.createPublicAccessToken)
	}
	rawToken, row, err := createSessionChannelPublicAccessToken(context.Background(), store, []byte(waitpointTokenTestAuthSecret), sessionChannelPublicAccessTokenGrant{
		Channel:   channel,
		ScopeType: "session.output.read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !publicaccess.IsToken(rawToken) || !publicAccessTokenAllowsChannel(row.AllowedScopes, "session.output.read", channel, "") {
		t.Fatal("created output read token does not authorize its session channel")
	}
}

func TestChannelRecordFingerprintIncludesRecordEnvelope(t *testing.T) {
	base := channelRecordFingerprintInput{
		SessionID:       "session-1",
		ChannelID:       "channel-1",
		Channel:         "approval",
		Direction:       "input",
		ContentType:     "application/json",
		CorrelationID:   "thread-1",
		Source:          "public_access_token",
		AuthSubjectType: "public_access_token",
		AuthSubjectID:   "token-1",
		ExternalEventID: "event-1",
		Actor:           json.RawMessage(`{"type":"public_access_token"}`),
		Data:            json.RawMessage(`{"approved":true}`),
	}
	first, err := channelRecordFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	sameCanonical := base
	sameCanonical.Data = json.RawMessage(`{"approved": true}`)
	second, err := channelRecordFingerprint(sameCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("canonical data changed fingerprint: %s != %s", second, first)
	}
	changedEvent := base
	changedEvent.ExternalEventID = "event-2"
	third, err := channelRecordFingerprint(changedEvent)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("external event id did not change fingerprint")
	}
	changedCorrelation := base
	changedCorrelation.CorrelationID = "thread-2"
	fourth, err := channelRecordFingerprint(changedCorrelation)
	if err != nil {
		t.Fatal(err)
	}
	if fourth == first {
		t.Fatal("correlation id did not change fingerprint")
	}
}

func TestExistingChannelRecordMismatchReturnsConflict(t *testing.T) {
	existing := db.ChannelRecord{
		CorrelationID:          "thread-1",
		IdempotencyFingerprint: "fingerprint-1",
	}

	if err := ensureExistingChannelRecordMatches(existing, "thread-2", "fingerprint-1", "idempotency key"); errorStatus(err) != http.StatusConflict {
		t.Fatalf("correlation mismatch status = %d err=%v", errorStatus(err), err)
	}
	if err := ensureExistingChannelRecordMatches(existing, "thread-1", "fingerprint-2", "external event id"); errorStatus(err) != http.StatusConflict {
		t.Fatalf("fingerprint mismatch status = %d err=%v", errorStatus(err), err)
	}
	if err := ensureExistingChannelRecordMatches(existing, "thread-1", "fingerprint-1", "idempotency key"); err != nil {
		t.Fatalf("matching record returned error: %v", err)
	}
}
