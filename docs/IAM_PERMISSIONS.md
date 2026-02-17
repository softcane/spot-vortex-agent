# IAM Permissions for SpotVortex

SpotVortex requires AWS IAM permissions to read spot pricing data and (in active mode) manage Auto Scaling Groups.

## Authentication

SpotVortex uses IRSA (IAM Roles for Service Accounts) on EKS. Annotate the service account in `values.yaml`:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/spotvortex-shadow
```

## Shadow Mode (Read-Only)

Shadow mode (`dryRun: true`, the default) requires only read permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SpotVortexShadowMode",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeSpotPriceHistory",
        "ec2:DescribeInstances",
        "pricing:GetProducts",
        "autoscaling:DescribeAutoScalingGroups",
        "autoscaling:DescribeAutoScalingInstances"
      ],
      "Resource": "*"
    }
  ]
}
```

See also: [docs/iam-policy-shadow.json](iam-policy-shadow.json)

## Active Mode (Read + Write)

Active mode (`dryRun: false`) adds write permissions for ASG management:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SpotVortexActiveMode",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeSpotPriceHistory",
        "ec2:DescribeInstances",
        "pricing:GetProducts",
        "autoscaling:DescribeAutoScalingGroups",
        "autoscaling:DescribeAutoScalingInstances",
        "autoscaling:SetDesiredCapacity",
        "autoscaling:TerminateInstanceInAutoScalingGroup"
      ],
      "Resource": "*"
    }
  ]
}
```

See also: [docs/iam-policy-active.json](iam-policy-active.json)

## Startup Validation

SpotVortex performs a lightweight IAM canary check at startup by calling `ec2:DescribeSpotPriceHistory` with a 1-result limit. If this fails, the agent logs a clear warning with a link to this document.

## Scope Restrictions

To restrict permissions to specific ASGs, replace `"Resource": "*"` with ASG ARN patterns:

```json
"Resource": "arn:aws:autoscaling:REGION:ACCOUNT:autoScalingGroup:*:autoScalingGroupName/spotvortex-*"
```
