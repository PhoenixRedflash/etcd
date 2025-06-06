// Copyright 2017 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !cluster_proxy

package connectivity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	integration2 "go.etcd.io/etcd/tests/v3/framework/integration"
	clientv3test "go.etcd.io/etcd/tests/v3/integration/clientv3"
)

var errExpected = errors.New("expected error")

func isErrorExpected(err error) bool {
	return clientv3test.IsClientTimeout(err) || clientv3test.IsServerCtxTimeout(err) ||
		errors.Is(err, rpctypes.ErrTimeout) || errors.Is(err, rpctypes.ErrTimeoutDueToLeaderFail)
}

// TestBalancerUnderNetworkPartitionPut tests when one member becomes isolated,
// first Put request fails, and following retry succeeds with client balancer
// switching to others.
func TestBalancerUnderNetworkPartitionPut(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Put(ctx, "a", "b")
		if isErrorExpected(err) {
			return errExpected
		}
		return err
	}, time.Second)
}

func TestBalancerUnderNetworkPartitionDelete(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Delete(ctx, "a")
		if isErrorExpected(err) {
			return errExpected
		}
		return err
	}, time.Second)
}

func TestBalancerUnderNetworkPartitionTxn(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Txn(ctx).
			If(clientv3.Compare(clientv3.Version("foo"), "=", 0)).
			Then(clientv3.OpPut("foo", "bar")).
			Else(clientv3.OpPut("foo", "baz")).Commit()
		if isErrorExpected(err) {
			return errExpected
		}
		return err
	}, time.Second)
}

// TestBalancerUnderNetworkPartitionLinearizableGetWithLongTimeout tests
// when one member becomes isolated, first quorum Get request succeeds
// by switching endpoints within the timeout (long enough to cover endpoint switch).
func TestBalancerUnderNetworkPartitionLinearizableGetWithLongTimeout(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Get(ctx, "a")
		if isErrorExpected(err) {
			return errExpected
		}
		return err
	}, 7*time.Second)
}

// TestBalancerUnderNetworkPartitionLinearizableGetWithShortTimeout tests
// when one member becomes isolated, first quorum Get request fails,
// and following retry succeeds with client balancer switching to others.
func TestBalancerUnderNetworkPartitionLinearizableGetWithShortTimeout(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Get(ctx, "a")
		if clientv3test.IsClientTimeout(err) || clientv3test.IsServerCtxTimeout(err) {
			return errExpected
		}
		return err
	}, time.Second)
}

func TestBalancerUnderNetworkPartitionSerializableGet(t *testing.T) {
	testBalancerUnderNetworkPartition(t, func(cli *clientv3.Client, ctx context.Context) error {
		_, err := cli.Get(ctx, "a", clientv3.WithSerializable())
		return err
	}, time.Second)
}

func testBalancerUnderNetworkPartition(t *testing.T, op func(*clientv3.Client, context.Context) error, timeout time.Duration) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{
		Size: 3,
	})
	defer clus.Terminate(t)

	eps := []string{clus.Members[0].GRPCURL, clus.Members[1].GRPCURL, clus.Members[2].GRPCURL}

	// expect pin eps[0]
	ccfg := clientv3.Config{
		Endpoints:   []string{eps[0]},
		DialTimeout: 3 * time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	}
	cli, err := integration2.NewClient(t, ccfg)
	require.NoError(t, err)
	defer cli.Close()
	// wait for eps[0] to be pinned
	clientv3test.MustWaitPinReady(t, cli)

	// add other endpoints for later endpoint switch
	cli.SetEndpoints(eps...)
	time.Sleep(time.Second * 2)
	clus.Members[0].InjectPartition(t, clus.Members[1:]...)

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		err = op(cli, ctx)
		t.Logf("Op returned error: %v", err)
		t.Log("Cancelling...")
		cancel()
		if err == nil {
			break
		}
		if !errors.Is(err, errExpected) {
			t.Errorf("#%d: expected '%v', got '%v'", i, errExpected, err)
		}
		// give enough time for endpoint switch
		// TODO: remove random sleep by syncing directly with balancer
		if i == 0 {
			time.Sleep(5 * time.Second)
		}
	}
	if err != nil {
		t.Errorf("balancer did not switch in time (%v)", err)
	}
}

// TestBalancerUnderNetworkPartitionLinearizableGetLeaderElection ensures balancer
// switches endpoint when leader fails and linearizable get requests returns
// "etcdserver: request timed out".
func TestBalancerUnderNetworkPartitionLinearizableGetLeaderElection(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{
		Size: 3,
	})
	defer clus.Terminate(t)
	eps := []string{clus.Members[0].GRPCURL, clus.Members[1].GRPCURL, clus.Members[2].GRPCURL}

	lead := clus.WaitLeader(t)

	timeout := 3 * clus.Members[(lead+1)%2].ServerConfig.ReqTimeout()

	cli, err := integration2.NewClient(t, clientv3.Config{
		Endpoints:   []string{eps[(lead+1)%2]},
		DialTimeout: 2 * time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	})
	require.NoError(t, err)
	defer cli.Close()

	// add all eps to list, so that when the original pined one fails
	// the client can switch to other available eps
	cli.SetEndpoints(eps[lead], eps[(lead+1)%2])

	// isolate leader
	clus.Members[lead].InjectPartition(t, clus.Members[(lead+1)%3], clus.Members[(lead+2)%3])

	// expects balancer to round robin to leader within two attempts
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		_, err = cli.Get(ctx, "a")
		cancel()
		if err == nil {
			break
		}
	}
	require.NoError(t, err)
}

func TestBalancerUnderNetworkPartitionWatchLeader(t *testing.T) {
	testBalancerUnderNetworkPartitionWatch(t, true)
}

func TestBalancerUnderNetworkPartitionWatchFollower(t *testing.T) {
	testBalancerUnderNetworkPartitionWatch(t, false)
}

// testBalancerUnderNetworkPartitionWatch ensures watch stream
// to a partitioned node be closed when context requires leader.
func testBalancerUnderNetworkPartitionWatch(t *testing.T, isolateLeader bool) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{
		Size: 3,
	})
	defer clus.Terminate(t)

	eps := []string{clus.Members[0].GRPCURL, clus.Members[1].GRPCURL, clus.Members[2].GRPCURL}

	target := clus.WaitLeader(t)
	if !isolateLeader {
		target = (target + 1) % 3
	}

	// pin eps[target]
	watchCli, err := integration2.NewClient(t, clientv3.Config{Endpoints: []string{eps[target]}})
	require.NoError(t, err)
	t.Logf("watchCli created to: %v", target)
	defer watchCli.Close()

	// wait for eps[target] to be connected
	clientv3test.MustWaitPinReady(t, watchCli)
	t.Logf("successful connection with server: %v", target)

	// We stick to the original endpoint, so when the one fails we don't switch
	// under the cover to other available eps, but expose the failure to the
	// caller (test assertion).

	wch := watchCli.Watch(clientv3.WithRequireLeader(t.Context()), "foo", clientv3.WithCreatedNotify())
	select {
	case <-wch:
	case <-time.After(integration2.RequestWaitTimeout):
		t.Fatal("took too long to create watch")
	}

	t.Logf("watch established")

	// isolate eps[target]
	clus.Members[target].InjectPartition(t,
		clus.Members[(target+1)%3],
		clus.Members[(target+2)%3],
	)

	select {
	case ev := <-wch:
		if len(ev.Events) != 0 {
			t.Fatal("expected no event")
		}
		require.ErrorIs(t, ev.Err(), rpctypes.ErrNoLeader)
	case <-time.After(integration2.RequestWaitTimeout): // enough time to detect leader lost
		t.Fatal("took too long to detect leader lost")
	}
}

func TestDropReadUnderNetworkPartition(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{
		Size: 3,
	})
	defer clus.Terminate(t)
	leaderIndex := clus.WaitLeader(t)
	// get a follower endpoint
	eps := []string{clus.Members[(leaderIndex+1)%3].GRPCURL}
	ccfg := clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 10 * time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	}
	cli, err := integration2.NewClient(t, ccfg)
	require.NoError(t, err)
	defer cli.Close()

	// wait for eps[0] to be pinned
	clientv3test.MustWaitPinReady(t, cli)

	// add other endpoints for later endpoint switch
	cli.SetEndpoints(eps...)
	time.Sleep(time.Second * 2)
	conn, err := cli.Dial(clus.Members[(leaderIndex+1)%3].GRPCURL)
	require.NoError(t, err)
	defer conn.Close()

	clus.Members[leaderIndex].InjectPartition(t, clus.Members[(leaderIndex+1)%3], clus.Members[(leaderIndex+2)%3])
	kvc := clientv3.NewKVFromKVClient(pb.NewKVClient(conn), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	_, err = kvc.Get(ctx, "a")
	cancel()
	require.ErrorIsf(t, err, rpctypes.ErrLeaderChanged, "expected %v, got %v", rpctypes.ErrLeaderChanged, err)

	for i := 0; i < 5; i++ {
		ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
		_, err = kvc.Get(ctx, "a")
		cancel()
		if err != nil {
			if errors.Is(err, rpctypes.ErrTimeout) {
				<-time.After(time.Second)
				i++
				continue
			}
			t.Fatalf("expected nil or timeout, got %v", err)
		}
		// No error returned and no retry required
		break
	}
}
