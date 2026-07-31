package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func BuildAWSRequest(ctx context.Context, workerGroupID string, nonce string, supportsRun bool, supportsBuild bool) (json.RawMessage, error) {
	workerGroupID = strings.TrimSpace(workerGroupID)
	nonce = strings.TrimSpace(nonce)
	if workerGroupID == "" || nonce == "" || (!supportsRun && !supportsBuild) {
		return nil, fmt.Errorf("worker group, enrollment nonce, and at least one supported role are required")
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS worker identity configuration: %w", err)
	}
	metadata := imds.NewFromConfig(awsConfig)
	document, err := metadata.GetInstanceIdentityDocument(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read EC2 instance identity document: %w", err)
	}
	if document.Region == "" || document.InstanceID == "" {
		return nil, fmt.Errorf("EC2 instance identity document is incomplete")
	}
	awsConfig.Region = document.Region
	credentials, err := awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve EC2 instance credentials: %w", err)
	}
	body := "Action=GetCallerIdentity&Version=2011-06-15"
	endpoint := "https://sts." + document.Region + ".amazonaws.com/"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	httpRequest.Header.Set(enrollmentNonceHeader, nonce)
	payloadHash := sha256.Sum256([]byte(body))
	if err := v4.NewSigner().SignHTTP(ctx, credentials, httpRequest, hex.EncodeToString(payloadHash[:]), "sts", document.Region, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("sign AWS worker identity request: %w", err)
	}
	documentJSON, err := json.Marshal(document.InstanceIdentityDocument)
	if err != nil {
		return nil, fmt.Errorf("encode EC2 instance identity document: %w", err)
	}
	requestJSON, err := json.Marshal(awsWorkerEnrollmentRequest{
		WorkerEnrollmentIntent: api.WorkerEnrollmentIntent{
			WorkerGroupID: workerGroupID, Nonce: nonce, SupportsRun: supportsRun, SupportsBuild: supportsBuild,
			ProtocolVersion: auth.WorkerProtocolVersion,
		},
		InstanceIdentityDocument: documentJSON,
		SignedSTSRequest: signedHTTPRequest{
			Method: http.MethodPost, URL: endpoint, Headers: httpRequest.Header.Clone(), Body: body,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode AWS worker enrollment request: %w", err)
	}
	return requestJSON, nil
}
