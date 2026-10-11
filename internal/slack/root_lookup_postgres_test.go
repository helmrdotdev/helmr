package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMentionRootReadsSharePacingAndRespectRetryAfter(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	var reads atomic.Int32
	var limited atomic.Bool
	var limitedJSON atomic.Bool
	client := NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/conversations.info" {
			return f.admissionClient(t).http.Do(r)
		}
		if r.URL.Path != "/api/conversations.history" {
			t.Fatalf("unexpected %s", r.URL)
		}
		reads.Add(1)
		if limitedJSON.Load() {
			return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"ratelimited"}`))}, nil
		}
		if limited.Load() {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"messages":[{"ts":%q,"user":"human"}]}`, r.URL.Query().Get("latest"))))}, nil
	}))
	first := f.newMessage(t, "first", "234.567")
	second := f.newMessage(t, "second", "345.678")
	for _, id := range []uuid.UUID{first, second} {
		if err := admitMessage(t.Context(), f.Pool, nil, client, id); err != nil {
			t.Fatal(err)
		}
	}
	var accepted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='received' FROM slack_requests WHERE id=$1`, second).Scan(&accepted); err != nil || !accepted || reads.Load() != 1 {
		t.Fatal("concurrent history budget was bypassed", reads.Load(), err)
	}
	var delay float64
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch FROM history_next_at-clock_timestamp()) FROM slack_installations WHERE id=$1`, f.installation).Scan(&delay); err != nil || delay > 2.1 {
		t.Fatal("internal app gesture unnecessarily delayed", delay, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET history_next_at=clock_timestamp() WHERE id=$1`, f.installation)
	if err := admitMessage(t.Context(), f.Pool, nil, client, second); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(status='accepted') FROM slack_requests WHERE id=ANY($1::uuid[])`, []uuid.UUID{first, second}).Scan(&accepted); err != nil || !accepted || reads.Load() != 2 {
		t.Fatal("different existing roots not admitted", reads.Load(), err)
	}
	limited.Store(true)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET history_next_at=clock_timestamp() WHERE id=$1`, f.installation)
	third := f.newMessage(t, "third", "456.789")
	if err := admitMessage(t.Context(), f.Pool, nil, client, third); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch FROM history_next_at-clock_timestamp()) FROM slack_installations WHERE id=$1`, f.installation).Scan(&delay); err != nil || delay < 115 {
		t.Fatal("Retry-After lost", delay, err)
	}
	if err := admitMessage(t.Context(), f.Pool, nil, client, third); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 3 {
		t.Fatal("retried before Retry-After", reads.Load())
	}
	limitedJSON.Store(true)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET history_next_at=clock_timestamp() WHERE id=$1`, f.installation)
	if err := admitMessage(t.Context(), f.Pool, nil, client, third); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch FROM history_next_at-clock_timestamp()) FROM slack_installations WHERE id=$1`, f.installation).Scan(&delay); err != nil || delay < 115 {
		t.Fatal("JSON Retry-After lost", delay, err)
	}

}

func TestPublicationAdmissionSharesGateButDeliverySerializesPacing(t *testing.T) {
	f := newStatusFixture(t)
	first, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(context.Background())
	second, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(context.Background())
	if ok, err := agent.LockSlackPublications(t.Context(), first, f.Environment, []uuid.UUID{f.publication}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if ok, err := agent.LockSlackPublications(ctx, second, f.Environment, []uuid.UUID{f.publication}); err != nil || !ok {
		t.Fatal("independent admission serialized", ok, err)
	}
	writer, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err = writer.Exec(t.Context(), `SET LOCAL lock_timeout='50ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = agent.LockSlackChannelsForDelivery(t.Context(), writer, f.Environment, []uuid.UUID{f.channel})
	var blocked *pgconn.PgError
	if !errors.As(err, &blocked) || blocked.Code != "55P03" {
		t.Fatal("delivery write bypassed admission gate", err)
	}
}
