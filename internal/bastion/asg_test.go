package bastion_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Kwutzke/holepunch/internal/bastion"
	"github.com/Kwutzke/holepunch/internal/config"
)

type fakeASGClient struct {
	resp  *autoscaling.DescribeAutoScalingGroupsOutput
	err   error
	calls int
}

func asgWithInstances(instances ...astypes.Instance) *fakeASGClient {
	return &fakeASGClient{
		resp: &autoscaling.DescribeAutoScalingGroupsOutput{
			AutoScalingGroups: []astypes.AutoScalingGroup{
				{Instances: instances},
			},
		},
	}
}

func (f *fakeASGClient) DescribeAutoScalingGroups(_ context.Context, _ *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.calls++
	return f.resp, f.err
}

type fakeSSMClient struct {
	resp  *ssm.DescribeInstanceInformationOutput
	err   error
	calls int
}

func ssmWithInfo(infos ...ssmtypes.InstanceInformation) *fakeSSMClient {
	return &fakeSSMClient{
		resp: &ssm.DescribeInstanceInformationOutput{
			InstanceInformationList: infos,
		},
	}
}

func (f *fakeSSMClient) DescribeInstanceInformation(_ context.Context, _ *ssm.DescribeInstanceInformationInput, _ ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	f.calls++
	return f.resp, f.err
}

func TestASGResolver_Resolve(t *testing.T) {
	t.Parallel()

	t.Run("target pin returns directly", func(t *testing.T) {
		t.Parallel()

		// When profile.Target is set, the resolver should honor it without
		// calling AWS. Use a factory that would explode if invoked.
		factory := func(_ context.Context, _, _ string) (bastion.ASGClient, bastion.SSMClient, error) {
			t.Fatalf("factory should not be called when Target is pinned")
			return nil, nil, nil
		}
		r := bastion.NewASGResolverWithFactory(factory)

		id, err := r.Resolve(t.Context(), config.Profile{Target: "i-pinned"})
		require.NoError(t, err)
		assert.Equal(t, "i-pinned", id)
	})

	t.Run("resolves and caches", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"), inService("i-bbb"))
		s := ssmWithInfo(online("i-bbb"))
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, s, nil))

		id, err := r.Resolve(t.Context(), asgProfile())
		require.NoError(t, err)
		assert.Equal(t, "i-bbb", id)
		assert.Equal(t, 1, asg.calls)
		assert.Equal(t, 1, s.calls)

		// Second call hits the cache — no additional AWS calls.
		id, err = r.Resolve(t.Context(), asgProfile())
		require.NoError(t, err)
		assert.Equal(t, "i-bbb", id)
		assert.Equal(t, 1, asg.calls)
		assert.Equal(t, 1, s.calls)
	})

	t.Run("neither target nor ASG", func(t *testing.T) {
		t.Parallel()

		r := bastion.NewASGResolverWithFactory(nil)
		_, err := r.Resolve(t.Context(), config.Profile{AWSProfile: "x", AWSRegion: "y"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "neither target nor target_asg")
	})

	t.Run("ASG not found", func(t *testing.T) {
		t.Parallel()

		asg := &fakeASGClient{resp: &autoscaling.DescribeAutoScalingGroupsOutput{}}
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, &fakeSSMClient{}, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("no in-service instances", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(astypes.Instance{
			InstanceId:     aws.String("i-pending"),
			LifecycleState: astypes.LifecycleStatePending,
		})
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, &fakeSSMClient{}, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no in-service instances")
	})

	t.Run("no SSM-online instance", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"))
		s := ssmWithInfo(ssmtypes.InstanceInformation{
			InstanceId: aws.String("i-aaa"),
			PingStatus: ssmtypes.PingStatusConnectionLost,
		})
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, s, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ssm-online instances")
	})

	t.Run("client factory error", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("boom")
		r := bastion.NewASGResolverWithFactory(fakeFactory(nil, nil, sentinel))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("ASG client error", func(t *testing.T) {
		t.Parallel()

		asg := &fakeASGClient{err: errors.New("asg-down")}
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, &fakeSSMClient{}, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "describing asg")
	})

	t.Run("SSM client error", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"))
		s := &fakeSSMClient{err: errors.New("ssm-down")}
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, s, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "describing ssm info")
	})

	// TestASGResolver_ConcurrentResolveDeduplicates guards the cache-stampede
	// fix: N goroutines resolving the same key concurrently must produce exactly
	// one AWS call, not N.
	t.Run("concurrent resolves deduplicate", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"))
		s := ssmWithInfo(online("i-aaa"))

		var factoryCalls atomic.Int32
		slowFactory := func(_ context.Context, _, _ string) (bastion.ASGClient, bastion.SSMClient, error) {
			factoryCalls.Add(1)
			// Hold briefly so concurrent Resolves pile up on the lock.
			time.Sleep(50 * time.Millisecond)
			return asg, s, nil
		}
		r := bastion.NewASGResolverWithFactory(slowFactory)

		const n = 20
		var wg sync.WaitGroup
		results := make([]string, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = r.Resolve(t.Context(), asgProfile())
			}()
		}
		wg.Wait()

		for i := range n {
			require.NoError(t, errs[i])
			assert.Equal(t, "i-aaa", results[i])
		}
		assert.Equal(t, int32(1), factoryCalls.Load(), "factory should be called exactly once")
		assert.Equal(t, 1, asg.calls, "ASG API should be called exactly once")
	})
}

func TestASGResolver_Invalidate(t *testing.T) {
	t.Parallel()

	t.Run("forces a refresh", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"))
		s := ssmWithInfo(online("i-aaa"))
		r := bastion.NewASGResolverWithFactory(fakeFactory(asg, s, nil))

		_, err := r.Resolve(t.Context(), asgProfile())
		require.NoError(t, err)

		r.Invalidate(asgProfile())

		_, err = r.Resolve(t.Context(), asgProfile())
		require.NoError(t, err)
		assert.Equal(t, 2, asg.calls, "expected Invalidate to drop cache and re-query ASG")
		assert.Equal(t, 2, s.calls)
	})

	// Regression: an in-flight resolve's cache write-back must not undo an
	// Invalidate issued during it (see the lock note on ASGResolver.Resolve).
	t.Run("during an in-flight resolve", func(t *testing.T) {
		t.Parallel()

		asg := asgWithInstances(inService("i-aaa"))
		s := ssmWithInfo(online("i-aaa"))

		factoryEntered := make(chan struct{})
		releaseFactory := make(chan struct{})
		var enterOnce sync.Once
		factory := func(_ context.Context, _, _ string) (bastion.ASGClient, bastion.SSMClient, error) {
			enterOnce.Do(func() {
				close(factoryEntered)
				<-releaseFactory
			})
			return asg, s, nil
		}
		r := bastion.NewASGResolverWithFactory(factory)

		resolveDone := make(chan struct{})
		go func() {
			defer close(resolveDone)
			id, err := r.Resolve(t.Context(), asgProfile())
			require.NoError(t, err)
			assert.Equal(t, "i-aaa", id)
		}()

		<-factoryEntered

		invalidateDone := make(chan struct{})
		go func() {
			defer close(invalidateDone)
			r.Invalidate(asgProfile())
		}()

		close(releaseFactory)
		<-resolveDone
		<-invalidateDone

		_, err := r.Resolve(t.Context(), asgProfile())
		require.NoError(t, err)
		assert.Equal(t, 2, asg.calls, "second Resolve should hit AWS because Invalidate dropped the cached entry")
	})
}

func fakeFactory(asg *fakeASGClient, s *fakeSSMClient, err error) bastion.ClientFactory {
	return func(_ context.Context, _, _ string) (bastion.ASGClient, bastion.SSMClient, error) {
		if err != nil {
			return nil, nil, err
		}
		return asg, s, nil
	}
}

func inService(id string) astypes.Instance {
	return astypes.Instance{
		InstanceId:     aws.String(id),
		LifecycleState: astypes.LifecycleStateInService,
	}
}

func online(id string) ssmtypes.InstanceInformation {
	return ssmtypes.InstanceInformation{
		InstanceId: aws.String(id),
		PingStatus: ssmtypes.PingStatusOnline,
	}
}

func asgProfile() config.Profile {
	return config.Profile{
		AWSProfile: "prod",
		AWSRegion:  "eu-central-1",
		TargetASG:  "my-asg",
	}
}
