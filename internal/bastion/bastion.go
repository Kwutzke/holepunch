// Package bastion resolves the EC2 instance an SSM session targets from the
// profile's auto-scaling group, because instance replacement (AMI refresh,
// patching) leaves a configured instance ID permanently stale. Its port is
// engine.BastionResolver, declared at the consumer rather than here.
package bastion
