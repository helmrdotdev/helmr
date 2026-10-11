package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rejectedPreparationKeys struct {
	agent.DataKeyWrapper
	failure          string
	returned, copies [][]byte
}

func (k *rejectedPreparationKeys) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	k.returned = append(k.returned, key)
	k.copies = append(k.copies, bytes.Clone(key))
	if k.failure == "wrap" {
		return computerkey.Envelope{}, errors.New("injected provider failure")
	}
	return k.DataKeyWrapper.Wrap(ctx, scope, id, key)
}
func (k *rejectedPreparationKeys) Unwrap(ctx context.Context, scope, id string, envelope computerkey.Envelope) ([]byte, error) {
	key, err := k.DataKeyWrapper.Unwrap(ctx, scope, id, envelope)
	if err != nil {
		return key, err
	}
	if k.failure == "length" {
		clear(key)
		key = bytes.Repeat([]byte{37}, 31)
	}
	k.returned = append(k.returned, key)
	k.copies = append(k.copies, bytes.Clone(key))
	if k.failure == "unwrap" {
		return key, errors.New("injected provider failure")
	}
	return key, nil
}
func TestPreparationKeyFailuresClearMaterialAndHideResponseCause(t *testing.T) {
	for _, failure := range []string{"wrap", "unwrap", "length"} {
		t.Run(failure, func(t *testing.T) {
			f, request := preparationTransportFixture(t)
			local, err := computerkey.NewLocal("error-test", bytes.Repeat([]byte{8}, 32))
			if err != nil {
				t.Fatal(err)
			}
			keys := &rejectedPreparationKeys{DataKeyWrapper: local, failure: failure}
			server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.ComputerKeys = keys }))
			defer server.Close()
			auth := seedHostSecret(t, f.Pool, f.Worker)
			raw, _ := json.Marshal(request)
			response := postAgentComputerJSON(t, server, auth, "/worker/v1/allocations/preparation/key", raw)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusServiceUnavailable || bytes.Contains(body, []byte("injected provider failure")) {
				t.Fatalf("response=%d %s", response.StatusCode, body)
			}
			for _, plain := range keys.returned {
				if !bytes.Equal(plain, make([]byte, len(plain))) {
					t.Fatal("provider result was not cleared")
				}
			}
			for _, plain := range keys.copies {
				for _, encoding := range plaintextEncodings(plain) {
					if bytes.Contains(body, []byte(encoding)) {
						t.Fatal("response leaked plaintext")
					}
				}
			}
		})
	}
}
func plaintextEncodings(key []byte) []string {
	if len(key) == 0 {
		return nil
	}
	return []string{
		string(key),
		hex.EncodeToString(key),
		base64.StdEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key),
		strings.Trim(strings.Join(strings.Fields(fmt.Sprint(key)), ","), "[]"),
	}
}
