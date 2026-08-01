package ecr

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
)

const Username = "AWS"

const (
	ResourceKindTagKey   = "helmr.dev/resource-kind"
	ResourceKindTagValue = "environment-image-cache"
	EnvironmentIDTagKey  = "helmr.dev/environment-id"
)

var repositoryPrefixPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._/-][a-z0-9]+)*$`)

type Config struct {
	RegistryAuthority   string
	RepositoryPrefix    string
	CacheRoleARN        string
	RepositoryARNPrefix string
}

type validatedConfig struct {
	Config
	partition string
	region    string
	accountID string
}

func validateConfig(config Config) (validatedConfig, error) {
	if strings.TrimSpace(config.RegistryAuthority) != config.RegistryAuthority ||
		strings.TrimSpace(config.RepositoryPrefix) != config.RepositoryPrefix ||
		strings.TrimSpace(config.CacheRoleARN) != config.CacheRoleARN ||
		strings.TrimSpace(config.RepositoryARNPrefix) != config.RepositoryARNPrefix {
		return validatedConfig{}, contract("configuration values must be non-empty and whitespace-free")
	}
	if config.RegistryAuthority == "" || config.RepositoryPrefix == "" ||
		config.CacheRoleARN == "" || config.RepositoryARNPrefix == "" {
		return validatedConfig{}, contract("configuration is incomplete")
	}
	authority, err := imagebuild.CanonicalRegistryAuthority(config.RegistryAuthority)
	if err != nil || authority != config.RegistryAuthority {
		return validatedConfig{}, contract("registry authority is not canonical")
	}
	if len(config.RepositoryPrefix) > 200 || !repositoryPrefixPattern.MatchString(config.RepositoryPrefix) ||
		strings.Contains(config.RepositoryPrefix, "//") {
		return validatedConfig{}, contract("repository prefix is invalid")
	}

	role, err := arn.Parse(config.CacheRoleARN)
	if err != nil || role.Service != "iam" || role.Region != "" || role.AccountID == "" ||
		!strings.HasPrefix(role.Resource, "role/") || len(strings.TrimPrefix(role.Resource, "role/")) == 0 {
		return validatedConfig{}, contract("cache role ARN is invalid")
	}
	repositoryPrefixARN := strings.TrimSuffix(config.RepositoryARNPrefix, "/")
	repository, err := arn.Parse(repositoryPrefixARN)
	if err != nil || repository.Service != "ecr" || repository.Region == "" || repository.AccountID == "" ||
		repository.Resource != "repository/"+config.RepositoryPrefix {
		return validatedConfig{}, contract("repository ARN prefix is invalid")
	}
	if config.RepositoryARNPrefix != repositoryPrefixARN+"/" {
		return validatedConfig{}, contract("repository ARN prefix must end with one slash")
	}
	if role.Partition != repository.Partition || role.AccountID != repository.AccountID {
		return validatedConfig{}, contract("cache role and repository namespace must share partition and account")
	}
	wantAuthority, err := registryAuthority(repository.Partition, repository.Region, repository.AccountID)
	if err != nil || wantAuthority != config.RegistryAuthority {
		return validatedConfig{}, contract("registry authority does not match repository ARN prefix")
	}
	return validatedConfig{
		Config: config, partition: repository.Partition, region: repository.Region,
		accountID: repository.AccountID,
	}, nil
}

func registryAuthority(partition, region, accountID string) (string, error) {
	suffix := "amazonaws.com"
	switch partition {
	case "aws", "aws-us-gov":
	case "aws-cn":
		suffix = "amazonaws.com.cn"
	default:
		return "", fmt.Errorf("unsupported AWS partition %q", partition)
	}
	return accountID + ".dkr.ecr." + region + "." + suffix, nil
}

func contract(message string) error {
	return &imagecache.ContractError{Message: message}
}
