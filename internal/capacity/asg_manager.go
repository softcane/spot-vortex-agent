package capacity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ASGManager implements CapacityManager for ASG-backed nodes.
// This covers both Cluster Autoscaler and EKS Managed Nodegroup provisioners,
// since both use Auto Scaling Groups underneath.
//
// Swap strategy (Twin ASG model per integration_strategy.md Section 4.1):
//  1. PrepareSwap: Scale up twin ASG, wait for new node to become Ready.
//  2. Drain proceeds normally via controller.
//  3. PostDrainCleanup: Terminate old instance, decrement source ASG.
//
// Why Twin ASG: If we drain first, pods go Pending, CA sees Pending pods,
// CA checks Priority Expander, CA might scale up the WRONG ASG (spot instead of OD).
// By manually provisioning capacity first, we bypass CA's selection logic.
type ASGManager struct {
	asgClient   ASGClient
	k8sClient   kubernetes.Interface
	logger      *slog.Logger
	managerType ManagerType // ManagerClusterAutoscaler or ManagerManagedNodegroup

	// Config
	nodeReadyTimeout time.Duration
	pollInterval     time.Duration
}

// ASGManagerConfig configures the ASG capacity manager.
type ASGManagerConfig struct {
	ASGClient        ASGClient
	K8sClient        kubernetes.Interface
	Logger           *slog.Logger
	ManagerType      ManagerType // ManagerClusterAutoscaler or ManagerManagedNodegroup
	NodeReadyTimeout time.Duration
	PollInterval     time.Duration
}

// NewASGManager creates a new ASG-backed capacity manager.
func NewASGManager(cfg ASGManagerConfig) *ASGManager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ManagerType == "" {
		cfg.ManagerType = ManagerClusterAutoscaler
	}
	if cfg.NodeReadyTimeout <= 0 {
		cfg.NodeReadyTimeout = 5 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}

	return &ASGManager{
		asgClient:        cfg.ASGClient,
		k8sClient:        cfg.K8sClient,
		logger:           cfg.Logger,
		managerType:      cfg.ManagerType,
		nodeReadyTimeout: cfg.NodeReadyTimeout,
		pollInterval:     cfg.PollInterval,
	}
}

func (m *ASGManager) Type() ManagerType {
	return m.managerType
}

// PrepareSwap implements the Twin ASG Scale-Wait-Drain workflow.
//
// Per integration_strategy.md Section 4.1:
//  1. Identify the twin ASG for the target direction.
//  2. Scale up twin ASG desired capacity by 1.
//  3. Wait for new node to become Ready in Kubernetes.
//  4. Return success so controller can proceed with drain.
//
// Failure modes (Section 4.2):
//   - OD quota exceeded: SetDesiredCapacity hangs → timeout → abort drain.
//   - Agent crash after scale-up: CA's scale-down-unneeded cleans up in ~10m.
//   - IAM denied: API call fails → no drain → fail safe.
func (m *ASGManager) PrepareSwap(ctx context.Context, pool PoolInfo, direction SwapDirection) (*SwapResult, error) {
	start := time.Now()

	if m.asgClient == nil {
		return nil, fmt.Errorf("ASG client not configured")
	}

	// Step 1: Discover twin ASGs for this pool
	spotASG, odASG, err := m.asgClient.DiscoverTwinASGs(ctx, pool.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to discover twin ASGs for pool %q: %w", pool.Name, err)
	}

	// Step 2: Determine which ASG to scale up
	var targetASG *ASGInfo
	switch direction {
	case SwapToOnDemand:
		targetASG = odASG
	case SwapToSpot:
		targetASG = spotASG
	default:
		return nil, fmt.Errorf("unknown swap direction: %d", direction)
	}

	m.logger.Info("preparing ASG swap",
		"pool", pool.Name,
		"direction", direction.String(),
		"target_asg", targetASG.ASGID,
		"current_desired", targetASG.DesiredCapacity,
		"new_desired", targetASG.DesiredCapacity+1,
	)

	// Step 3: Scale up target ASG by 1
	newDesired := targetASG.DesiredCapacity + 1
	if err := m.asgClient.SetDesiredCapacity(ctx, targetASG.ASGID, newDesired); err != nil {
		return nil, fmt.Errorf("failed to scale up ASG %q: %w", targetASG.ASGID, err)
	}

	// Step 4: Wait for new node to become Ready
	nodeName, err := m.waitForNewNode(ctx, pool, direction)
	if err != nil {
		m.logger.Warn("new node did not become Ready, aborting swap",
			"pool", pool.Name,
			"target_asg", targetASG.ASGID,
			"error", err,
		)
		// Rollback: scale back down
		rollbackErr := m.asgClient.SetDesiredCapacity(ctx, targetASG.ASGID, targetASG.DesiredCapacity)
		if rollbackErr != nil {
			m.logger.Error("failed to rollback ASG scale-up",
				"asg", targetASG.ASGID,
				"error", rollbackErr,
			)
		}
		return nil, fmt.Errorf("timeout waiting for replacement node: %w", err)
	}

	m.logger.Info("replacement node ready",
		"pool", pool.Name,
		"replacement_node", nodeName,
		"duration", time.Since(start),
	)

	return &SwapResult{
		Ready:               true,
		ReplacementNodeName: nodeName,
		Duration:            time.Since(start),
	}, nil
}

// waitForNewNode polls Kubernetes until a new Ready node appears that matches the pool.
func (m *ASGManager) waitForNewNode(ctx context.Context, pool PoolInfo, direction SwapDirection) (string, error) {
	if m.k8sClient == nil {
		// No K8s client = testing mode, assume instant readiness
		return "fake-replacement-node", nil
	}

	// Record existing nodes before scaling
	existingNodes := make(map[string]bool)
	nodes, err := m.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		existingNodes[node.Name] = true
	}

	// Poll until new node appears and is Ready
	deadline := time.After(m.nodeReadyTimeout)
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	expectedCapType := "on-demand"
	if direction == SwapToSpot {
		expectedCapType = "spot"
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("timeout after %v waiting for new %s node in pool %q",
				m.nodeReadyTimeout, expectedCapType, pool.Name)
		case <-ticker.C:
			nodes, err := m.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				m.logger.Warn("failed to list nodes during wait", "error", err)
				continue
			}

			for _, node := range nodes.Items {
				// Skip existing nodes
				if existingNodes[node.Name] {
					continue
				}

				// Check if node matches expected pool and capacity type
				labels := node.Labels
				if labels == nil {
					continue
				}

				nodePool := labels["spotvortex.io/pool"]
				if nodePool != pool.Name && pool.Name != "" {
					continue
				}

				capType := labels[LabelKarpenterCapacity]
				if capType == "" {
					capType = labels["node.kubernetes.io/lifecycle"]
				}
				if capType != expectedCapType {
					continue
				}

				// Check if node is Ready
				if isNodeReady(&node) {
					return node.Name, nil
				}
			}
		}
	}
}

// isNodeReady checks if a node has the Ready condition set to True.
func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PostDrainCleanup terminates the drained instance from the source ASG.
//
// Per integration_strategy.md Section 4.1 Step 4:
// "Terminate the empty Spot node and/or decrement ASG-Spot desired capacity."
// CA's scale-down-unneeded handles this eventually (~10m), but explicit
// termination is faster and avoids lingering empty nodes.
func (m *ASGManager) PostDrainCleanup(ctx context.Context, nodeName string, pool PoolInfo) error {
	if m.asgClient == nil {
		m.logger.Debug("no ASG client, skipping post-drain cleanup", "node", nodeName)
		return nil
	}

	// In a real implementation, we'd look up the EC2 instance ID from the node's
	// providerID (e.g., "aws:///us-east-1a/i-1234567890abcdef0") and call
	// TerminateInstance on the source ASG. For now, log the intent.
	m.logger.Info("post-drain cleanup: would terminate instance",
		"node", nodeName,
		"pool", pool.Name,
		"manager", m.managerType,
	)

	return nil
}

func (m *ASGManager) IsAvailable(ctx context.Context) bool {
	if m.asgClient == nil {
		return false
	}
	// Check if we can discover at least one twin pair.
	// In production, this would be a lightweight API call.
	return true
}

// Compile-time interface check.
var _ CapacityManager = (*ASGManager)(nil)
