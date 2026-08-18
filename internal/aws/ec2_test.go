package aws

import (
	"context"
	"testing"
	"text/template"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type fakeEC2 struct {
	runInput       *ec2.RunInstancesInput
	describeOutput *ec2.DescribeInstancesOutput
	describeErr    error
	terminateErr   error
	terminated     []string
}

func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.describeOutput != nil {
		return f.describeOutput, nil
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (f *fakeEC2) RunInstances(_ context.Context, input *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.runInput = input
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: awssdk.String("i-123")}}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, input *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.terminated = append(f.terminated, input.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, f.terminateErr
}

func TestDeleteTreatsWrappedNotFoundAsSuccess(t *testing.T) {
	notFound := &smithy.OperationError{
		ServiceID: "EC2", OperationName: "DescribeInstances",
		Err: &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "missing", Fault: smithy.FaultClient},
	}
	client := newClient(&fakeEC2{describeErr: notFound}, nil, Config{ControllerID: "primary"})
	if err := client.DeleteRunner(context.Background(), "i-missing", "org/repo:42"); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}

	tags := []types.Tag{
		{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")},
		{Key: awssdk.String(jobTagKey), Value: awssdk.String(jobTag("org/repo:42"))},
	}
	api := &fakeEC2{
		describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
			Instances: []types.Instance{{InstanceId: awssdk.String("i-raced"), Tags: tags}},
		}}},
		terminateErr: notFound,
	}
	client = newClient(api, nil, Config{ControllerID: "primary"})
	if err := client.DeleteRunner(context.Background(), "i-raced", "org/repo:42"); err != nil {
		t.Fatalf("DeleteRunner() terminate race error = %v", err)
	}
}

func TestSpotRequestIsOneTimeTerminateAndRequiresIMDSv2(t *testing.T) {
	api := &fakeEC2{}
	tmpl := template.Must(template.New("cloud-init").Parse("#cloud-config\n{{.RunnerJITConfig}}"))
	client := newClient(api, tmpl, Config{
		AMI: "ami-pinned", InstanceType: "m7i.xlarge", SubnetID: "subnet-private",
		SecurityGroupIDs: []string{"sg-egress-only"}, ControllerID: "primary", Spot: true,
	})
	instance, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:42", RunnerJITConfig: "jit"})
	if err != nil || instance.ID != "i-123" {
		t.Fatalf("CreateRunner() = %#v, %v", instance, err)
	}
	input := api.runInput
	if input.InstanceMarketOptions == nil || input.InstanceMarketOptions.MarketType != types.MarketTypeSpot ||
		input.InstanceMarketOptions.SpotOptions == nil ||
		input.InstanceMarketOptions.SpotOptions.SpotInstanceType != types.SpotInstanceTypeOneTime ||
		input.InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior != types.InstanceInterruptionBehaviorTerminate {
		t.Fatalf("unexpected spot options: %#v", input.InstanceMarketOptions)
	}
	if input.MetadataOptions == nil || input.MetadataOptions.HttpTokens != types.HttpTokensStateRequired ||
		input.MetadataOptions.InstanceMetadataTags != types.InstanceMetadataTagsStateDisabled {
		t.Fatalf("unsafe instance metadata options: %#v", input.MetadataOptions)
	}
	if input.ClientToken == nil || *input.ClientToken == "" {
		t.Fatal("missing idempotency token")
	}
	if len(input.NetworkInterfaces) != 1 || input.NetworkInterfaces[0].AssociatePublicIpAddress == nil ||
		*input.NetworkInterfaces[0].AssociatePublicIpAddress {
		t.Fatal("private-by-default runner unexpectedly requested a public IP")
	}
}

func TestJobTagIsStableAndScoped(t *testing.T) {
	first := jobTag("org/repo:1")
	if first != jobTag("org/repo:1") {
		t.Fatal("job tag is not deterministic")
	}
	if first == jobTag("org/repo:2") {
		t.Fatal("different jobs received the same tag")
	}
}

func TestClientTokenIsStableForRetriesAndChangesForReplacementEpoch(t *testing.T) {
	first := clientToken("org/repo:1", 1)
	if first == clientToken("org/repo:1", 2) {
		t.Fatal("replacement provisioning epoch reused the EC2 client token")
	}
	if first != clientToken("org/repo:1", 1) {
		t.Fatal("EC2 client token is not stable within a provisioning epoch")
	}
}

func TestCleanupRunnerTerminatesEveryDuplicate(t *testing.T) {
	tags := []types.Tag{
		{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")},
		{Key: awssdk.String(jobTagKey), Value: awssdk.String(jobTag("org/repo:42"))},
	}
	api := &fakeEC2{describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
		Instances: []types.Instance{
			{InstanceId: awssdk.String("i-one"), Tags: tags},
			{InstanceId: awssdk.String("i-two"), Tags: tags},
		},
	}}}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	if _, _, err := client.FindRunner(context.Background(), "org/repo:42"); err == nil {
		t.Fatal("FindRunner accepted duplicate EC2 instances")
	}
	if err := client.CleanupRunner(context.Background(), "org/repo:42"); err != nil {
		t.Fatal(err)
	}
	if len(api.terminated) != 2 || api.terminated[0] != "i-one" || api.terminated[1] != "i-two" {
		t.Fatalf("terminated instances = %#v", api.terminated)
	}
}
