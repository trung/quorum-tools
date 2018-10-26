/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package docker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/core"

	"github.com/ethereum/go-ethereum/node"

	"github.com/jpmorganchase/quorum-tools/bootstrap"

	"github.com/ethereum/go-ethereum/log"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"

	"github.com/docker/docker/client"

	"gopkg.in/yaml.v2"
)

type Container interface {
	Name() string
	Start() error
	Stop() error
}

var CurrrentBuilder *QuorumBuilder

type QuorumBuilderConsensus struct {
	Name   string            `yaml:"name"`
	Config map[string]string `yaml:"config"`
}

func (qbc *QuorumBuilderConsensus) toGethArgs() (map[string]string, error) {
	consensusGethArgs := make(map[string]string)
	if a, ok := qbc.Config["geth_args"]; ok {
		args := strings.Split(strings.TrimSpace(a), " ")
		for _, arg := range args {
			parts := strings.Split(strings.TrimSpace(arg), "=")
			switch l := len(parts); l {
			case 1:
				consensusGethArgs[strings.TrimSpace(parts[0])] = ""
			case 2:
				consensusGethArgs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			default:
				return nil, fmt.Errorf("consensus config: invalid geth arg %s", arg)
			}
		}
	}
	return consensusGethArgs, nil
}

type QuorumBuilderNodeDocker struct {
	Image  string            `yaml:"image" json:"image"`
	Config map[string]string `yaml:"config" json:"config"`
}

type QuorumBuilderNode struct {
	Quorum    QuorumBuilderNodeDocker `yaml:"quorum" json:"quorum"`
	TxManager QuorumBuilderNodeDocker `yaml:"tx_manager" json:"tx_manager"`
}

type QuorumBuilder struct {
	Name      string                 `yaml:"name"`
	Genesis   string                 `yaml:"genesis"`
	Consensus QuorumBuilderConsensus `yaml:"consensus"`
	Nodes     []QuorumBuilderNode    `yaml:",flow"`

	commonLabels  map[string]string
	dockerClient  *client.Client
	dockerNetwork *Network
	pullMux       *sync.RWMutex
	tmpDir        string
}

func NewQuorumBuilder(r io.Reader) (*QuorumBuilder, error) {
	b := &QuorumBuilder{}
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, b); err != nil {
		return nil, err
	}
	b.dockerClient, err = client.NewEnvClient()
	if err != nil {
		return nil, err
	}
	b.commonLabels = map[string]string{
		"com.quorum.quorum-tools.id": b.Name,
	}
	b.pullMux = new(sync.RWMutex)
	return b, nil
}

// 1. Build Docker Network
// 2. Start Tx Manager
// 3. Start Quorum
func (qb *QuorumBuilder) Build(export string) (*QuorumNetwork, error) {
	if t, err := ioutil.TempDir("", ""); err != nil {
		return nil, err
	} else {
		qb.tmpDir = t
	}
	if err := qb.buildDockerNetwork(); err != nil {
		return nil, err
	}
	txManagers, err := qb.startTxManagers()
	if err != nil {
		return nil, err
	}
	nodes, genesis, err := qb.startQuorums(txManagers)
	if err != nil {
		return nil, err
	}
	qn := &QuorumNetwork{
		TxManagers:  txManagers,
		QuorumNodes: nodes,
		NodeCount:   len(nodes),
		Genesis:     genesis,
	}
	switch export {
	case "":
		// don't do anything
	case "-":
		// output to stdout
		qn.WriteNetworkConfigurationYAML(os.Stdout)
	default:
		// write a file
		f, err := os.Create(export)
		if err != nil {
			return nil, err
		}
		qn.WriteNetworkConfigurationYAML(f)
	}
	return qn, nil
}

func (qb *QuorumBuilder) startTxManagers() ([]TxManager, error) {
	nodeCount := len(qb.Nodes)
	log.Info("Start Tx Managers", "count", nodeCount)
	ips, err := qb.dockerNetwork.GetFreeIPAddrs(nodeCount)
	if err != nil {
		return nil, err
	}
	txManagers := make([]TxManager, nodeCount)
	if err := qb.startContainers(func(idx int, node QuorumBuilderNode) (Container, error) {
		myIP := ips[idx].String()
		txManagerContainer, err := qb.prepareTxManager(idx, nodeCount, node, myIP)
		if err != nil {
			return nil, err
		}
		txManagers[idx] = txManagerContainer.(TxManager)
		return txManagerContainer, nil
	}); err != nil {
		return nil, err
	}
	return txManagers, nil
}

func (qb *QuorumBuilder) startQuorums(txManagers []TxManager) ([]*Quorum, *core.Genesis, error) {
	nodeCount := len(qb.Nodes)
	log.Info("Start Quorum nodes", "count", nodeCount, "consensus", qb.Consensus.Name)
	quorums := make([]*Quorum, nodeCount)
	ips, err := qb.dockerNetwork.GetFreeIPAddrs(nodeCount)
	if err != nil {
		return nil, nil, err
	}
	bsNodes := make([]*bootstrap.Node, nodeCount)
	for i := 0; i < nodeCount; i++ {
		port, err := strconv.Atoi(node.DefaultConfig.P2P.ListenAddr[1:])
		if err != nil {
			return nil, nil, err
		}
		bsNodes[i], err = bootstrap.NewNode(qb.tmpDir, i, ips[i].String(), port)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := bootstrap.WritePermissionedNodes(bsNodes, defaultRaftPort); err != nil {
		return nil, nil, err
	}
	genesis, err := bootstrap.NewGenesis(bsNodes, qb.Consensus.Name, qb.Consensus.Config)
	if err != nil {
		return nil, nil, err
	}
	if err := qb.startContainers(func(idx int, meta QuorumBuilderNode) (Container, error) {
		q, err := qb.prepareQuorum(idx, nodeCount, meta, bsNodes[idx], txManagers[idx], genesis)
		if err != nil {
			return nil, err
		}
		quorums[idx] = q.(*Quorum)
		return q, nil
	}); err != nil {
		return nil, nil, err
	}
	return quorums, genesis, nil
}

func (qb *QuorumBuilder) startContainers(containerFn func(idx int, meta QuorumBuilderNode) (Container, error)) error {
	return doWorkInParallel("starting containers", quorumNodesToGeneric(qb.Nodes), func(idx int, el interface{}) error {
		node := el.(QuorumBuilderNode)
		c, err := containerFn(idx, node)
		if err != nil {
			return err
		}
		log.Info("Start Container", "name", c.Name())
		return c.Start()
	})
}

func (qb *QuorumBuilder) buildDockerNetwork() error {
	log.Info("Create Docker network", "name", qb.Name)
	network, err := NewDockerNetwork(qb.dockerClient, qb.Name, qb.commonLabels)
	if err != nil {
		return err
	}
	qb.dockerNetwork = network
	return nil
}

func (qb *QuorumBuilder) pullImage(image string) error {
	qb.pullMux.Lock()
	defer qb.pullMux.Unlock()
	log.Debug("Pull Docker Image", "name", image)
	filters := filters.NewArgs()
	filters.Add("reference", image)

	images, err := qb.dockerClient.ImageList(context.Background(), types.ImageListOptions{
		Filters: filters,
	})

	if len(images) == 0 || err != nil {
		_, err := qb.dockerClient.ImagePull(context.Background(), image, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("pullImage: %s - %s", image, err)
		}
	}
	return nil
}

func (qb *QuorumBuilder) Destroy(includeTmpDir bool) error {
	if includeTmpDir {
		log.Debug("removing temp directory")
		os.RemoveAll(qb.tmpDir)
	}

	filters := filters.NewArgs()
	for k, v := range qb.commonLabels {
		filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}
	// find all containers
	containers, err := qb.dockerClient.ContainerList(context.Background(), types.ContainerListOptions{Filters: filters, All: true})
	if err != nil {
		return fmt.Errorf("destroy: %s", err)
	}
	if err := doWorkInParallel("removing containers", containersToGeneric(containers), func(_ int, el interface{}) error {
		c := el.(types.Container)
		log.Info("removing container", "id", c.ID[:6], "name", c.Names)
		return qb.dockerClient.ContainerRemove(context.Background(), c.ID, types.ContainerRemoveOptions{Force: true})
	}); err != nil {
		return fmt.Errorf("destroy: %s", err)
	}

	// find networks
	networks, err := qb.dockerClient.NetworkList(context.Background(), types.NetworkListOptions{Filters: filters})
	if err != nil {
		return fmt.Errorf("destroy: %s", err)
	}
	if err := doWorkInParallel("removing network", networksToGeneric(networks), func(_ int, el interface{}) error {
		c := el.(types.NetworkResource)
		log.Info("removing network", "id", c.ID[:6], "name", c.Name)
		return qb.dockerClient.NetworkRemove(context.Background(), c.ID)
	}); err != nil {
		return fmt.Errorf("destroy: %s", err)
	}

	return nil
}
func (qb *QuorumBuilder) prepareTxManager(idx int, nodeCount int, meta QuorumBuilderNode, ip string) (Container, error) {
	if err := qb.pullImage(meta.TxManager.Image); err != nil {
		return nil, err
	}
	return NewTesseraTxManager(
		ConfigureTempDir(qb.tmpDir),
		ConfigureNodeCount(nodeCount),
		ConfigureMyIP(ip),
		ConfigureNodeIndex(idx),
		ConfigureProvisionId(qb.Name),
		ConfigureDockerClient(qb.dockerClient),
		ConfigureNetwork(qb.dockerNetwork),
		ConfigureDockerImage(meta.TxManager.Image),
		ConfigureConfig(meta.TxManager.Config),
		ConfigureLabels(qb.commonLabels),
	)
}
func (qb *QuorumBuilder) prepareQuorum(idx int, nodeCount int, meta QuorumBuilderNode, bs *bootstrap.Node, txManager TxManager, genesis *core.Genesis) (Container, error) {
	consensusGethArgs, err := qb.Consensus.toGethArgs()
	if err != nil {
		return nil, err
	}
	if err := qb.pullImage(meta.Quorum.Image); err != nil {
		return nil, err
	}
	return NewQuorum(
		ConfigureTempDir(qb.tmpDir),
		ConfigureTxManager(txManager),
		ConfigureBootstrapData(bs),
		ConfigureMyIP(bs.IP),
		ConfigureGenesis(genesis),
		ConfigureConsensusGethArgs(consensusGethArgs),
		ConfigureConsensusAlgorithm(qb.Consensus.Name),
		ConfigureNodeCount(nodeCount),
		ConfigureNodeIndex(idx),
		ConfigureProvisionId(qb.Name),
		ConfigureDockerClient(qb.dockerClient),
		ConfigureNetwork(qb.dockerNetwork),
		ConfigureDockerImage(meta.Quorum.Image),
		ConfigureConfig(meta.Quorum.Config),
		ConfigureLabels(qb.commonLabels),
	)

}

func quorumNodesToGeneric(n []QuorumBuilderNode) []interface{} {
	g := make([]interface{}, len(n))
	for i := range n {
		g[i] = n[i]
	}
	return g
}

func containersToGeneric(n []types.Container) []interface{} {
	g := make([]interface{}, len(n))
	for i := range n {
		g[i] = n[i]
	}
	return g
}

func networksToGeneric(n []types.NetworkResource) []interface{} {
	g := make([]interface{}, len(n))
	for i := range n {
		g[i] = n[i]
	}
	return g
}

func doWorkInParallel(title string, elements []interface{}, callback func(idx int, el interface{}) error) error {
	log.Debug(title)
	if len(elements) == 0 {
		return nil
	}
	doneChan := make(chan struct{})
	errChan := make(chan error)
	for idx, el := range elements {
		go func(_idx int, _el interface{}) {
			if err := callback(_idx, _el); err != nil {
				errChan <- err
			} else {
				doneChan <- struct{}{}
			}
		}(idx, el)
	}
	doneCount := 0
	allErr := make([]string, 0)
	for {
		select {
		case <-doneChan:
			doneCount++
		case err := <-errChan:
			allErr = append(allErr, err.Error())
		}
		if len(allErr)+doneCount >= len(elements) {
			break
		}
	}
	if len(allErr) > 0 {
		return fmt.Errorf("%s: %d/%d\n%s", title, doneCount, len(elements), strings.Join(allErr, "\n"))
	}
	return nil
}
