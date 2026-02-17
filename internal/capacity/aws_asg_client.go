package capacity

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// AWSASGClientConfig configures the real AWS ASG client.
type AWSASGClientConfig struct {
	// Region is the AWS region for API calls.
	Region string

	// PoolTagKey is the ASG tag key for workload pool name.
	// Default: "spotvortex.io/pool"
	PoolTagKey string

	// CapacityTagKey is the ASG tag key for capacity type (spot/on-demand).
	// Default: "spotvortex.io/capacity-type"
	CapacityTagKey string
}

// AWSASGClient implements ASGClient using the real AWS Auto Scaling API.
type AWSASGClient struct {
	asgClient  *autoscaling.Client
	logger     *slog.Logger
	poolTagKey string
	capTagKey  string
}

// NewAWSASGClient creates a real AWS ASG client.
func NewAWSASGClient(ctx context.Context, cfg AWSASGClientConfig) (*AWSASGClient, error) {
	if cfg.PoolTagKey == "" {
		cfg.PoolTagKey = "spotvortex.io/pool"
	}
	if cfg.CapacityTagKey == "" {
		cfg.CapacityTagKey = "spotvortex.io/capacity-type"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &AWSASGClient{
		asgClient:  autoscaling.NewFromConfig(awsCfg),
		logger:     slog.Default(),
		poolTagKey: cfg.PoolTagKey,
		capTagKey:  cfg.CapacityTagKey,
	}, nil
}

// DiscoverTwinASGs finds paired Spot/OD ASGs for a workload pool using tag filters.
// Uses pagination to handle accounts with many ASGs.
func (c *AWSASGClient) DiscoverTwinASGs(ctx context.Context, pool string) (*ASGInfo, *ASGInfo, error) {
	input := &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:" + c.poolTagKey),
				Values: []string{pool},
			},
		},
	}

	var spot, od *ASGInfo
	for {
		result, err := c.asgClient.DescribeAutoScalingGroups(ctx, input)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to describe ASGs for pool %q: %w", pool, err)
		}

		for _, asg := range result.AutoScalingGroups {
			info := asgInfoFromAWS(asg, c.poolTagKey, c.capTagKey)
			if info == nil {
				continue
			}

			switch info.CapacityType {
			case "spot":
				spot = info
			case "on-demand":
				od = info
			}
		}

		// Both found or no more pages
		if (spot != nil && od != nil) || result.NextToken == nil {
			break
		}
		input.NextToken = result.NextToken
	}

	if spot == nil || od == nil {
		return nil, nil, fmt.Errorf("twin ASG pair not found for pool %q (spot=%v, od=%v)",
			pool, spot != nil, od != nil)
	}

	c.logger.Info("discovered twin ASG pair",
		"pool", pool,
		"spot_asg", spot.ASGID,
		"od_asg", od.ASGID,
	)

	return spot, od, nil
}

// SetDesiredCapacity updates the desired capacity of an ASG.
func (c *AWSASGClient) SetDesiredCapacity(ctx context.Context, asgID string, desired int32) error {
	input := &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(asgID),
		DesiredCapacity:      aws.Int32(desired),
	}

	_, err := c.asgClient.SetDesiredCapacity(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to set desired capacity for ASG %q to %d: %w", asgID, desired, err)
	}

	c.logger.Info("set ASG desired capacity",
		"asg", asgID,
		"desired", desired,
	)

	return nil
}

// TerminateInstance terminates a specific instance in an ASG.
func (c *AWSASGClient) TerminateInstance(ctx context.Context, asgID string, instanceID string, decrementDesired bool) error {
	input := &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     aws.String(instanceID),
		ShouldDecrementDesiredCapacity: aws.Bool(decrementDesired),
	}

	_, err := c.asgClient.TerminateInstanceInAutoScalingGroup(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to terminate instance %s in ASG %s: %w", instanceID, asgID, err)
	}

	c.logger.Info("terminated instance in ASG",
		"asg", asgID,
		"instance", instanceID,
		"decrement_desired", decrementDesired,
	)

	return nil
}

// GetInstanceASG returns the ASG ID for a given EC2 instance ID.
func (c *AWSASGClient) GetInstanceASG(ctx context.Context, instanceID string) (string, error) {
	input := &autoscaling.DescribeAutoScalingInstancesInput{
		InstanceIds: []string{instanceID},
	}

	result, err := c.asgClient.DescribeAutoScalingInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe ASG instance %s: %w", instanceID, err)
	}

	if len(result.AutoScalingInstances) == 0 {
		return "", fmt.Errorf("instance %s not found in any ASG", instanceID)
	}

	asgName := result.AutoScalingInstances[0].AutoScalingGroupName
	if asgName == nil {
		return "", fmt.Errorf("instance %s has no ASG name", instanceID)
	}

	return *asgName, nil
}

// asgInfoFromAWS converts an AWS ASG to our ASGInfo, extracting tag values.
func asgInfoFromAWS(asg types.AutoScalingGroup, poolTagKey, capTagKey string) *ASGInfo {
	if asg.AutoScalingGroupName == nil {
		return nil
	}

	info := &ASGInfo{
		ASGID:           *asg.AutoScalingGroupName,
		DesiredCapacity: aws.ToInt32(asg.DesiredCapacity),
		CurrentCount:    int32(len(asg.Instances)),
		MaxSize:         aws.ToInt32(asg.MaxSize),
	}

	for _, tag := range asg.Tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}
		switch *tag.Key {
		case poolTagKey:
			info.Pool = *tag.Value
		case capTagKey:
			info.CapacityType = *tag.Value
		}
	}

	if info.Pool == "" || info.CapacityType == "" {
		return nil
	}

	return info
}

// Compile-time interface check.
var _ ASGClient = (*AWSASGClient)(nil)
