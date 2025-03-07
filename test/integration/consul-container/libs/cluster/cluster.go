package cluster

import (
	"context"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/serf/serf"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/teris-io/shortid"
	"github.com/testcontainers/testcontainers-go"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	libagent "github.com/hashicorp/consul/test/integration/consul-container/libs/agent"
)

// Cluster provides an interface for creating and controlling a Consul cluster
// in integration tests, with agents running in containers.
// These fields are public in the event someone might want to surgically
// craft a test case.
type Cluster struct {
	Agents      []libagent.Agent
	CACert      string
	CAKey       string
	ID          string
	Index       int
	Network     testcontainers.Network
	NetworkName string
}

// New creates a Consul cluster. An agent will be started for each of the given
// configs and joined to the cluster.
//
// A cluster has its own docker network for DNS connectivity, but is also joined
func New(configs []libagent.Config) (*Cluster, error) {
	id, err := shortid.Generate()
	if err != nil {
		return nil, errors.Wrap(err, "could not cluster id")
	}

	name := fmt.Sprintf("consul-int-cluster-%s", id)
	network, err := createNetwork(name)
	if err != nil {
		return nil, errors.Wrap(err, "could not create cluster container network")
	}

	cluster := Cluster{
		ID:          id,
		Network:     network,
		NetworkName: name,
	}

	if err := cluster.Add(configs); err != nil {
		return nil, errors.Wrap(err, "could not start or join all agents")
	}
	return &cluster, nil
}

// Add starts an agent with the given configuration and joins it with the existing cluster
func (c *Cluster) Add(configs []libagent.Config) error {

	agents := make([]libagent.Agent, len(configs))
	for idx, conf := range configs {
		n, err := libagent.NewConsulContainer(context.Background(), conf, c.NetworkName, c.Index)
		if err != nil {
			return errors.Wrapf(err, "could not add container index %d", idx)
		}
		agents[idx] = n
		c.Index++
	}
	if err := c.Join(agents); err != nil {
		return errors.Wrapf(err, "could not join agent")
	}
	return nil
}

// Join joins the given agent to the cluster.
func (c *Cluster) Join(agents []libagent.Agent) error {
	var joinAddr string
	if len(c.Agents) >= 1 {
		joinAddr, _ = c.Agents[0].GetAddr()
	} else if len(agents) >= 1 {
		joinAddr, _ = agents[0].GetAddr()
	}

	for _, n := range agents {
		err := n.GetClient().Agent().Join(joinAddr, false)
		if err != nil {
			return errors.Wrapf(err, "could not join agent %s to %s", n.GetName(), joinAddr)
		}
		c.Agents = append(c.Agents, n)
	}
	return nil
}

// Remove instructs the agent to leave the cluster then removes it
// from the cluster Agent list.
func (c *Cluster) Remove(n libagent.Agent) error {
	err := n.GetClient().Agent().Leave()
	if err != nil {
		return errors.Wrapf(err, "could not remove agent %s", n.GetName())
	}

	foundIdx := -1
	for idx, this := range c.Agents {
		if this == n {
			foundIdx = idx
			break
		}
	}

	if foundIdx == -1 {
		return errors.New("could not find agent in cluster")
	}

	c.Agents = append(c.Agents[:foundIdx], c.Agents[foundIdx+1:]...)
	return nil
}

// StandardUpgrade upgrades a running consul cluster following the steps from
//
//	https://developer.hashicorp.com/consul/docs/upgrading#standard-upgrades
//
// - takes a snapshot
// - terminate and rejoin the pod of new version of consul
func (c *Cluster) StandardUpgrade(t *testing.T, ctx context.Context, targetVersion string) error {
	retry.RunWith(&retry.Timer{Timeout: 30 * time.Second, Wait: 1 * time.Second}, t, func(r *retry.R) {
		// NOTE: to suppress flakiness
		execCode, err := c.Agents[0].Exec(context.Background(), []string{"consul", "snapshot", "save", "backup.snap"})
		require.Equal(r, 0, execCode)
		require.NoError(r, err)
	})

	// verify only the leader can take a snapshot
	snapshotCount := 0
	for _, agent := range c.Agents {
		if !agent.IsServer() {
			continue
		}

		files, err := ioutil.ReadDir(filepath.Join(agent.DataDir(), "raft", "snapshots"))
		if err != nil {
			return err
		}
		if len(files) >= 1 {
			snapshotCount++
		}
	}
	require.Equalf(t, 1, snapshotCount, "only leader agent can have a snapshot file")

	// Upgrade individual agent to the target version in the following order
	// 1. followers
	// 2. leader
	// 3. clients (TODO)
	leader, err := c.Leader()
	client := leader.GetClient()
	require.NoError(t, err)
	t.Log("Leader name:", leader.GetName())

	followers, err := c.Followers()
	require.NoError(t, err)
	t.Log("The number of followers", len(followers))

	for _, agent := range followers {
		t.Log("Upgrade follower", agent.GetName())

		config := agent.GetConfig()
		config.Version = targetVersion
		err = agent.Upgrade(context.Background(), config)
		if err != nil {
			return err
		}

		WaitForMembers(t, client, len(c.Agents))
		break
	}

	if len(followers) > 0 {
		client = followers[0].GetClient()
	}

	t.Log("Upgrade leader:", leader.GetName())
	config := leader.GetConfig()
	config.Version = targetVersion
	err = leader.Upgrade(context.Background(), config)
	if err != nil {
		return err
	}

	WaitForMembers(t, client, len(c.Agents))

	return nil
}

// Terminate will attempt to terminate all agents in the cluster and its network. If any agent
// termination fails, Terminate will abort and return an error.
func (c *Cluster) Terminate() error {
	for _, n := range c.Agents {
		err := n.Terminate()
		if err != nil {
			return err
		}
	}

	// Testcontainers seems to clean this the network.
	// Trigger it now will throw an error while the containers are still shutting down
	//if err := c.Network.Remove(context.Background()); err != nil {
	//	return errors.Wrapf(err, "could not terminate cluster network %s", c.ID)
	//}
	return nil
}

// Leader returns the cluster leader agent, or an error if no leader is
// available.
func (c *Cluster) Leader() (libagent.Agent, error) {
	if len(c.Agents) < 1 {
		return nil, fmt.Errorf("no agent available")
	}
	n0 := c.Agents[0]

	leaderAdd, err := getLeader(n0.GetClient())
	if err != nil {
		return nil, err
	}

	for _, n := range c.Agents {
		addr, _ := n.GetAddr()
		if strings.Contains(leaderAdd, addr) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("leader not found")
}

func getLeader(client *api.Client) (string, error) {
	leaderAdd, err := client.Status().Leader()
	if err != nil {
		return "", errors.Wrap(err, "could not query leader")
	}
	if leaderAdd == "" {
		return "", errors.New("no leader available")
	}
	return leaderAdd, nil
}

// Followers returns the cluster following servers.
func (c *Cluster) Followers() ([]libagent.Agent, error) {
	var followers []libagent.Agent

	leader, err := c.Leader()
	if err != nil {
		return nil, fmt.Errorf("could not determine leader: %w", err)
	}

	for _, n := range c.Agents {
		if n != leader && n.IsServer() {
			followers = append(followers, n)
		}
	}
	return followers, nil
}

// Servers returns the handle to server agents
func (c *Cluster) Servers() ([]libagent.Agent, error) {
	var servers []libagent.Agent

	for _, n := range c.Agents {
		if n.IsServer() {
			servers = append(servers, n)
		}
	}
	return servers, nil
}

// Clients returns the handle to client agents
func (c *Cluster) Clients() ([]libagent.Agent, error) {
	var clients []libagent.Agent

	for _, n := range c.Agents {
		if !n.IsServer() {
			clients = append(clients, n)
		}
	}
	return clients, nil
}

// PeerWithCluster establishes peering with the acceptor cluster
func (c *Cluster) PeerWithCluster(acceptingClient *api.Client, acceptingPeerName string, dialingPeerName string) error {
	node := c.Agents[0]
	dialingClient := node.GetClient()

	generateReq := api.PeeringGenerateTokenRequest{
		PeerName: acceptingPeerName,
	}
	generateRes, _, err := acceptingClient.Peerings().GenerateToken(context.Background(), generateReq, &api.WriteOptions{})
	if err != nil {
		return fmt.Errorf("error generate token: %v", err)
	}

	establishReq := api.PeeringEstablishRequest{
		PeerName:     dialingPeerName,
		PeeringToken: generateRes.PeeringToken,
	}
	_, _, err = dialingClient.Peerings().Establish(context.Background(), establishReq, &api.WriteOptions{})
	if err != nil {
		return fmt.Errorf("error establish peering: %v", err)
	}

	return nil
}

const retryTimeout = 90 * time.Second
const retryFrequency = 500 * time.Millisecond

func LongFailer() *retry.Timer {
	return &retry.Timer{Timeout: retryTimeout, Wait: retryFrequency}
}

func WaitForLeader(t *testing.T, cluster *Cluster, client *api.Client) {
	retry.RunWith(LongFailer(), t, func(r *retry.R) {
		leader, err := cluster.Leader()
		require.NoError(r, err)
		require.NotEmpty(r, leader)
	})

	if client != nil {
		waitForLeaderFromClient(t, client)
	}
}

func waitForLeaderFromClient(t *testing.T, client *api.Client) {
	retry.RunWith(LongFailer(), t, func(r *retry.R) {
		leader, err := getLeader(client)
		require.NoError(r, err)
		require.NotEmpty(r, leader)
	})
}

func WaitForMembers(t *testing.T, client *api.Client, expectN int) {
	retry.RunWith(LongFailer(), t, func(r *retry.R) {
		members, err := client.Agent().Members(false)
		var activeMembers int
		for _, member := range members {
			if serf.MemberStatus(member.Status) == serf.StatusAlive {
				activeMembers++
			}
		}
		require.NoError(r, err)
		require.Equal(r, expectN, activeMembers)
	})
}
