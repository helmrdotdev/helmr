package computerhost

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestComputerSaveFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"storage access denied", &smithy.GenericAPIError{Code: "AccessDenied", Fault: smithy.FaultClient}, true},
		{"ordinary credential response", &httpclient.Error{StatusCode: http.StatusForbidden}, true},
		{"graph conflict", &httpclient.Error{StatusCode: http.StatusConflict}, true},
		{"local adoption IO", io.ErrUnexpectedEOF, true},
		{"capture ambiguity", io.EOF, true},
		{"lease fenced", &httpclient.Error{Code: workerapi.SaveAuthorityLost}, false},
		{"host authority lost", workerclient.HostAuthorityRejectedError{Err: errors.New("host fenced")}, false},
		{"source corrupt", &disk.DeviceFailure{Cause: blockformat.ErrIntegrity}, false},
		{"authentication corrupt", blockformat.ErrAuthentication, false},
		{"immutable digest corrupt", cas.ErrDigestMismatch, false},
		{"stored bytes mismatch", errors.Join(cas.ErrRemoteIntegrity, cas.ErrDigestMismatch), true},
		{"protocol mismatch", &saveProtocolError{cause: errors.New("wrong owner")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if retry := saveOperationRetryable(test.err); retry != test.retry {
				t.Fatalf("retry=%t want=%t", retry, test.retry)
			}
		})
	}
}
