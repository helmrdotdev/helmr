package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestActorUpdatePostgresReplacesClearsAndPreservesLifecycleFences(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:update"
	start := fixture.request(0, &key, "update-start")
	start.Metadata = json.RawMessage(`{"before":true}`)
	start.Tags = []string{"before"}
	created, err := fixture.server.startActor(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}

	var beforeStateVersion, beforeRunGeneration int64
	var beforeUpdatedAt time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state_version, run_generation, updated_at
		  FROM actors
		 WHERE id = $1
	`, created.ActorID).Scan(&beforeStateVersion, &beforeRunGeneration, &beforeUpdatedAt); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	status, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:         actorReadAddress{key: key},
		MetadataPresent: true, Metadata: json.RawMessage(`{"z":1,"a":2}`),
		TagsPresent: true, Tags: []string{" beta ", "alpha", "beta"},
		ExpiresAtPresent: true, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != created.ActorPublicID ||
		status.CurrentRunID == nil || *status.CurrentRunID != created.BootRunPublicID ||
		string(status.Metadata) != `{"a": 2, "z": 1}` ||
		len(status.Tags) != 2 || status.Tags[0] != "alpha" || status.Tags[1] != "beta" ||
		status.ExpiresAt == nil || !status.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("updated status = %+v metadata=%s", status, status.Metadata)
	}

	var afterStateVersion, afterRunGeneration int64
	var afterUpdatedAt time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state_version, run_generation, updated_at
		  FROM actors
		 WHERE id = $1
	`, created.ActorID).Scan(&afterStateVersion, &afterRunGeneration, &afterUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if afterStateVersion != beforeStateVersion || afterRunGeneration != beforeRunGeneration {
		t.Fatalf(
			"lifecycle fences changed state=%d->%d run=%d->%d",
			beforeStateVersion, afterStateVersion, beforeRunGeneration, afterRunGeneration,
		)
	}
	if !afterUpdatedAt.After(beforeUpdatedAt) {
		t.Fatalf("updated_at did not advance: %v -> %v", beforeUpdatedAt, afterUpdatedAt)
	}

	time.Sleep(2 * time.Millisecond)
	cleared, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:         actorReadAddress{publicID: created.ActorPublicID},
		MetadataPresent: true, Metadata: json.RawMessage(`{}`),
		TagsPresent: true, Tags: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(cleared.Metadata) != `{}` || len(cleared.Tags) != 0 ||
		cleared.ExpiresAt == nil || !cleared.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("cleared status = %+v metadata=%s", cleared, cleared.Metadata)
	}

	time.Sleep(2 * time.Millisecond)
	same, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:         actorReadAddress{publicID: created.ActorPublicID},
		MetadataPresent: true, Metadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !same.UpdatedAt.After(cleared.UpdatedAt) {
		t.Fatalf("same-value update did not advance updated_at: %v -> %v", cleared.UpdatedAt, same.UpdatedAt)
	}
}

func TestActorUpdatePostgresExpiryConflictsAreAtomic(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:expiry"
	created, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "expiry-start"))
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	if _, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:          actorReadAddress{key: key},
		ExpiresAtPresent: true, ExpiresAt: &expiresAt,
	}); err != nil {
		t.Fatal(err)
	}

	for name, rejectedExpiry := range map[string]time.Time{
		"equal":   expiresAt,
		"shorter": expiresAt.Add(-time.Minute),
		"past":    time.Now().UTC().Add(-time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
				EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
				Address:         actorReadAddress{key: key},
				MetadataPresent: true, Metadata: json.RawMessage(`{"must_not_commit":true}`),
				ExpiresAtPresent: true, ExpiresAt: &rejectedExpiry,
			})
			if !errors.Is(err, errActorUpdateConflict) {
				t.Fatalf("error = %v", err)
			}
			var metadata []byte
			if err := fixture.pool.QueryRow(t.Context(), `
				SELECT metadata FROM actors WHERE id = $1
			`, created.ActorID).Scan(&metadata); err != nil {
				t.Fatal(err)
			}
			if string(metadata) != `{}` {
				t.Fatalf("metadata changed on conflict: %s", metadata)
			}
		})
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors SET expires_at = transaction_timestamp() - interval '1 second' WHERE id = $1
	`, created.ActorID); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(3 * time.Hour)
	_, err = fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:     actorReadAddress{key: key},
		TagsPresent: true, Tags: []string{"must-not-commit"},
		ExpiresAtPresent: true, ExpiresAt: &future,
	})
	if !errors.Is(err, errActorUpdateConflict) {
		t.Fatalf("logically expired update error = %v", err)
	}
	var tags []string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT tags FROM actors WHERE id = $1
	`, created.ActorID).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatalf("tags changed on logically expired conflict: %#v", tags)
	}
}

func TestActorUpdatePostgresAllowsAnnotationsAcrossRetainedLifecycles(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:lifecycle"
	created, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "lifecycle-start"))
	if err != nil {
		t.Fatal(err)
	}
	for _, lifecycle := range []string{
		"open", "closing", "closed", "cancelling", "cancelled", "failed", "expired",
	} {
		failureCode := any(nil)
		failureRunID := any(nil)
		if lifecycle == "failed" {
			failureCode = "run-failed"
			failureRunID = created.BootRunID
		}
		if _, err := fixture.pool.Exec(t.Context(), `
			UPDATE actors
			   SET state = $1,
			       failure_code = $2,
			       failure_run_id = $3
			 WHERE id = $4
		`, lifecycle, failureCode, failureRunID, created.ActorID); err != nil {
			t.Fatal(err)
		}
		status, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
			EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
			Address:         actorReadAddress{key: key},
			MetadataPresent: true,
			Metadata:        json.RawMessage(`{"lifecycle":"` + lifecycle + `"}`),
		})
		if err != nil {
			t.Fatalf("%s: %v", lifecycle, err)
		}
		if string(status.Metadata) != `{"lifecycle": "`+lifecycle+`"}` {
			t.Fatalf("%s metadata = %s", lifecycle, status.Metadata)
		}
		if lifecycle != "open" {
			future := time.Now().UTC().Add(time.Hour)
			if _, err := fixture.server.updateActor(t.Context(), actorUpdateRequest{
				EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
				Address:          actorReadAddress{key: key},
				ExpiresAtPresent: true, ExpiresAt: &future,
			}); !errors.Is(err, errActorUpdateConflict) {
				t.Fatalf("%s expiry error = %v", lifecycle, err)
			}
		}
	}
}

func TestActorUpdateHTTPPostgresUsesUpdateGrantWithoutReadGrant(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:http-update"
	created, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "http-update-start"))
	if err != nil {
		t.Fatal(err)
	}
	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionActorsUpdate},
	}
	request := actorUpdateHTTPTestRequest(
		`{"actor_key":"thread:http-update","metadata":{"updated":true},"tags":[]}`,
		principal,
	)
	recorder := httptest.NewRecorder()
	fixture.server.updateActorHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var status api.ActorStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ID != created.ActorPublicID ||
		status.CurrentRunID == nil || *status.CurrentRunID != created.BootRunPublicID ||
		string(status.Metadata) != `{"updated":true}` {
		t.Fatalf("status = %+v metadata=%s", status, status.Metadata)
	}
}

func TestActorUpdatePostgresDistinguishesNotFoundFromConflict(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:not-found"
	created, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "not-found-start"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors SET state = 'closing' WHERE id = $1
	`, created.ActorID); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	_, err = fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:          actorReadAddress{key: key},
		ExpiresAtPresent: true, ExpiresAt: &future,
	})
	if !errors.Is(err, errActorUpdateConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	_, err = fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
		Address:         actorReadAddress{key: "missing"},
		MetadataPresent: true, Metadata: json.RawMessage(`{}`),
	})
	if !errors.Is(err, errActorUpdateNotFound) {
		t.Fatalf("not-found error = %v", err)
	}
	_, err = fixture.server.updateActor(t.Context(), actorUpdateRequest{
		EnvironmentID: fixture.environmentID, ActorDeclaredID: "other.v1",
		Address:         actorReadAddress{publicID: created.ActorPublicID},
		MetadataPresent: true, Metadata: json.RawMessage(`{}`),
	})
	if !errors.Is(err, errActorUpdateNotFound) {
		t.Fatalf("wrong declaration error = %v", err)
	}
}

func TestActorUpdatePostgresExpiryRaceConvergesOnActorRowLock(t *testing.T) {
	t.Run("extension commits before stale expiry", func(t *testing.T) {
		fixture := newActorStartPostgresFixture(t, 1)
		key := "thread:extension-first"
		created, err := fixture.server.startActor(
			t.Context(),
			fixture.request(0, &key, "extension-first-start"),
		)
		if err != nil {
			t.Fatal(err)
		}
		oldExpiry := time.Now().UTC().Add(150 * time.Millisecond)
		newExpiry := oldExpiry.Add(2 * time.Hour)
		if _, err := fixture.pool.Exec(t.Context(), `
			UPDATE actors
			   SET current_run_id = NULL,
			       expires_at = $1
			 WHERE id = $2
		`, oldExpiry, created.ActorID); err != nil {
			t.Fatal(err)
		}

		tx, err := fixture.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		txQueries := db.New(tx)
		if _, err := txQueries.UpdateActorAnnotations(t.Context(), db.UpdateActorAnnotationsParams{
			SetMetadata: false, Metadata: []byte(`{}`),
			SetTags: false, Tags: []string{},
			SetExpiresAt: true, ExpiresAt: pgvalue.Timestamptz(newExpiry),
			EnvironmentID:   pgvalue.UUID(fixture.environmentID),
			ActorDeclaredID: "operator.v1",
			AddressKey:      pgvalue.Text(key),
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Until(oldExpiry) + 20*time.Millisecond)

		expiryDone := make(chan []db.Actor, 1)
		expiryError := make(chan error, 1)
		go func() {
			rows, err := db.New(fixture.pool).ExpireDueActors(
				context.Background(),
				pgvalue.UUID(fixture.orgID),
			)
			if err != nil {
				expiryError <- err
				return
			}
			expiryDone <- rows
		}()
		select {
		case rows := <-expiryDone:
			t.Fatalf("expiry did not wait for Actor row lock: %+v", rows)
		case err := <-expiryError:
			t.Fatal(err)
		case <-time.After(30 * time.Millisecond):
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case rows := <-expiryDone:
			if len(rows) != 0 {
				t.Fatalf("stale expiry terminalized extended Actor: %+v", rows)
			}
		case err := <-expiryError:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("expiry did not finish")
		}
		var state string
		var storedExpiry time.Time
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT state, expires_at FROM actors WHERE id = $1
		`, created.ActorID).Scan(&state, &storedExpiry); err != nil {
			t.Fatal(err)
		}
		if state != "open" || !storedExpiry.Equal(newExpiry) {
			t.Fatalf("state=%s expires_at=%v want open/%v", state, storedExpiry, newExpiry)
		}
	})

	t.Run("expiry commits before extension", func(t *testing.T) {
		fixture := newActorStartPostgresFixture(t, 1)
		key := "thread:expiry-first"
		created, err := fixture.server.startActor(
			t.Context(),
			fixture.request(0, &key, "expiry-first-start"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(t.Context(), `
			UPDATE actors
			   SET current_run_id = NULL,
			       expires_at = transaction_timestamp() - interval '1 second'
			 WHERE id = $1
		`, created.ActorID); err != nil {
			t.Fatal(err)
		}

		tx, err := fixture.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		expired, err := db.New(tx).ExpireDueActors(
			t.Context(),
			pgvalue.UUID(fixture.orgID),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(expired) != 1 || expired[0].ID.Bytes != created.ActorID {
			t.Fatalf("expired = %+v", expired)
		}

		future := time.Now().UTC().Add(2 * time.Hour)
		updateDone := make(chan error, 1)
		go func() {
			_, err := fixture.server.updateActor(context.Background(), actorUpdateRequest{
				EnvironmentID: fixture.environmentID, ActorDeclaredID: "operator.v1",
				Address:          actorReadAddress{key: key},
				ExpiresAtPresent: true, ExpiresAt: &future,
			})
			updateDone <- err
		}()
		select {
		case err := <-updateDone:
			t.Fatalf("update did not wait for Actor row lock: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-updateDone:
			if !errors.Is(err, errActorUpdateConflict) {
				t.Fatalf("update error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("update did not finish")
		}
	})
}
