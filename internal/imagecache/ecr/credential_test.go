package ecr

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	awsecr "github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/imagecache"
)

func TestCredentialProviderUsesMandatoryExactRepositorySessionPolicy(t *testing.T) {
	provisioner, err := NewProvisioner(testConfig(), &repositoryClient{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := provisioner.Target(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	stsClient := &stsClient{}
	tokenClient := &tokenClient{endpoint: "https://" + target.Authority,
		token: base64.StdEncoding.EncodeToString([]byte("AWS:attempt-password"))}
	provider, err := NewCredentialProvider(testConfig(), stsClient, testTokenFactory{new: func(value awsdk.Credentials) (TokenAPI, error) {
		if value.AccessKeyID != "access" || value.SecretAccessKey != "secret" || value.SessionToken != "session" {
			t.Fatalf("credentials = %+v", value)
		}
		return tokenClient, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider.now = func() time.Time { return time.Unix(100, 0) }
	credential, err := provider.Fetch(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer credential.Clear()
	repositoryARN := testConfig().RepositoryARNPrefix + "environments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	if stsClient.input == nil || stsClient.input.Policy == nil ||
		!strings.Contains(*stsClient.input.Policy, `"ecr:GetAuthorizationToken"`) ||
		!strings.Contains(*stsClient.input.Policy, `"NotResource":"`+repositoryARN+`"`) ||
		!strings.Contains(*stsClient.input.Policy, `"Resource":"`+repositoryARN+`"`) {
		t.Fatalf("AssumeRole = %+v", stsClient.input)
	}
	if credential.Authority != target.Authority || credential.Username != Username || string(credential.Password) != "attempt-password" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestCredentialProviderRejectsMismatchedProxyAsHardContractError(t *testing.T) {
	provisioner, _ := NewProvisioner(testConfig(), &repositoryClient{})
	target, _ := provisioner.Target(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	provider, err := NewCredentialProvider(testConfig(), &stsClient{}, testTokenFactory{new: func(awsdk.Credentials) (TokenAPI, error) {
		return &tokenClient{endpoint: "https://other.example", token: base64.StdEncoding.EncodeToString([]byte("AWS:password"))}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider.now = func() time.Time { return time.Unix(100, 0) }
	_, err = provider.Fetch(t.Context(), target)
	var mismatch *imagecache.ContractError
	if !errors.As(err, &mismatch) || imagecache.IsUnavailable(err) {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestCredentialProviderClassifiesAWSFailureAsUnavailable(t *testing.T) {
	provisioner, _ := NewProvisioner(testConfig(), &repositoryClient{})
	target, _ := provisioner.Target(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	provider, err := NewCredentialProvider(testConfig(), &stsClient{err: errors.New("throttled")}, testTokenFactory{new: func(awsdk.Credentials) (TokenAPI, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Fetch(t.Context(), target)
	if !imagecache.IsUnavailable(err) {
		t.Fatalf("error = %T %v", err, err)
	}
}

type stsClient struct {
	input *sts.AssumeRoleInput
	err   error
}

func (client *stsClient) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	client.input = input
	if client.err != nil {
		return nil, client.err
	}
	access, secret, session := "access", "secret", "session"
	expires := time.Unix(1000, 0)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: &access, SecretAccessKey: &secret, SessionToken: &session, Expiration: &expires,
	}}, nil
}

type tokenClient struct{ endpoint, token string }

func (client *tokenClient) GetAuthorizationToken(context.Context, *awsecr.GetAuthorizationTokenInput, ...func(*awsecr.Options)) (*awsecr.GetAuthorizationTokenOutput, error) {
	return &awsecr.GetAuthorizationTokenOutput{AuthorizationData: []types.AuthorizationData{{
		ProxyEndpoint: &client.endpoint, AuthorizationToken: &client.token,
	}}}, nil
}

type testTokenFactory struct {
	region string
	new    func(awsdk.Credentials) (TokenAPI, error)
}

func (factory testTokenFactory) Region() string {
	if factory.region != "" {
		return factory.region
	}
	return "us-east-1"
}

func (factory testTokenFactory) New(credentials awsdk.Credentials) (TokenAPI, error) {
	return factory.new(credentials)
}
