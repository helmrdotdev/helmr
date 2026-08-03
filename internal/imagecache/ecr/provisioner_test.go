package ecr

import (
	"context"
	"errors"
	"strings"
	"testing"

	awsecr "github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/imagecache"
)

func TestProvisionerTargetIsOpaqueAndEnsureReconcilesExactContract(t *testing.T) {
	client := &repositoryClient{notFound: true}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	environmentID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	target, err := provisioner.Target(environmentID, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if target.Authority != testConfig().RegistryAuthority || target.Username != Username ||
		!strings.HasPrefix(target.Ref, testConfig().RegistryAuthority+"/helmr-cache/environments/"+environmentID.String()+":cache-") ||
		strings.Contains(target.Ref, "arn:aws") {
		t.Fatalf("target = %+v", target)
	}
	replay, err := provisioner.Target(environmentID, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil || replay != target {
		t.Fatalf("replay target = %+v, %v", replay, err)
	}
	changed, err := provisioner.Target(environmentID, "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil || changed.Ref == target.Ref {
		t.Fatalf("changed target = %+v, %v", changed, err)
	}
	if err := provisioner.Ensure(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if client.create == nil || client.create.EncryptionConfiguration == nil ||
		client.create.EncryptionConfiguration.EncryptionType != types.EncryptionTypeAes256 ||
		client.create.ImageTagMutability != types.ImageTagMutabilityMutable || len(client.create.Tags) != 2 {
		t.Fatalf("create = %+v", client.create)
	}
	if client.policy == nil || !strings.Contains(*client.policy.PolicyText, testConfig().CacheRoleARN) ||
		client.lifecycle == nil || !strings.Contains(*client.lifecycle.LifecyclePolicyText, `"countNumber":3`) ||
		!strings.Contains(*client.lifecycle.LifecyclePolicyText, `"countNumber":30`) {
		t.Fatalf("policy = %+v lifecycle = %+v", client.policy, client.lifecycle)
	}
}

func TestConfigStrictlyBindsRegistryRoleAndRepositoryNamespace(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"authority account": func(config *Config) { config.RegistryAuthority = "999999999999.dkr.ecr.us-east-1.amazonaws.com" },
		"role account":      func(config *Config) { config.CacheRoleARN = "arn:aws:iam::999999999999:role/helmr-cache" },
		"repository prefix": func(config *Config) {
			config.RepositoryARNPrefix = "arn:aws:ecr:us-east-1:123456789012:repository/other/"
		},
		"missing slash": func(config *Config) { config.RepositoryARNPrefix = strings.TrimSuffix(config.RepositoryARNPrefix, "/") },
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			mutate(&config)
			if _, err := NewProvisioner(config, &repositoryClient{}); err == nil {
				t.Fatal("mismatched ECR configuration accepted")
			}
		})
	}
}

func TestProvisionerConvergesAfterConcurrentCreate(t *testing.T) {
	client := &repositoryClient{notFound: true, concurrentCreate: true}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	target, err := provisioner.Target(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Ensure(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if client.describeCalls != 2 {
		t.Fatalf("DescribeRepositories calls = %d", client.describeCalls)
	}
}

func TestProvisionerEncryptionMismatchIsHardAndNeverDeletes(t *testing.T) {
	client := &repositoryClient{encryption: types.EncryptionTypeKms}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	target, err := provisioner.Target(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	err = provisioner.Ensure(t.Context(), target)
	var mismatch *imagecache.ContractError
	if !errors.As(err, &mismatch) || imagecache.IsUnavailable(err) {
		t.Fatalf("error = %T %v", err, err)
	}
	if client.policy != nil || client.lifecycle != nil {
		t.Fatal("immutable mismatch was reconciled")
	}
}

func TestProvisionerRetiresOnlyExactEnvironmentRepository(t *testing.T) {
	environmentID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	client := &repositoryClient{tags: ownerTags(environmentID)}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Retire(t.Context(), environmentID); err != nil {
		t.Fatal(err)
	}
	wantName := testConfig().RepositoryPrefix + "/environments/" + environmentID.String()
	if client.deleted == nil || client.deleted.RepositoryName == nil ||
		*client.deleted.RepositoryName != wantName || !client.deleted.Force {
		t.Fatalf("delete = %+v", client.deleted)
	}
}

func TestProvisionerRetirementProvesAbsence(t *testing.T) {
	client := &repositoryClient{alwaysNotFound: true}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Retire(t.Context(), uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")); err != nil {
		t.Fatal(err)
	}
	if client.deleted != nil {
		t.Fatal("absent repository was deleted")
	}
}

func TestProvisionerRetirementRejectsOwnerTagMismatch(t *testing.T) {
	environmentID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	wrongID := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
	client := &repositoryClient{tags: ownerTags(wrongID)}
	provisioner, err := NewProvisioner(testConfig(), client)
	if err != nil {
		t.Fatal(err)
	}
	err = provisioner.Retire(t.Context(), environmentID)
	var mismatch *imagecache.ContractError
	if !errors.As(err, &mismatch) || client.deleted != nil {
		t.Fatalf("Retire error = %T %v, delete = %+v", err, err, client.deleted)
	}
}

func testConfig() Config {
	return Config{
		RegistryAuthority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		RepositoryPrefix:  "helmr-cache", CacheRoleARN: "arn:aws:iam::123456789012:role/helmr-cache",
		RepositoryARNPrefix: "arn:aws:ecr:us-east-1:123456789012:repository/helmr-cache/",
	}
}

type repositoryClient struct {
	notFound, alwaysNotFound, concurrentCreate bool
	describeCalls                              int
	encryption                                 types.EncryptionType
	tags                                       []types.Tag
	create                                     *awsecr.CreateRepositoryInput
	policy                                     *awsecr.SetRepositoryPolicyInput
	lifecycle                                  *awsecr.PutLifecyclePolicyInput
	deleted                                    *awsecr.DeleteRepositoryInput
}

func (client *repositoryClient) repository(name string) types.Repository {
	arn := testConfig().RepositoryARNPrefix + strings.TrimPrefix(name, testConfig().RepositoryPrefix+"/")
	encryption := client.encryption
	if encryption == "" {
		encryption = types.EncryptionTypeAes256
	}
	return types.Repository{RepositoryName: &name, RepositoryArn: &arn,
		EncryptionConfiguration: &types.EncryptionConfiguration{EncryptionType: encryption},
		ImageTagMutability:      types.ImageTagMutabilityMutable}
}

func (client *repositoryClient) DescribeRepositories(_ context.Context, input *awsecr.DescribeRepositoriesInput, _ ...func(*awsecr.Options)) (*awsecr.DescribeRepositoriesOutput, error) {
	client.describeCalls++
	if client.alwaysNotFound || client.notFound && client.describeCalls == 1 {
		return nil, &types.RepositoryNotFoundException{}
	}
	repository := client.repository(input.RepositoryNames[0])
	return &awsecr.DescribeRepositoriesOutput{Repositories: []types.Repository{repository}}, nil
}

func (client *repositoryClient) CreateRepository(_ context.Context, input *awsecr.CreateRepositoryInput, _ ...func(*awsecr.Options)) (*awsecr.CreateRepositoryOutput, error) {
	client.create = input
	if client.concurrentCreate {
		return nil, &types.RepositoryAlreadyExistsException{}
	}
	repository := client.repository(*input.RepositoryName)
	return &awsecr.CreateRepositoryOutput{Repository: &repository}, nil
}

func (client *repositoryClient) ListTagsForResource(context.Context, *awsecr.ListTagsForResourceInput, ...func(*awsecr.Options)) (*awsecr.ListTagsForResourceOutput, error) {
	return &awsecr.ListTagsForResourceOutput{Tags: client.tags}, nil
}
func (client *repositoryClient) TagResource(context.Context, *awsecr.TagResourceInput, ...func(*awsecr.Options)) (*awsecr.TagResourceOutput, error) {
	return &awsecr.TagResourceOutput{}, nil
}
func (client *repositoryClient) UntagResource(context.Context, *awsecr.UntagResourceInput, ...func(*awsecr.Options)) (*awsecr.UntagResourceOutput, error) {
	return &awsecr.UntagResourceOutput{}, nil
}
func (client *repositoryClient) PutImageTagMutability(context.Context, *awsecr.PutImageTagMutabilityInput, ...func(*awsecr.Options)) (*awsecr.PutImageTagMutabilityOutput, error) {
	return &awsecr.PutImageTagMutabilityOutput{}, nil
}
func (client *repositoryClient) SetRepositoryPolicy(_ context.Context, input *awsecr.SetRepositoryPolicyInput, _ ...func(*awsecr.Options)) (*awsecr.SetRepositoryPolicyOutput, error) {
	client.policy = input
	return &awsecr.SetRepositoryPolicyOutput{}, nil
}
func (client *repositoryClient) PutLifecyclePolicy(_ context.Context, input *awsecr.PutLifecyclePolicyInput, _ ...func(*awsecr.Options)) (*awsecr.PutLifecyclePolicyOutput, error) {
	client.lifecycle = input
	return &awsecr.PutLifecyclePolicyOutput{}, nil
}
func (client *repositoryClient) DeleteRepository(_ context.Context, input *awsecr.DeleteRepositoryInput, _ ...func(*awsecr.Options)) (*awsecr.DeleteRepositoryOutput, error) {
	client.deleted = input
	return &awsecr.DeleteRepositoryOutput{}, nil
}
