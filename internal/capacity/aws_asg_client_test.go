package capacity

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// Compile-time interface check: AWSASGClient must implement ASGClient.
var _ ASGClient = (*AWSASGClient)(nil)

func TestASGInfoFromAWS_FullTags(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: aws.String("my-pool-spot-asg"),
		DesiredCapacity:      aws.Int32(3),
		MaxSize:              aws.Int32(10),
		Instances:            []types.Instance{{}, {}, {}},
		Tags: []types.TagDescription{
			{Key: aws.String("spotvortex.io/pool"), Value: aws.String("my-pool")},
			{Key: aws.String("spotvortex.io/capacity-type"), Value: aws.String("spot")},
		},
	}

	info := asgInfoFromAWS(asg, "spotvortex.io/pool", "spotvortex.io/capacity-type")
	if info == nil {
		t.Fatal("expected non-nil ASGInfo")
	}

	if info.ASGID != "my-pool-spot-asg" {
		t.Errorf("expected ASGID 'my-pool-spot-asg', got %q", info.ASGID)
	}
	if info.Pool != "my-pool" {
		t.Errorf("expected Pool 'my-pool', got %q", info.Pool)
	}
	if info.CapacityType != "spot" {
		t.Errorf("expected CapacityType 'spot', got %q", info.CapacityType)
	}
	if info.DesiredCapacity != 3 {
		t.Errorf("expected DesiredCapacity 3, got %d", info.DesiredCapacity)
	}
	if info.CurrentCount != 3 {
		t.Errorf("expected CurrentCount 3, got %d", info.CurrentCount)
	}
	if info.MaxSize != 10 {
		t.Errorf("expected MaxSize 10, got %d", info.MaxSize)
	}
}

func TestASGInfoFromAWS_MissingPoolTag(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: aws.String("my-asg"),
		DesiredCapacity:      aws.Int32(1),
		MaxSize:              aws.Int32(5),
		Tags: []types.TagDescription{
			{Key: aws.String("spotvortex.io/capacity-type"), Value: aws.String("spot")},
		},
	}

	info := asgInfoFromAWS(asg, "spotvortex.io/pool", "spotvortex.io/capacity-type")
	if info != nil {
		t.Fatal("expected nil ASGInfo when pool tag is missing")
	}
}

func TestASGInfoFromAWS_MissingCapacityTag(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: aws.String("my-asg"),
		DesiredCapacity:      aws.Int32(1),
		MaxSize:              aws.Int32(5),
		Tags: []types.TagDescription{
			{Key: aws.String("spotvortex.io/pool"), Value: aws.String("my-pool")},
		},
	}

	info := asgInfoFromAWS(asg, "spotvortex.io/pool", "spotvortex.io/capacity-type")
	if info != nil {
		t.Fatal("expected nil ASGInfo when capacity-type tag is missing")
	}
}

func TestASGInfoFromAWS_NilGroupName(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: nil,
		DesiredCapacity:      aws.Int32(1),
		MaxSize:              aws.Int32(5),
	}

	info := asgInfoFromAWS(asg, "spotvortex.io/pool", "spotvortex.io/capacity-type")
	if info != nil {
		t.Fatal("expected nil ASGInfo when group name is nil")
	}
}

func TestASGInfoFromAWS_CustomTagKeys(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: aws.String("custom-asg"),
		DesiredCapacity:      aws.Int32(2),
		MaxSize:              aws.Int32(8),
		Instances:            []types.Instance{{}, {}},
		Tags: []types.TagDescription{
			{Key: aws.String("custom/pool"), Value: aws.String("web-backend")},
			{Key: aws.String("custom/type"), Value: aws.String("on-demand")},
		},
	}

	info := asgInfoFromAWS(asg, "custom/pool", "custom/type")
	if info == nil {
		t.Fatal("expected non-nil ASGInfo with custom tags")
	}
	if info.Pool != "web-backend" {
		t.Errorf("expected Pool 'web-backend', got %q", info.Pool)
	}
	if info.CapacityType != "on-demand" {
		t.Errorf("expected CapacityType 'on-demand', got %q", info.CapacityType)
	}
}

func TestASGInfoFromAWS_NilTagValues(t *testing.T) {
	asg := types.AutoScalingGroup{
		AutoScalingGroupName: aws.String("asg-nil-tags"),
		DesiredCapacity:      aws.Int32(1),
		MaxSize:              aws.Int32(5),
		Tags: []types.TagDescription{
			{Key: aws.String("spotvortex.io/pool"), Value: nil},
			{Key: nil, Value: aws.String("spot")},
		},
	}

	info := asgInfoFromAWS(asg, "spotvortex.io/pool", "spotvortex.io/capacity-type")
	if info != nil {
		t.Fatal("expected nil ASGInfo when tag values are nil")
	}
}
