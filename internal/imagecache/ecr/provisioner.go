package ecr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	awsecr "github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const cacheRefDomain = "helmr.image-cache-ref.v0\x00"

var repositoryDataActions = []string{
	"ecr:BatchCheckLayerAvailability",
	"ecr:BatchGetImage",
	"ecr:CompleteLayerUpload",
	"ecr:GetDownloadUrlForLayer",
	"ecr:InitiateLayerUpload",
	"ecr:PutImage",
	"ecr:UploadLayerPart",
}

type RepositoryAPI interface {
	DescribeRepositories(context.Context, *awsecr.DescribeRepositoriesInput, ...func(*awsecr.Options)) (*awsecr.DescribeRepositoriesOutput, error)
	CreateRepository(context.Context, *awsecr.CreateRepositoryInput, ...func(*awsecr.Options)) (*awsecr.CreateRepositoryOutput, error)
	ListTagsForResource(context.Context, *awsecr.ListTagsForResourceInput, ...func(*awsecr.Options)) (*awsecr.ListTagsForResourceOutput, error)
	TagResource(context.Context, *awsecr.TagResourceInput, ...func(*awsecr.Options)) (*awsecr.TagResourceOutput, error)
	UntagResource(context.Context, *awsecr.UntagResourceInput, ...func(*awsecr.Options)) (*awsecr.UntagResourceOutput, error)
	PutImageTagMutability(context.Context, *awsecr.PutImageTagMutabilityInput, ...func(*awsecr.Options)) (*awsecr.PutImageTagMutabilityOutput, error)
	SetRepositoryPolicy(context.Context, *awsecr.SetRepositoryPolicyInput, ...func(*awsecr.Options)) (*awsecr.SetRepositoryPolicyOutput, error)
	PutLifecyclePolicy(context.Context, *awsecr.PutLifecyclePolicyInput, ...func(*awsecr.Options)) (*awsecr.PutLifecyclePolicyOutput, error)
	DeleteRepository(context.Context, *awsecr.DeleteRepositoryInput, ...func(*awsecr.Options)) (*awsecr.DeleteRepositoryOutput, error)
}

type Provisioner struct {
	config validatedConfig
	client RepositoryAPI
}

func NewProvisioner(config Config, client RepositoryAPI) (*Provisioner, error) {
	validated, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, contract("ECR repository client is required")
	}
	return &Provisioner{config: validated, client: client}, nil
}

func (provisioner *Provisioner) Target(environmentID uuid.UUID, cacheScope string) (imagecache.Target, error) {
	if ids.Validate(environmentID.String()) != nil {
		return imagecache.Target{}, contract("environment ID must be canonical")
	}
	if !sha256sum.ValidDigest(cacheScope) {
		return imagecache.Target{}, contract("cache scope must be a canonical sha256 digest")
	}
	repository := provisioner.repositoryName(environmentID)
	digest := sha256.Sum256([]byte(cacheRefDomain + cacheScope))
	return imagecache.Target{
		Authority: provisioner.config.RegistryAuthority,
		Username:  Username,
		Ref:       provisioner.config.RegistryAuthority + "/" + repository + ":cache-" + hex.EncodeToString(digest[:]),
	}, nil
}

func (provisioner *Provisioner) Ensure(ctx context.Context, target imagecache.Target) error {
	repositoryName, environmentID, err := provisioner.validateTarget(target)
	if err != nil {
		return err
	}
	repository, err := provisioner.describe(ctx, repositoryName)
	if err != nil {
		var notFound *types.RepositoryNotFoundException
		if !errors.As(err, &notFound) {
			return unavailable("describe repository", err)
		}
		repository, err = provisioner.create(ctx, repositoryName, environmentID)
		if err != nil {
			var exists *types.RepositoryAlreadyExistsException
			if !errors.As(err, &exists) {
				return unavailable("create repository", err)
			}
			repository, err = provisioner.describe(ctx, repositoryName)
			if err != nil {
				return unavailable("describe concurrently-created repository", err)
			}
		}
	}
	if repository == nil || repository.RepositoryName == nil || *repository.RepositoryName != repositoryName ||
		repository.RepositoryArn == nil || *repository.RepositoryArn != provisioner.repositoryARN(repositoryName) {
		return contract("ECR returned a repository outside the configured namespace")
	}
	if repository.EncryptionConfiguration == nil ||
		repository.EncryptionConfiguration.EncryptionType != types.EncryptionTypeAes256 {
		return contract("existing repository encryption must be AES256; it will not be replaced")
	}
	if repository.ImageTagMutability != types.ImageTagMutabilityMutable {
		if _, err := provisioner.client.PutImageTagMutability(ctx, &awsecr.PutImageTagMutabilityInput{
			RepositoryName: &repositoryName, ImageTagMutability: types.ImageTagMutabilityMutable,
		}); err != nil {
			return unavailable("reconcile repository tag mutability", err)
		}
	}
	if err := provisioner.reconcileTags(ctx, *repository.RepositoryArn, environmentID); err != nil {
		return err
	}
	policy, err := repositoryPolicy(*repository.RepositoryArn, provisioner.config.CacheRoleARN)
	if err != nil {
		return contract("encode repository policy: " + err.Error())
	}
	if _, err := provisioner.client.SetRepositoryPolicy(ctx, &awsecr.SetRepositoryPolicyInput{
		RepositoryName: &repositoryName, PolicyText: &policy, Force: true,
	}); err != nil {
		return unavailable("reconcile repository policy", err)
	}
	lifecycle, err := lifecyclePolicy()
	if err != nil {
		return contract("encode lifecycle policy: " + err.Error())
	}
	if _, err := provisioner.client.PutLifecyclePolicy(ctx, &awsecr.PutLifecyclePolicyInput{
		RepositoryName: &repositoryName, LifecyclePolicyText: &lifecycle,
	}); err != nil {
		return unavailable("reconcile lifecycle policy", err)
	}
	return nil
}

func (provisioner *Provisioner) Retire(ctx context.Context, environmentID uuid.UUID) error {
	if ids.Validate(environmentID.String()) != nil {
		return contract("environment ID must be canonical")
	}
	repositoryName := provisioner.repositoryName(environmentID)
	repository, err := provisioner.describe(ctx, repositoryName)
	if err != nil {
		var notFound *types.RepositoryNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return unavailable("describe repository for retirement", err)
	}
	if repository == nil || repository.RepositoryName == nil || *repository.RepositoryName != repositoryName ||
		repository.RepositoryArn == nil || *repository.RepositoryArn != provisioner.repositoryARN(repositoryName) {
		return contract("ECR returned a retirement repository outside the configured namespace")
	}
	output, err := provisioner.client.ListTagsForResource(ctx, &awsecr.ListTagsForResourceInput{
		ResourceArn: repository.RepositoryArn,
	})
	if err != nil {
		return unavailable("list repository tags for retirement", err)
	}
	tags := make(map[string]string, len(output.Tags))
	for _, tag := range output.Tags {
		if tag.Key == nil || tag.Value == nil {
			return contract("retirement repository contains an invalid tag")
		}
		tags[*tag.Key] = *tag.Value
	}
	if tags[ResourceKindTagKey] != ResourceKindTagValue ||
		tags[EnvironmentIDTagKey] != environmentID.String() {
		return contract("retirement repository owner tags do not match the environment")
	}
	if _, err := provisioner.client.DeleteRepository(ctx, &awsecr.DeleteRepositoryInput{
		RepositoryName: &repositoryName,
		Force:          true,
	}); err != nil {
		var notFound *types.RepositoryNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return unavailable("delete retired repository", err)
	}
	return nil
}

func (provisioner *Provisioner) describe(ctx context.Context, repositoryName string) (*types.Repository, error) {
	output, err := provisioner.client.DescribeRepositories(ctx, &awsecr.DescribeRepositoriesInput{
		RepositoryNames: []string{repositoryName},
	})
	if err != nil {
		return nil, err
	}
	if output == nil || len(output.Repositories) != 1 {
		return nil, errors.New("ECR did not return exactly one repository")
	}
	return &output.Repositories[0], nil
}

func (provisioner *Provisioner) create(ctx context.Context, repositoryName string, environmentID uuid.UUID) (*types.Repository, error) {
	output, err := provisioner.client.CreateRepository(ctx, &awsecr.CreateRepositoryInput{
		RepositoryName:          &repositoryName,
		EncryptionConfiguration: &types.EncryptionConfiguration{EncryptionType: types.EncryptionTypeAes256},
		ImageTagMutability:      types.ImageTagMutabilityMutable,
		Tags:                    ownerTags(environmentID),
	})
	if err != nil {
		return nil, err
	}
	if output == nil || output.Repository == nil {
		return nil, errors.New("ECR create returned no repository")
	}
	return output.Repository, nil
}

func (provisioner *Provisioner) reconcileTags(ctx context.Context, repositoryARN string, environmentID uuid.UUID) error {
	output, err := provisioner.client.ListTagsForResource(ctx, &awsecr.ListTagsForResourceInput{ResourceArn: &repositoryARN})
	if err != nil {
		return unavailable("list repository tags", err)
	}
	desired := ownerTags(environmentID)
	current := make(map[string]string, len(output.Tags))
	for _, tag := range output.Tags {
		if tag.Key == nil || tag.Value == nil {
			return contract("repository contains an invalid tag")
		}
		current[*tag.Key] = *tag.Value
	}
	remove := make([]string, 0)
	for key := range current {
		if key != ResourceKindTagKey && key != EnvironmentIDTagKey {
			remove = append(remove, key)
		}
	}
	slices.Sort(remove)
	if len(remove) > 0 {
		if _, err := provisioner.client.UntagResource(ctx, &awsecr.UntagResourceInput{
			ResourceArn: &repositoryARN, TagKeys: remove,
		}); err != nil {
			return unavailable("remove non-owner repository tags", err)
		}
	}
	if current[ResourceKindTagKey] != ResourceKindTagValue || current[EnvironmentIDTagKey] != environmentID.String() {
		if _, err := provisioner.client.TagResource(ctx, &awsecr.TagResourceInput{
			ResourceArn: &repositoryARN, Tags: desired,
		}); err != nil {
			return unavailable("reconcile repository owner tags", err)
		}
	}
	return nil
}

func (provisioner *Provisioner) validateTarget(target imagecache.Target) (string, uuid.UUID, error) {
	if target.Authority != provisioner.config.RegistryAuthority || target.Username != Username {
		return "", uuid.Nil, contract("target authority or username does not match configuration")
	}
	if err := imagebuild.ValidateCacheReference(target.Ref); err != nil {
		return "", uuid.Nil, contract("target ref is invalid")
	}
	prefix := target.Authority + "/"
	if !strings.HasPrefix(target.Ref, prefix) {
		return "", uuid.Nil, contract("target ref authority does not match configuration")
	}
	nameAndTag := strings.TrimPrefix(target.Ref, prefix)
	separator := strings.LastIndexByte(nameAndTag, ':')
	if separator <= 0 || !validCacheTag(nameAndTag[separator+1:]) {
		return "", uuid.Nil, contract("target cache tag is invalid")
	}
	repositoryName := nameAndTag[:separator]
	environmentText := strings.TrimPrefix(repositoryName, provisioner.config.RepositoryPrefix+"/environments/")
	if environmentText == repositoryName || strings.Contains(environmentText, "/") {
		return "", uuid.Nil, contract("target repository is outside the configured environment namespace")
	}
	environmentID, err := ids.Parse(environmentText)
	if err != nil ||
		provisioner.repositoryName(environmentID) != repositoryName {
		return "", uuid.Nil, contract("target environment repository is invalid")
	}
	return repositoryName, environmentID, nil
}

func (provisioner *Provisioner) repositoryName(environmentID uuid.UUID) string {
	return provisioner.config.RepositoryPrefix + "/environments/" + environmentID.String()
}

func (provisioner *Provisioner) repositoryARN(repositoryName string) string {
	return provisioner.config.RepositoryARNPrefix + strings.TrimPrefix(repositoryName, provisioner.config.RepositoryPrefix+"/")
}

func ownerTags(environmentID uuid.UUID) []types.Tag {
	kindKey, kindValue := ResourceKindTagKey, ResourceKindTagValue
	environmentKey, environmentValue := EnvironmentIDTagKey, environmentID.String()
	return []types.Tag{{Key: &environmentKey, Value: &environmentValue}, {Key: &kindKey, Value: &kindValue}}
}

func repositoryPolicy(repositoryARN, roleARN string) (string, error) {
	type statement struct {
		Sid         string                       `json:"Sid"`
		Effect      string                       `json:"Effect"`
		Principal   any                          `json:"Principal"`
		Action      []string                     `json:"Action"`
		Resource    string                       `json:"Resource,omitempty"`
		NotResource string                       `json:"NotResource,omitempty"`
		Condition   map[string]map[string]string `json:"Condition,omitempty"`
	}
	policy := struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{Version: "2012-10-17", Statement: []statement{
		{Sid: "AllowExecutionCacheRole", Effect: "Allow", Principal: map[string]string{"AWS": roleARN}, Action: repositoryDataActions, Resource: repositoryARN},
		{Sid: "DenyOtherPrincipals", Effect: "Deny", Principal: "*", Action: repositoryDataActions, Resource: repositoryARN,
			Condition: map[string]map[string]string{"StringNotEquals": {"aws:PrincipalArn": roleARN}}},
	}}
	raw, err := json.Marshal(policy)
	return string(raw), err
}

func lifecyclePolicy() (string, error) {
	type selection struct {
		TagStatus      string   `json:"tagStatus"`
		TagPatternList []string `json:"tagPatternList,omitempty"`
		CountType      string   `json:"countType"`
		CountUnit      string   `json:"countUnit"`
		CountNumber    int      `json:"countNumber"`
	}
	type rule struct {
		RulePriority int       `json:"rulePriority"`
		Description  string    `json:"description"`
		Selection    selection `json:"selection"`
		Action       struct {
			Type string `json:"type"`
		} `json:"action"`
	}
	policy := struct {
		Rules []rule `json:"rules"`
	}{}
	for _, input := range []struct {
		priority, days      int
		status, description string
		patterns            []string
	}{
		{1, 3, "untagged", "expire replaced cache layers after 3 days", nil},
		{2, 30, "tagged", "expire inactive cache refs after 30 days", []string{"*"}},
	} {
		item := rule{RulePriority: input.priority, Description: input.description,
			Selection: selection{TagStatus: input.status, TagPatternList: input.patterns, CountType: "sinceImagePushed", CountUnit: "days", CountNumber: input.days}}
		item.Action.Type = "expire"
		policy.Rules = append(policy.Rules, item)
	}
	raw, err := json.Marshal(policy)
	return string(raw), err
}

func validCacheTag(value string) bool {
	return len(value) == len("cache-")+sha256.Size*2 && strings.HasPrefix(value, "cache-") &&
		sha256sum.ValidDigest("sha256:"+strings.TrimPrefix(value, "cache-"))
}

func unavailable(operation string, err error) error {
	return &imagecache.UnavailableError{Operation: operation, Err: fmt.Errorf("ECR: %w", err)}
}
