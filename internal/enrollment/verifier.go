package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

const enrollmentNonceHeader = "X-Helmr-Enrollment-Nonce"

type AWSGroupBoundary struct {
	workergroup.Spec
	Capacity           workergroup.Capacity `json:"instance_capacity"`
	Region             string               `json:"region"`
	AccountID          string               `json:"account_id"`
	AutoScalingGroup   string               `json:"autoscaling_group"`
	InstanceProfileARN string               `json:"instance_profile_arn"`
	LaunchAMIID        string               `json:"launch_ami_id"`
	AMIIDs             []string             `json:"ami_ids"`
}

type AWSVerifiedIdentity struct {
	WorkerGroupID               string
	ResourceID                  string
	AllowsRun                   bool
	AllowsBuild                 bool
	ProtocolVersion             string
	EnrollmentPolicyFingerprint string
	AttestationFingerprint      string
}

type EC2Client interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

type AutoScalingClient interface {
	DescribeAutoScalingInstances(context.Context, *autoscaling.DescribeAutoScalingInstancesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingInstancesOutput, error)
}

type IAMClient interface {
	GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
}

type AWSVerifier struct {
	groups      map[string]AWSGroupBoundary
	ec2         EC2Client
	autoscaling AutoScalingClient
	iam         IAMClient
	http        *http.Client
}

func NewAWSVerifier(cfg aws.Config, groups []AWSGroupBoundary) (*AWSVerifier, error) {
	if len(groups) == 0 {
		return nil, errors.New("at least one worker group is required")
	}
	configured := make(map[string]AWSGroupBoundary, len(groups))
	for _, group := range groups {
		var err error
		group, err = NormalizeAWSGroupBoundary(group)
		if err != nil {
			return nil, err
		}
		if group.Region != cfg.Region {
			return nil, fmt.Errorf("worker group %q region %q differs from control AWS region %q", group.ID, group.Region, cfg.Region)
		}
		if _, exists := configured[group.ID]; exists {
			return nil, fmt.Errorf("worker group %q is duplicated", group.ID)
		}
		configured[group.ID] = group
	}
	return &AWSVerifier{
		groups: configured, ec2: ec2.NewFromConfig(cfg), autoscaling: autoscaling.NewFromConfig(cfg),
		iam: iam.NewFromConfig(cfg), http: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func NormalizeAWSGroupBoundary(group AWSGroupBoundary) (AWSGroupBoundary, error) {
	spec, err := workergroup.Normalize(group.Spec)
	if err != nil {
		return AWSGroupBoundary{}, err
	}
	group.Spec = spec
	group.Region = strings.TrimSpace(group.Region)
	group.AccountID = strings.TrimSpace(group.AccountID)
	group.AutoScalingGroup = strings.TrimSpace(group.AutoScalingGroup)
	group.InstanceProfileARN = strings.TrimSpace(group.InstanceProfileARN)
	group.LaunchAMIID = strings.TrimSpace(group.LaunchAMIID)
	amis := make([]string, 0, len(group.AMIIDs))
	seen := make(map[string]struct{}, len(group.AMIIDs))
	for _, raw := range group.AMIIDs {
		ami := strings.TrimSpace(raw)
		if ami == "" {
			continue
		}
		if _, ok := seen[ami]; ok {
			continue
		}
		seen[ami] = struct{}{}
		amis = append(amis, ami)
	}
	sort.Strings(amis)
	group.AMIIDs = amis
	if group.Region == "" || group.AccountID == "" || group.AutoScalingGroup == "" || group.InstanceProfileARN == "" || group.LaunchAMIID == "" || !slices.Contains(group.AMIIDs, group.LaunchAMIID) {
		return AWSGroupBoundary{}, fmt.Errorf("worker group %q is incomplete", group.ID)
	}
	return group, nil
}

func AWSPolicyFingerprint(group AWSGroupBoundary) (string, error) {
	group, err := NormalizeAWSGroupBoundary(group)
	if err != nil {
		return "", err
	}
	policy := struct {
		ID                 string   `json:"id"`
		Region             string   `json:"region"`
		AccountID          string   `json:"account_id"`
		AutoScalingGroup   string   `json:"autoscaling_group"`
		InstanceProfileARN string   `json:"instance_profile_arn"`
		LaunchAMIID        string   `json:"launch_ami_id"`
		AMIIDs             []string `json:"ami_ids"`
		AllowsRun          bool     `json:"allows_run"`
		AllowsBuild        bool     `json:"allows_build"`
	}{
		ID: group.ID, Region: group.Region, AccountID: group.AccountID,
		AutoScalingGroup: group.AutoScalingGroup, InstanceProfileARN: group.InstanceProfileARN, LaunchAMIID: group.LaunchAMIID,
		AMIIDs: group.AMIIDs, AllowsRun: group.AllowsRun, AllowsBuild: group.AllowsBuild,
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode worker enrollment policy: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

func AWSAttestationFingerprint(group AWSGroupBoundary, amiID string) (string, error) {
	group, err := NormalizeAWSGroupBoundary(group)
	if err != nil {
		return "", err
	}
	amiID = strings.TrimSpace(amiID)
	if !slices.Contains(group.AMIIDs, amiID) {
		return "", fmt.Errorf("AMI %q is outside worker group %q", amiID, group.ID)
	}
	attestation := struct {
		ID                 string `json:"id"`
		Region             string `json:"region"`
		AccountID          string `json:"account_id"`
		AutoScalingGroup   string `json:"autoscaling_group"`
		InstanceProfileARN string `json:"instance_profile_arn"`
		AMIID              string `json:"ami_id"`
		AllowsRun          bool   `json:"allows_run"`
		AllowsBuild        bool   `json:"allows_build"`
		ProtocolVersion    string `json:"protocol_version"`
	}{
		ID: group.ID, Region: group.Region, AccountID: group.AccountID,
		AutoScalingGroup: group.AutoScalingGroup, InstanceProfileARN: group.InstanceProfileARN,
		AMIID: amiID, AllowsRun: group.AllowsRun, AllowsBuild: group.AllowsBuild,
		ProtocolVersion: auth.WorkerProtocolVersion,
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return "", fmt.Errorf("encode worker attestation: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

func (v *AWSVerifier) VerifyWorkerEnrollment(ctx context.Context, request api.WorkerEnrollmentRequest) (AWSVerifiedIdentity, error) {
	group, ok := v.groups[strings.TrimSpace(request.WorkerGroupID)]
	if !ok {
		return AWSVerifiedIdentity{}, errors.New("worker group is not configured")
	}
	var document instanceIdentityDocument
	if err := json.Unmarshal(request.InstanceIdentityDocument, &document); err != nil {
		return AWSVerifiedIdentity{}, fmt.Errorf("decode instance identity document: %w", err)
	}
	if document.InstanceID == "" || document.AccountID != group.AccountID || document.Region != group.Region || !slices.Contains(group.AMIIDs, document.ImageID) {
		return AWSVerifiedIdentity{}, errors.New("instance identity document is outside the worker group boundary")
	}
	caller, err := v.verifySignedCallerIdentity(ctx, group, request)
	if err != nil {
		return AWSVerifiedIdentity{}, err
	}
	if caller.Account != group.AccountID {
		return AWSVerifiedIdentity{}, errors.New("signed caller account does not match worker group")
	}
	instance, err := v.describeInstance(ctx, document.InstanceID)
	if err != nil {
		return AWSVerifiedIdentity{}, err
	}
	if aws.ToString(instance.ImageId) != document.ImageID || instance.State == nil || (instance.State.Name != types.InstanceStateNamePending && instance.State.Name != types.InstanceStateNameRunning) {
		return AWSVerifiedIdentity{}, errors.New("EC2 instance is not an allowed live worker")
	}
	if instance.IamInstanceProfile == nil || aws.ToString(instance.IamInstanceProfile.Arn) != group.InstanceProfileARN {
		return AWSVerifiedIdentity{}, errors.New("EC2 instance profile does not match worker group")
	}
	if err := v.verifyAutoScalingGroup(ctx, document.InstanceID, group.AutoScalingGroup); err != nil {
		return AWSVerifiedIdentity{}, err
	}
	if err := v.verifyInstanceProfileCaller(ctx, group.InstanceProfileARN, document.InstanceID, caller.Arn); err != nil {
		return AWSVerifiedIdentity{}, err
	}
	attestationFingerprint, err := AWSAttestationFingerprint(group, document.ImageID)
	if err != nil {
		return AWSVerifiedIdentity{}, err
	}
	policyFingerprint, err := AWSPolicyFingerprint(group)
	if err != nil {
		return AWSVerifiedIdentity{}, err
	}
	return AWSVerifiedIdentity{
		WorkerGroupID: group.ID, ResourceID: document.InstanceID, AllowsRun: group.AllowsRun,
		AllowsBuild: group.AllowsBuild, ProtocolVersion: auth.WorkerProtocolVersion,
		EnrollmentPolicyFingerprint: policyFingerprint, AttestationFingerprint: attestationFingerprint,
	}, nil
}

func (v *AWSVerifier) VerifyWorkerLiveness(ctx context.Context, workerGroupID string, resourceID string, attestationFingerprint string) error {
	group, ok := v.groups[strings.TrimSpace(workerGroupID)]
	if !ok {
		return errors.New("worker group is not configured")
	}
	resourceID = strings.TrimSpace(resourceID)
	attestationFingerprint = strings.TrimSpace(attestationFingerprint)
	if resourceID == "" || attestationFingerprint == "" {
		return errors.New("worker liveness authority is incomplete")
	}
	instance, err := v.describeInstance(ctx, resourceID)
	if err != nil {
		return err
	}
	if instance.State == nil || (instance.State.Name != types.InstanceStateNamePending && instance.State.Name != types.InstanceStateNameRunning) {
		return errors.New("EC2 instance is not a live worker")
	}
	imageID := aws.ToString(instance.ImageId)
	expectedAttestation, err := AWSAttestationFingerprint(group, imageID)
	if err != nil || expectedAttestation != attestationFingerprint {
		return errors.New("EC2 worker attestation no longer matches its worker group")
	}
	if instance.IamInstanceProfile == nil || aws.ToString(instance.IamInstanceProfile.Arn) != group.InstanceProfileARN {
		return errors.New("EC2 instance profile does not match worker group")
	}
	if err := v.verifyAutoScalingLiveness(ctx, resourceID, group.AutoScalingGroup); err != nil {
		return err
	}
	return nil
}

type instanceIdentityDocument struct {
	AccountID  string `json:"accountId"`
	Region     string `json:"region"`
	InstanceID string `json:"instanceId"`
	ImageID    string `json:"imageId"`
}

type callerIdentity struct {
	Arn     string `xml:"GetCallerIdentityResult>Arn"`
	Account string `xml:"GetCallerIdentityResult>Account"`
}

func (v *AWSVerifier) verifySignedCallerIdentity(ctx context.Context, group AWSGroupBoundary, enrollment api.WorkerEnrollmentRequest) (callerIdentity, error) {
	signed := enrollment.SignedSTSRequest
	if signed.Method != http.MethodPost || signed.URL != "https://sts."+group.Region+".amazonaws.com/" || signed.Body != "Action=GetCallerIdentity&Version=2011-06-15" {
		return callerIdentity{}, errors.New("signed STS request target is invalid")
	}
	headers := make(http.Header, len(signed.Headers))
	for name, values := range signed.Headers {
		canonical := http.CanonicalHeaderKey(name)
		switch canonical {
		case "Authorization", "Content-Type", "X-Amz-Date", "X-Amz-Security-Token", enrollmentNonceHeader:
		default:
			return callerIdentity{}, fmt.Errorf("signed STS request header %q is not allowed", name)
		}
		for _, value := range values {
			headers.Add(canonical, value)
		}
	}
	if headers.Get(enrollmentNonceHeader) != enrollment.Nonce {
		return callerIdentity{}, errors.New("signed STS request does not bind the enrollment nonce")
	}
	authorization := headers.Get("Authorization")
	if authorization == "" || !signedHeaderContains(authorization, strings.ToLower(enrollmentNonceHeader)) {
		return callerIdentity{}, errors.New("enrollment nonce is not covered by the AWS signature")
	}
	if headers.Get("Content-Type") != "application/x-www-form-urlencoded; charset=utf-8" {
		return callerIdentity{}, errors.New("signed STS request content type is invalid")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, signed.URL, strings.NewReader(signed.Body))
	if err != nil {
		return callerIdentity{}, err
	}
	httpRequest.Header = headers
	response, err := v.http.Do(httpRequest)
	if err != nil {
		return callerIdentity{}, fmt.Errorf("verify signed STS request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if err != nil {
		return callerIdentity{}, fmt.Errorf("read STS verification response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return callerIdentity{}, fmt.Errorf("STS rejected signed identity request with status %d", response.StatusCode)
	}
	var caller callerIdentity
	if err := xml.Unmarshal(body, &caller); err != nil || caller.Arn == "" || caller.Account == "" {
		return callerIdentity{}, errors.New("STS identity response is invalid")
	}
	return caller, nil
}

func signedHeaderContains(authorization string, header string) bool {
	for field := range strings.SplitSeq(authorization, ",") {
		field = strings.TrimSpace(field)
		if !strings.HasPrefix(field, "SignedHeaders=") {
			continue
		}
		return slices.Contains(strings.Split(strings.TrimPrefix(field, "SignedHeaders="), ";"), header)
	}
	return false
}

func (v *AWSVerifier) describeInstance(ctx context.Context, instanceID string) (types.Instance, error) {
	output, err := v.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return types.Instance{}, fmt.Errorf("describe EC2 worker instance: %w", err)
	}
	if len(output.Reservations) != 1 || len(output.Reservations[0].Instances) != 1 || aws.ToString(output.Reservations[0].Instances[0].InstanceId) != instanceID {
		return types.Instance{}, errors.New("EC2 worker instance was not found")
	}
	return output.Reservations[0].Instances[0], nil
}

func (v *AWSVerifier) verifyAutoScalingGroup(ctx context.Context, instanceID string, expected string) error {
	return v.verifyAutoScalingLifecycle(ctx, instanceID, expected, []string{"Pending", "Pending:Wait", "Pending:Proceed", "InService"})
}

func (v *AWSVerifier) verifyAutoScalingLiveness(ctx context.Context, instanceID string, expected string) error {
	return v.verifyAutoScalingLifecycle(ctx, instanceID, expected, []string{"Pending", "Pending:Wait", "Pending:Proceed", "InService", "Terminating:Wait"})
}

func (v *AWSVerifier) verifyAutoScalingLifecycle(ctx context.Context, instanceID string, expected string, allowedLifecycleStates []string) error {
	output, err := v.autoscaling.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return fmt.Errorf("describe worker Auto Scaling membership: %w", err)
	}
	if len(output.AutoScalingInstances) != 1 || aws.ToString(output.AutoScalingInstances[0].InstanceId) != instanceID || aws.ToString(output.AutoScalingInstances[0].AutoScalingGroupName) != expected {
		return errors.New("worker is not a member of the configured Auto Scaling group")
	}
	instance := output.AutoScalingInstances[0]
	if aws.ToString(instance.HealthStatus) != "HEALTHY" || !slices.Contains(allowedLifecycleStates, aws.ToString(instance.LifecycleState)) {
		return errors.New("worker Auto Scaling instance is not in an enrollable lifecycle state")
	}
	return nil
}

func (v *AWSVerifier) verifyInstanceProfileCaller(ctx context.Context, profileARN string, instanceID string, callerARN string) error {
	profileIdentity, err := arn.Parse(profileARN)
	if err != nil || profileIdentity.Service != "iam" || !strings.HasPrefix(profileIdentity.Resource, "instance-profile/") {
		return errors.New("worker instance profile ARN is invalid")
	}
	profileName := strings.TrimPrefix(profileIdentity.Resource, "instance-profile/")
	if profileName == "" || strings.Contains(profileName, "/") {
		return errors.New("worker instance profile ARN is invalid")
	}
	output, err := v.iam.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: &profileName})
	if err != nil {
		return fmt.Errorf("get worker instance profile: %w", err)
	}
	if output.InstanceProfile == nil || aws.ToString(output.InstanceProfile.Arn) != profileARN || len(output.InstanceProfile.Roles) != 1 {
		return errors.New("worker instance profile is invalid")
	}
	roleARN := aws.ToString(output.InstanceProfile.Roles[0].Arn)
	roleName := aws.ToString(output.InstanceProfile.Roles[0].RoleName)
	roleIdentity, roleErr := arn.Parse(roleARN)
	callerIdentity, callerErr := arn.Parse(callerARN)
	if roleErr != nil || callerErr != nil || roleIdentity.Service != "iam" || callerIdentity.Service != "sts" || roleIdentity.Partition != profileIdentity.Partition || callerIdentity.Partition != profileIdentity.Partition || roleIdentity.AccountID != profileIdentity.AccountID || callerIdentity.AccountID != profileIdentity.AccountID || !strings.HasPrefix(roleIdentity.Resource, "role/") {
		return errors.New("worker instance profile role is invalid")
	}
	if callerIdentity.Resource != "assumed-role/"+roleName+"/"+instanceID {
		return errors.New("signed caller is not the EC2 instance-profile session")
	}
	return nil
}
