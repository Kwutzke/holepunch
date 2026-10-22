package bastion

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/Kwutzke/holepunch/internal/config"
)

// ASGClient is the subset of the AWS autoscaling API the resolver uses.
type ASGClient interface {
	DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, opts ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

// SSMClient is the subset of the AWS SSM API the resolver uses. The shape
// matches ssm.DescribeInstanceInformationAPIClient so the SDK's paginator
// can accept an SSMClient directly.
type SSMClient interface {
	DescribeInstanceInformation(ctx context.Context, in *ssm.DescribeInstanceInformationInput, opts ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
}

// ClientFactory returns an ASG+SSM client pair bound to a specific AWS
// profile + region. Injectable so tests can substitute fakes without
// touching AWS.
type ClientFactory func(ctx context.Context, awsProfile, awsRegion string) (ASGClient, SSMClient, error)

// ASGResolver resolves a profile's SSM target by picking an InService +
// SSM-Online instance from the profile's auto-scaling group. It caches
// the resolved ID per (aws profile, region, asg) triple until Invalidate
// is called.
type ASGResolver struct {
	mu      sync.Mutex
	cache   map[cacheKey]string
	factory ClientFactory
}

// NewASGResolver returns a resolver backed by real AWS clients.
func NewASGResolver() *ASGResolver {
	return &ASGResolver{
		cache:   make(map[cacheKey]string),
		factory: defaultClientFactory,
	}
}

// NewASGResolverWithFactory returns a resolver that uses the supplied
// ClientFactory. Used by tests.
func NewASGResolverWithFactory(f ClientFactory) *ASGResolver {
	return &ASGResolver{
		cache:   make(map[cacheKey]string),
		factory: f,
	}
}

// Resolve returns the profile's pinned Target without calling AWS, else an
// SSM-Online instance of its TargetASG, cached until Invalidate.
//
// r.mu stays held across the AWS call: concurrent cold resolves of one ASG
// cost one call instead of N, and Invalidate cannot land between lookup and
// cache write. holepunch runs ≤ ~10 services, so serializing is cheap.
func (r *ASGResolver) Resolve(ctx context.Context, profile config.Profile) (string, error) {
	if profile.Target != "" {
		return profile.Target, nil
	}
	if profile.TargetASG == "" {
		return "", fmt.Errorf("bastion: profile %q has neither target nor target_asg", profile.AWSProfile)
	}

	key := keyFor(profile)

	r.mu.Lock()
	defer r.mu.Unlock()

	if id, ok := r.cache[key]; ok {
		return id, nil
	}

	asgClient, ssmClient, err := r.factory(ctx, profile.AWSProfile, profile.AWSRegion)
	if err != nil {
		return "", fmt.Errorf("bastion: building aws clients for profile %q: %w", profile.AWSProfile, err)
	}

	id, err := lookupOnlineInstance(ctx, asgClient, ssmClient, profile.TargetASG)
	if err != nil {
		return "", err
	}
	r.cache[key] = id
	return id, nil
}

// Invalidate drops the cached instance ID for the profile. Called when a
// session fails with ErrTargetNotConnected so the next Resolve hits AWS
// and picks a freshly-running instance.
func (r *ASGResolver) Invalidate(profile config.Profile) {
	r.mu.Lock()
	delete(r.cache, keyFor(profile))
	r.mu.Unlock()
}

// cacheKey is the per-(profile,region,asg) lookup key. Using a struct
// instead of a delimiter-joined string means no value can collide with
// another by containing the delimiter character.
type cacheKey struct {
	awsProfile string
	awsRegion  string
	targetASG  string
}

func keyFor(profile config.Profile) cacheKey {
	return cacheKey{
		awsProfile: profile.AWSProfile,
		awsRegion:  profile.AWSRegion,
		targetASG:  profile.TargetASG,
	}
}

func lookupOnlineInstance(ctx context.Context, asgClient ASGClient, ssmClient SSMClient, asgName string) (string, error) {
	asgOut, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return "", fmt.Errorf("describing asg %q: %w", asgName, err)
	}
	if len(asgOut.AutoScalingGroups) == 0 {
		return "", fmt.Errorf("asg %q not found", asgName)
	}

	inService := make(map[string]struct{})
	for _, inst := range asgOut.AutoScalingGroups[0].Instances {
		if inst.LifecycleState == astypes.LifecycleStateInService && inst.InstanceId != nil {
			inService[*inst.InstanceId] = struct{}{}
		}
	}
	if len(inService) == 0 {
		return "", fmt.Errorf("asg %q has no in-service instances", asgName)
	}

	candidates := make([]string, 0, len(inService))
	for id := range inService {
		candidates = append(candidates, id)
	}

	// Paginate: SSM returns at most 50 entries per page. An ASG with more
	// than 50 InService instances would silently miss candidates on later
	// pages without this loop.
	paginator := ssm.NewDescribeInstanceInformationPaginator(ssmClient, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{
			{Key: aws.String("InstanceIds"), Values: candidates},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("describing ssm info for asg %q: %w", asgName, err)
		}
		for _, info := range page.InstanceInformationList {
			if info.PingStatus == ssmtypes.PingStatusOnline && info.InstanceId != nil {
				if _, ok := inService[*info.InstanceId]; ok {
					return *info.InstanceId, nil
				}
			}
		}
	}
	return "", fmt.Errorf("asg %q: no ssm-online instances among %d candidates", asgName, len(candidates))
}

func defaultClientFactory(ctx context.Context, awsProfile, awsRegion string) (ASGClient, SSMClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithSharedConfigProfile(awsProfile),
		awsconfig.WithRegion(awsRegion),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("loading AWS config for profile %s: %w", awsProfile, err)
	}
	return autoscaling.NewFromConfig(cfg), ssm.NewFromConfig(cfg), nil
}
