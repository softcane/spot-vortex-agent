package capacity

import (
	"context"
	"fmt"
	"sync"
)

// ASGInfo describes an Auto Scaling Group discovered for SpotVortex management.
type ASGInfo struct {
	// ASGID is the ASG name or ARN.
	ASGID string

	// Pool is the workload pool name (from spotvortex.io/pool tag).
	Pool string

	// CapacityType is "spot" or "on-demand".
	CapacityType string

	// DesiredCapacity is the current desired count.
	DesiredCapacity int32

	// CurrentCount is the current number of running instances.
	CurrentCount int32

	// MaxSize is the ASG max size.
	MaxSize int32
}

// ASGClient abstracts AWS Auto Scaling Group operations.
// This interface enables testing with a fake client in Kind clusters.
type ASGClient interface {
	// DiscoverTwinASGs finds paired Spot/OD ASGs for a workload pool.
	// Discovery uses tags: spotvortex.io/pool=<pool>, spotvortex.io/capacity-type=spot|on-demand.
	DiscoverTwinASGs(ctx context.Context, pool string) (spot *ASGInfo, od *ASGInfo, err error)

	// SetDesiredCapacity updates the desired capacity of an ASG.
	SetDesiredCapacity(ctx context.Context, asgID string, desired int32) error

	// TerminateInstance terminates a specific instance in an ASG and decrements desired capacity.
	TerminateInstance(ctx context.Context, asgID string, instanceID string, decrementDesired bool) error

	// GetInstanceASG returns the ASG ID for a given EC2 instance ID.
	GetInstanceASG(ctx context.Context, instanceID string) (string, error)
}

// FakeASGClient implements ASGClient for testing in Kind clusters.
// It simulates Twin ASG discovery and scaling operations in memory.
type FakeASGClient struct {
	mu   sync.Mutex
	asgs map[string]*ASGInfo // asgID -> info

	// ScaleUpCalls tracks calls to SetDesiredCapacity for assertions.
	ScaleUpCalls []fakeScaleCall
	// TerminateCalls tracks calls to TerminateInstance for assertions.
	TerminateCalls []fakeTerminateCall
}

type fakeScaleCall struct {
	ASGID   string
	Desired int32
}

type fakeTerminateCall struct {
	ASGID      string
	InstanceID string
	Decrement  bool
}

// NewFakeASGClient creates a fake ASG client pre-populated with twin ASG pairs.
func NewFakeASGClient() *FakeASGClient {
	return &FakeASGClient{
		asgs: make(map[string]*ASGInfo),
	}
}

// AddTwinPair registers a spot/OD ASG pair for a workload pool.
func (f *FakeASGClient) AddTwinPair(pool string, spotDesired, odDesired int32) {
	f.mu.Lock()
	defer f.mu.Unlock()

	spotID := pool + "-spot-asg"
	odID := pool + "-od-asg"

	f.asgs[spotID] = &ASGInfo{
		ASGID:           spotID,
		Pool:            pool,
		CapacityType:    "spot",
		DesiredCapacity: spotDesired,
		CurrentCount:    spotDesired,
		MaxSize:         spotDesired + 5,
	}
	f.asgs[odID] = &ASGInfo{
		ASGID:           odID,
		Pool:            pool,
		CapacityType:    "on-demand",
		DesiredCapacity: odDesired,
		CurrentCount:    odDesired,
		MaxSize:         odDesired + 5,
	}
}

func (f *FakeASGClient) DiscoverTwinASGs(ctx context.Context, pool string) (*ASGInfo, *ASGInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	spotID := pool + "-spot-asg"
	odID := pool + "-od-asg"

	spot, spotOK := f.asgs[spotID]
	od, odOK := f.asgs[odID]

	if !spotOK || !odOK {
		return nil, nil, fmt.Errorf("twin ASG pair not found for pool %q", pool)
	}

	// Return copies to avoid data races
	spotCopy := *spot
	odCopy := *od
	return &spotCopy, &odCopy, nil
}

func (f *FakeASGClient) SetDesiredCapacity(ctx context.Context, asgID string, desired int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	asg, ok := f.asgs[asgID]
	if !ok {
		return fmt.Errorf("ASG %q not found", asgID)
	}
	if desired > asg.MaxSize {
		return fmt.Errorf("desired %d exceeds max %d for ASG %q", desired, asg.MaxSize, asgID)
	}

	asg.DesiredCapacity = desired
	// Simulate instant scaling for tests
	asg.CurrentCount = desired

	f.ScaleUpCalls = append(f.ScaleUpCalls, fakeScaleCall{
		ASGID:   asgID,
		Desired: desired,
	})

	return nil
}

func (f *FakeASGClient) TerminateInstance(ctx context.Context, asgID string, instanceID string, decrementDesired bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	asg, ok := f.asgs[asgID]
	if !ok {
		return fmt.Errorf("ASG %q not found", asgID)
	}

	if decrementDesired && asg.DesiredCapacity > 0 {
		asg.DesiredCapacity--
	}
	if asg.CurrentCount > 0 {
		asg.CurrentCount--
	}

	f.TerminateCalls = append(f.TerminateCalls, fakeTerminateCall{
		ASGID:      asgID,
		InstanceID: instanceID,
		Decrement:  decrementDesired,
	})

	return nil
}

func (f *FakeASGClient) GetInstanceASG(ctx context.Context, instanceID string) (string, error) {
	// In the fake, we don't track individual instances.
	// Return error to signal callers should use node labels instead.
	return "", fmt.Errorf("fake client: use node labels for ASG discovery")
}

// GetASG returns the current state of an ASG (for test assertions).
func (f *FakeASGClient) GetASG(asgID string) *ASGInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if asg, ok := f.asgs[asgID]; ok {
		copy := *asg
		return &copy
	}
	return nil
}

// Compile-time interface check.
var _ ASGClient = (*FakeASGClient)(nil)
