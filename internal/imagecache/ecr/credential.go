package ecr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	awsdk "github.com/aws/aws-sdk-go-v2/aws"
	awsecr "github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/helmrdotdev/helmr/internal/imagecache"
)

type STSAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type TokenAPI interface {
	GetAuthorizationToken(context.Context, *awsecr.GetAuthorizationTokenInput, ...func(*awsecr.Options)) (*awsecr.GetAuthorizationTokenOutput, error)
}

type TokenClientFactory interface {
	Region() string
	New(awsdk.Credentials) (TokenAPI, error)
}

type CredentialProvider struct {
	config validatedConfig
	sts    STSAPI
	tokens TokenClientFactory
	now    func() time.Time
}

func NewCredentialProvider(config Config, stsClient STSAPI, tokens TokenClientFactory) (*CredentialProvider, error) {
	validated, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	if stsClient == nil || tokens == nil {
		return nil, contract("STS client and ECR token client factory are required")
	}
	if tokens.Region() != validated.region {
		return nil, contract("ECR token client Region does not match the configured registry")
	}
	return &CredentialProvider{config: validated, sts: stsClient, tokens: tokens, now: time.Now}, nil
}

// NewTokenClientFactory binds assumed credentials to the already loaded AWS
// configuration without exposing them outside the Worker host adapter.
func NewTokenClientFactory(config awsdk.Config) TokenClientFactory {
	return &awsTokenClientFactory{config: config}
}

type awsTokenClientFactory struct{ config awsdk.Config }

func (factory *awsTokenClientFactory) Region() string { return factory.config.Region }

func (factory *awsTokenClientFactory) New(value awsdk.Credentials) (TokenAPI, error) {
	if value.AccessKeyID == "" || value.SecretAccessKey == "" || value.SessionToken == "" {
		return nil, errors.New("assumed AWS credentials are incomplete")
	}
	bound := factory.config
	bound.Credentials = awsdk.CredentialsProviderFunc(func(context.Context) (awsdk.Credentials, error) {
		return value, nil
	})
	return awsecr.NewFromConfig(bound), nil
}

func (provider *CredentialProvider) Fetch(ctx context.Context, target imagecache.Target) (imagecache.Credential, error) {
	repositoryName, environmentID, err := (&Provisioner{config: provider.config}).validateTarget(target)
	if err != nil {
		return imagecache.Credential{}, err
	}
	repositoryARN := provider.config.RepositoryARNPrefix +
		strings.TrimPrefix(repositoryName, provider.config.RepositoryPrefix+"/")
	policy, err := sessionPolicy(repositoryARN)
	if err != nil {
		return imagecache.Credential{}, contract("encode cache session policy: " + err.Error())
	}
	roleARN := provider.config.CacheRoleARN
	sessionName := "helmr-image-cache-" + strings.ReplaceAll(environmentID.String(), "-", "")
	if len(sessionName) > 64 {
		sessionName = sessionName[:64]
	}
	duration := int32(900)
	output, err := provider.sts.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &roleARN, RoleSessionName: &sessionName, Policy: &policy, DurationSeconds: &duration,
	})
	if err != nil {
		return imagecache.Credential{}, unavailable("assume exact repository cache role", err)
	}
	if output == nil || output.Credentials == nil || output.Credentials.AccessKeyId == nil ||
		output.Credentials.SecretAccessKey == nil || output.Credentials.SessionToken == nil ||
		*output.Credentials.AccessKeyId == "" || *output.Credentials.SecretAccessKey == "" ||
		*output.Credentials.SessionToken == "" || output.Credentials.Expiration == nil ||
		!output.Credentials.Expiration.After(provider.now()) {
		return imagecache.Credential{}, unavailable("assume exact repository cache role", errors.New("STS returned incomplete or expired credentials"))
	}
	assumed := awsdk.Credentials{
		AccessKeyID: *output.Credentials.AccessKeyId, SecretAccessKey: *output.Credentials.SecretAccessKey,
		SessionToken: *output.Credentials.SessionToken, CanExpire: true, Expires: *output.Credentials.Expiration,
		Source: "helmr-image-cache",
	}
	client, err := provider.tokens.New(assumed)
	*output.Credentials.SecretAccessKey = ""
	*output.Credentials.SessionToken = ""
	assumed.SecretAccessKey = ""
	assumed.SessionToken = ""
	if err != nil {
		return imagecache.Credential{}, unavailable("bind exact repository ECR client", err)
	}
	tokenOutput, err := client.GetAuthorizationToken(ctx, &awsecr.GetAuthorizationTokenInput{})
	if err != nil {
		return imagecache.Credential{}, unavailable("get ECR authorization token", err)
	}
	if tokenOutput == nil || len(tokenOutput.AuthorizationData) != 1 {
		return imagecache.Credential{}, contract("ECR returned an ambiguous authorization token set")
	}
	data := tokenOutput.AuthorizationData[0]
	if data.AuthorizationToken == nil || data.ProxyEndpoint == nil {
		return imagecache.Credential{}, contract("ECR authorization response is incomplete")
	}
	proxyAuthority := strings.TrimPrefix(*data.ProxyEndpoint, "https://")
	if proxyAuthority == *data.ProxyEndpoint || strings.ContainsAny(proxyAuthority, "/?#") ||
		proxyAuthority != provider.config.RegistryAuthority {
		return imagecache.Credential{}, contract("ECR token proxy authority does not exact-match assignment")
	}
	decoded, err := base64.StdEncoding.DecodeString(*data.AuthorizationToken)
	*data.AuthorizationToken = ""
	if err != nil {
		return imagecache.Credential{}, contract("ECR authorization token is not base64")
	}
	defer clearBytes(decoded)
	separator := bytes.IndexByte(decoded, ':')
	if separator < 0 || !bytes.Equal(decoded[:separator], []byte(Username)) || separator == len(decoded)-1 {
		return imagecache.Credential{}, contract("ECR authorization token has an invalid Basic-auth payload")
	}
	return imagecache.Credential{
		Authority: target.Authority, Username: Username, Password: bytes.Clone(decoded[separator+1:]),
	}, nil
}

func sessionPolicy(repositoryARN string) (string, error) {
	type statement struct {
		Sid         string   `json:"Sid"`
		Effect      string   `json:"Effect"`
		Action      []string `json:"Action"`
		Resource    any      `json:"Resource,omitempty"`
		NotResource string   `json:"NotResource,omitempty"`
	}
	policy := struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{Version: "2012-10-17", Statement: []statement{
		{Sid: "AllowRegistryToken", Effect: "Allow", Action: []string{"ecr:GetAuthorizationToken"}, Resource: "*"},
		{Sid: "AllowExactEnvironmentRepository", Effect: "Allow", Action: repositoryDataActions, Resource: repositoryARN},
		{Sid: "DenyOtherRepositories", Effect: "Deny", Action: repositoryDataActions, NotResource: repositoryARN},
	}}
	raw, err := json.Marshal(policy)
	return string(raw), err
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ imagecache.CredentialProvider = (*CredentialProvider)(nil)
