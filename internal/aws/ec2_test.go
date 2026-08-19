package aws

import (
	"context"
	"errors"
	"testing"
	"text/template"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type fakeEC2 struct {
	runInput       *ec2.RunInstancesInput
	describeOutput *ec2.DescribeInstancesOutput
	describePages  []*ec2.DescribeInstancesOutput
	describeTokens []string
	describeErr    error
	terminateErr   error
	terminated     []string
}

func (f *fakeEC2) DescribeInstances(_ context.Context, input *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if len(f.describePages) != 0 {
		token := ""
		if input.NextToken != nil {
			token = *input.NextToken
		}
		f.describeTokens = append(f.describeTokens, token)
		page := f.describePages[0]
		f.describePages = f.describePages[1:]
		return page, nil
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

func TestDeleteTreatsTerminatedUntaggedInstanceAsGone(t *testing.T) {
	api := &fakeEC2{describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
		Instances: []types.Instance{{
			InstanceId: awssdk.String("i-terminated"),
			State:      &types.InstanceState{Name: types.InstanceStateNameTerminated},
		}},
	}}}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	if err := client.DeleteRunner(context.Background(), "i-terminated", "org/repo:42"); err != nil {
		t.Fatalf("DeleteRunner terminated instance error = %v", err)
	}
	if len(api.terminated) != 0 {
		t.Fatalf("terminated an already terminated instance: %v", api.terminated)
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

func TestFindRunnerDetectsDuplicatesAcrossPages(t *testing.T) {
	jobKey := "org/repo:paginated-duplicate"
	tags := []types.Tag{
		{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")},
		{Key: awssdk.String(jobTagKey), Value: awssdk.String(jobTag(jobKey))},
	}
	api := &fakeEC2{describePages: []*ec2.DescribeInstancesOutput{
		{Reservations: []types.Reservation{{Instances: []types.Instance{{InstanceId: awssdk.String("i-page-one"), Tags: tags}}}}, NextToken: awssdk.String("page-2")},
		{Reservations: []types.Reservation{{Instances: []types.Instance{{InstanceId: awssdk.String("i-page-two"), Tags: tags}}}}},
	}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	if _, _, err := client.FindRunner(context.Background(), jobKey); !errors.Is(err, compute.ErrDuplicateInstances) {
		t.Fatalf("paginated duplicate error = %v", err)
	}
	if len(api.describeTokens) != 2 || api.describeTokens[0] != "" || api.describeTokens[1] != "page-2" {
		t.Fatalf("pagination tokens = %v", api.describeTokens)
	}
}

func TestSweepOrphanedRunnersPreservesKnownAndDeletesOldUnknown(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	tags := []types.Tag{{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")}}
	api := &fakeEC2{describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
		Instances: []types.Instance{
			{InstanceId: awssdk.String("i-known"), LaunchTime: &old, Tags: tags},
			{InstanceId: awssdk.String("i-orphan"), LaunchTime: &old, Tags: tags},
		},
	}}}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	deleted, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"i-known": {}}, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || len(api.terminated) != 1 || api.terminated[0] != "i-orphan" {
		t.Fatalf("sweep = %d, %v, terminated=%v", deleted, err, api.terminated)
	}
}

func TestSweepOrphanedRunnersFollowsPagination(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	tags := []types.Tag{{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")}}
	api := &fakeEC2{describePages: []*ec2.DescribeInstancesOutput{
		{NextToken: awssdk.String("page-2")},
		{Reservations: []types.Reservation{{Instances: []types.Instance{{
			InstanceId: awssdk.String("i-page-two"), LaunchTime: &old, Tags: tags,
		}}}}},
	}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || len(api.terminated) != 1 || api.terminated[0] != "i-page-two" {
		t.Fatalf("paginated sweep = %d, %v, terminated=%v", deleted, err, api.terminated)
	}
	if len(api.describeTokens) != 2 || api.describeTokens[0] != "" || api.describeTokens[1] != "page-2" {
		t.Fatalf("pagination tokens = %v", api.describeTokens)
	}
}

func TestSweepOrphanedRunnersFailsClosedWithoutLaunchTime(t *testing.T) {
	tags := []types.Tag{{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")}}
	api := &fakeEC2{describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
		Instances: []types.Instance{{InstanceId: awssdk.String("i-undated"), Tags: tags}},
	}}}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err == nil || deleted != 0 || len(api.terminated) != 0 {
		t.Fatalf("undated sweep = %d, %v, terminated=%v", deleted, err, api.terminated)
	}
}

func TestConcurrentSweepFindAndCreatePreserveFreshRunner(t *testing.T) {
	jobKey := "org/repo:concurrent"
	created := time.Now()
	tags := []types.Tag{
		{Key: awssdk.String(controllerTagKey), Value: awssdk.String("primary")},
		{Key: awssdk.String(jobTagKey), Value: awssdk.String(jobTag(jobKey))},
	}
	api := &fakeEC2{describeOutput: &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{
		Instances: []types.Instance{{InstanceId: awssdk.String("i-concurrent"), LaunchTime: &created, Tags: tags}},
	}}}}
	client := newClient(api, nil, Config{ControllerID: "primary"})
	errorsCh := make(chan error, 3)
	go func() { _, _, err := client.FindRunner(context.Background(), jobKey); errorsCh <- err }()
	go func() {
		_, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: jobKey})
		errorsCh <- err
	}()
	go func() {
		_, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
		errorsCh <- err
	}()
	for range 3 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if len(api.terminated) != 0 {
		t.Fatalf("concurrent sweep terminated fresh runner: %v", api.terminated)
	}
}
