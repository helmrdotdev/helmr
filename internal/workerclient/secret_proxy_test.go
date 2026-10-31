package workerclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
)

func TestSecretProxyErrorRecognizesRevokedSecret(t *testing.T) {
	err := secretProxyError(fmt.Errorf("resolve: %w", &httpclient.Error{StatusCode: 409, Code: secretproxy.SecretRevokedCode, Message: "protected Secret is revoked; rotate it to resume"}))
	if !errors.Is(err, secretproxy.ErrSecretRevoked) {
		t.Fatalf("revoked resolution = %v", err)
	}
	other := &httpclient.Error{StatusCode: 409, Code: "conflict", Message: "secret delivery authority is unavailable"}
	if err := secretProxyError(other); errors.Is(err, secretproxy.ErrSecretRevoked) {
		t.Fatalf("unrelated conflict mapped to revocation: %v", err)
	}
}
