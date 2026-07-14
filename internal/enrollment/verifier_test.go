package enrollment

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

const (
	testAccountID  = "123456789012"
	testInstanceID = "i-0123456789abcdef0"
	testProfileARN = "arn:aws:iam::123456789012:instance-profile/helmr-run"
)

type fakeEC2 struct{}

func (fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{
		InstanceId: aws.String(testInstanceID), ImageId: aws.String("ami-allowed"),
		State:              &types.InstanceState{Name: types.InstanceStateNameRunning},
		IamInstanceProfile: &types.IamInstanceProfile{Arn: aws.String(testProfileARN)},
	}}}}}, nil
}

type fakeAutoScaling struct {
	lifecycle string
	health    string
}

func (f fakeAutoScaling) DescribeAutoScalingInstances(context.Context, *autoscaling.DescribeAutoScalingInstancesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingInstancesOutput, error) {
	lifecycle := f.lifecycle
	if lifecycle == "" {
		lifecycle = "Pending:Wait"
	}
	health := f.health
	if health == "" {
		health = "HEALTHY"
	}
	return &autoscaling.DescribeAutoScalingInstancesOutput{AutoScalingInstances: []autoscalingtypes.AutoScalingInstanceDetails{{
		InstanceId: aws.String(testInstanceID), AutoScalingGroupName: aws.String("helmr-run"),
		HealthStatus: aws.String(health), LifecycleState: aws.String(lifecycle),
	}}}, nil
}

type fakeIAM struct{}

func (fakeIAM) GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		Arn: aws.String(testProfileARN), InstanceProfileName: aws.String("helmr-run"),
		Roles: []iamtypes.Role{{Arn: aws.String("arn:aws:iam::123456789012:role/helmr-run"), RoleName: aws.String("helmr-run")}},
	}}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestVerifierBindsNonceAndAWSResourceBoundaries(t *testing.T) {
	verifier := &AWSVerifier{
		groups: map[string]AWSGroupBoundary{"run-workers": {
			Spec: workergroup.Spec{ID: "run-workers", AllowsRun: true}, Region: "us-east-1", AccountID: testAccountID,
			AutoScalingGroup: "helmr-run", InstanceProfileARN: testProfileARN,
			LaunchAMIID: "ami-allowed", AMIIDs: []string{"ami-allowed"},
		}},
		ec2: fakeEC2{}, autoscaling: fakeAutoScaling{}, iam: fakeIAM{},
		http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get(enrollmentNonceHeader) != "fresh-nonce" {
				t.Fatalf("nonce header = %q", request.Header.Get(enrollmentNonceHeader))
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
				`<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>arn:aws:sts::123456789012:assumed-role/helmr-run/i-0123456789abcdef0</Arn><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`,
			)), Header: make(http.Header)}, nil
		})},
	}
	request := api.WorkerEnrollmentRequest{
		WorkerGroupID: "run-workers", Nonce: "fresh-nonce",
		InstanceIdentityDocument: []byte(`{"accountId":"123456789012","region":"us-east-1","instanceId":"i-0123456789abcdef0","imageId":"ami-allowed"}`),
		SignedSTSRequest: api.SignedHTTPRequest{
			Method: http.MethodPost, URL: "https://sts.us-east-1.amazonaws.com/",
			Body: "Action=GetCallerIdentity&Version=2011-06-15",
			Headers: map[string][]string{
				"Authorization": {"AWS4-HMAC-SHA256 Credential=test, SignedHeaders=content-type;host;x-amz-date;x-helmr-enrollment-nonce, Signature=test"},
				"Content-Type":  {"application/x-www-form-urlencoded; charset=utf-8"},
				"X-Amz-Date":    {"20260713T000000Z"}, enrollmentNonceHeader: {"fresh-nonce"},
			},
		},
	}
	verified, err := verifier.VerifyWorkerEnrollment(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if verified.WorkerGroupID != "run-workers" || verified.ResourceID != testInstanceID || !verified.AllowsRun || verified.AllowsBuild {
		t.Fatalf("verified = %+v", verified)
	}

	request.Nonce = "different-nonce"
	if _, err := verifier.VerifyWorkerEnrollment(context.Background(), request); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("nonce mismatch error = %v", err)
	}

	request.Nonce = "fresh-nonce"
	verifier.autoscaling = fakeAutoScaling{lifecycle: "Terminating:Wait"}
	if _, err := verifier.VerifyWorkerEnrollment(context.Background(), request); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("terminating lifecycle error = %v", err)
	}
	verifier.autoscaling = fakeAutoScaling{health: "UNHEALTHY"}
	if _, err := verifier.VerifyWorkerEnrollment(context.Background(), request); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("unhealthy lifecycle error = %v", err)
	}
}

func TestVerifierRevalidatesWorkerLivenessDuringCredentialAuthentication(t *testing.T) {
	group := AWSGroupBoundary{
		Spec: workergroup.Spec{ID: "run-workers", AllowsRun: true}, Region: "us-east-1", AccountID: testAccountID,
		AutoScalingGroup: "helmr-run", InstanceProfileARN: testProfileARN,
		LaunchAMIID: "ami-allowed", AMIIDs: []string{"ami-allowed"},
	}
	attestation, err := AWSAttestationFingerprint(group, "ami-allowed")
	if err != nil {
		t.Fatal(err)
	}
	verifier := &AWSVerifier{
		groups: map[string]AWSGroupBoundary{group.ID: group},
		ec2:    fakeEC2{}, autoscaling: fakeAutoScaling{lifecycle: "Terminating:Wait"}, iam: fakeIAM{},
	}
	if err := verifier.VerifyWorkerLiveness(context.Background(), group.ID, testInstanceID, attestation); err != nil {
		t.Fatalf("terminating worker liveness = %v", err)
	}
	if err := verifier.VerifyWorkerLiveness(context.Background(), group.ID, testInstanceID, "sha256:stale"); err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("stale attestation error = %v", err)
	}
	verifier.autoscaling = fakeAutoScaling{lifecycle: "Terminated"}
	if err := verifier.VerifyWorkerLiveness(context.Background(), group.ID, testInstanceID, attestation); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("terminated lifecycle error = %v", err)
	}
}

func TestPolicyFingerprintCanonicalizesAMISetAndIgnoresPresentation(t *testing.T) {
	base := AWSGroupBoundary{
		Spec:   workergroup.Spec{ID: "run-workers", Name: "Run workers", Description: "first", AllowsRun: true},
		Region: "us-east-1", AccountID: "123456789012", AutoScalingGroup: "run-asg",
		InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/run",
		LaunchAMIID:        "ami-b", AMIIDs: []string{"ami-b", "ami-a", "ami-b"},
	}
	first, err := AWSPolicyFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Name = "Renamed workers"
	base.Description = "second"
	base.AMIIDs = []string{" ami-a ", "ami-b"}
	second, err := AWSPolicyFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("presentation-only change altered policy fingerprint: %s != %s", first, second)
	}
	base.LaunchAMIID = "ami-a"
	launchChanged, err := AWSPolicyFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if launchChanged == first {
		t.Fatal("launch AMI change did not alter policy fingerprint")
	}
	base.LaunchAMIID = "ami-b"
	base.AutoScalingGroup = "replacement-asg"
	third, err := AWSPolicyFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("attestation boundary change did not alter policy fingerprint")
	}
}

func TestNormalizeWorkerGroupRequiresLaunchAMIInAllowedSet(t *testing.T) {
	group := AWSGroupBoundary{
		Spec: workergroup.Spec{ID: "run-workers", AllowsRun: true}, Region: "us-east-1", AccountID: testAccountID,
		AutoScalingGroup: "helmr-run", InstanceProfileARN: testProfileARN,
		LaunchAMIID: "ami-launch", AMIIDs: []string{"ami-other"},
	}
	if _, err := NormalizeAWSGroupBoundary(group); err == nil {
		t.Fatal("launch AMI outside allowed set was accepted")
	}
}
