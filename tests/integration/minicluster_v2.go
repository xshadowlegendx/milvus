// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"context"
	"fmt"
	"net"
	"path"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	grpcdatacoord "github.com/milvus-io/milvus/internal/distributed/datacoord"
	grpcdatacoordclient "github.com/milvus-io/milvus/internal/distributed/datacoord/client"
	grpcdatanode "github.com/milvus-io/milvus/internal/distributed/datanode"
	grpcdatanodeclient "github.com/milvus-io/milvus/internal/distributed/datanode/client"
	grpcindexnode "github.com/milvus-io/milvus/internal/distributed/indexnode"
	grpcindexnodeclient "github.com/milvus-io/milvus/internal/distributed/indexnode/client"
	grpcproxy "github.com/milvus-io/milvus/internal/distributed/proxy"
	grpcproxyclient "github.com/milvus-io/milvus/internal/distributed/proxy/client"
	grpcquerycoord "github.com/milvus-io/milvus/internal/distributed/querycoord"
	grpcquerycoordclient "github.com/milvus-io/milvus/internal/distributed/querycoord/client"
	grpcquerynode "github.com/milvus-io/milvus/internal/distributed/querynode"
	grpcquerynodeclient "github.com/milvus-io/milvus/internal/distributed/querynode/client"
	grpcrootcoord "github.com/milvus-io/milvus/internal/distributed/rootcoord"
	grpcrootcoordclient "github.com/milvus-io/milvus/internal/distributed/rootcoord/client"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/dependency"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/etcd"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
)

var params *paramtable.ComponentParam = paramtable.Get()

type ClusterConfig struct {
	// ProxyNum int
	// todo coord num can be more than 1 if enable Active-Standby
	// RootCoordNum int
	// DataCoordNum int
	// IndexCoordNum int
	// QueryCoordNum int
	QueryNodeNum int
	DataNodeNum  int
	IndexNodeNum int
}

func DefaultParams() map[string]string {
	testPath := fmt.Sprintf("integration-test-%d", time.Now().Unix())
	return map[string]string{
		params.EtcdCfg.RootPath.Key:  testPath,
		params.MinioCfg.RootPath.Key: testPath,
		//"runtime.role": typeutil.StandaloneRole,
		//params.IntegrationTestCfg.IntegrationMode.Key: "true",
		params.LocalStorageCfg.Path.Key:              path.Join("/tmp", testPath),
		params.CommonCfg.StorageType.Key:             "local",
		params.DataNodeCfg.MemoryForceSyncEnable.Key: "false", // local execution will print too many logs
		params.CommonCfg.GracefulStopTimeout.Key:     "30",
	}
}

func DefaultClusterConfig() ClusterConfig {
	return ClusterConfig{
		QueryNodeNum: 1,
		DataNodeNum:  1,
		IndexNodeNum: 1,
	}
}

type MiniClusterV2 struct {
	ctx context.Context

	mu sync.RWMutex

	params        map[string]string
	clusterConfig ClusterConfig

	factory      dependency.Factory
	ChunkManager storage.ChunkManager

	EtcdCli *clientv3.Client

	Proxy      *grpcproxy.Server
	DataCoord  *grpcdatacoord.Server
	RootCoord  *grpcrootcoord.Server
	QueryCoord *grpcquerycoord.Server

	DataCoordClient  types.DataCoordClient
	RootCoordClient  types.RootCoordClient
	QueryCoordClient types.QueryCoordClient

	ProxyClient     types.ProxyClient
	DataNodeClient  types.DataNodeClient
	QueryNodeClient types.QueryNodeClient
	IndexNodeClient types.IndexNodeClient

	DataNode  *grpcdatanode.Server
	QueryNode *grpcquerynode.Server
	IndexNode *grpcindexnode.Server

	MetaWatcher MetaWatcher
	ptmu        sync.Mutex
	querynodes  []*grpcquerynode.Server
	qnid        atomic.Int64
	datanodes   []*grpcdatanode.Server
	dnid        atomic.Int64
}

type OptionV2 func(cluster *MiniClusterV2)

func StartMiniClusterV2(ctx context.Context, opts ...OptionV2) (*MiniClusterV2, error) {
	cluster := &MiniClusterV2{
		ctx:  ctx,
		qnid: *atomic.NewInt64(10000),
		dnid: *atomic.NewInt64(20000),
	}
	paramtable.Init()
	cluster.params = DefaultParams()
	cluster.clusterConfig = DefaultClusterConfig()
	for _, opt := range opts {
		opt(cluster)
	}
	for k, v := range cluster.params {
		params.Save(k, v)
	}
	// setup etcd client
	etcdConfig := &paramtable.Get().EtcdCfg
	etcdCli, err := etcd.GetEtcdClient(
		etcdConfig.UseEmbedEtcd.GetAsBool(),
		etcdConfig.EtcdUseSSL.GetAsBool(),
		etcdConfig.Endpoints.GetAsStrings(),
		etcdConfig.EtcdTLSCert.GetValue(),
		etcdConfig.EtcdTLSKey.GetValue(),
		etcdConfig.EtcdTLSCACert.GetValue(),
		etcdConfig.EtcdTLSMinVersion.GetValue())
	if err != nil {
		return nil, err
	}
	cluster.EtcdCli = etcdCli

	cluster.MetaWatcher = &EtcdMetaWatcher{
		rootPath: etcdConfig.RootPath.GetValue(),
		etcdCli:  cluster.EtcdCli,
	}

	ports, err := GetAvailablePorts(7)
	if err != nil {
		return nil, err
	}
	log.Info("minicluster ports", zap.Ints("ports", ports))
	params.Save(params.RootCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[0]))
	params.Save(params.DataCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[1]))
	params.Save(params.QueryCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[2]))
	params.Save(params.DataNodeGrpcServerCfg.Port.Key, fmt.Sprint(ports[3]))
	params.Save(params.QueryNodeGrpcServerCfg.Port.Key, fmt.Sprint(ports[4]))
	params.Save(params.IndexNodeGrpcServerCfg.Port.Key, fmt.Sprint(ports[5]))
	params.Save(params.ProxyGrpcServerCfg.Port.Key, fmt.Sprint(ports[6]))

	// setup clients
	cluster.RootCoordClient, err = grpcrootcoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	cluster.DataCoordClient, err = grpcdatacoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	cluster.QueryCoordClient, err = grpcquerycoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	cluster.ProxyClient, err = grpcproxyclient.NewClient(ctx, paramtable.Get().ProxyGrpcClientCfg.GetInternalAddress(), 0)
	if err != nil {
		return nil, err
	}
	cluster.DataNodeClient, err = grpcdatanodeclient.NewClient(ctx, paramtable.Get().DataNodeGrpcClientCfg.GetAddress(), 0)
	if err != nil {
		return nil, err
	}
	cluster.QueryNodeClient, err = grpcquerynodeclient.NewClient(ctx, paramtable.Get().QueryNodeGrpcClientCfg.GetAddress(), 0)
	if err != nil {
		return nil, err
	}
	cluster.IndexNodeClient, err = grpcindexnodeclient.NewClient(ctx, paramtable.Get().IndexNodeGrpcClientCfg.GetAddress(), 0, false)
	if err != nil {
		return nil, err
	}

	// setup servers
	cluster.factory = dependency.MockDefaultFactory(true, params)
	chunkManager, err := cluster.factory.NewPersistentStorageChunkManager(cluster.ctx)
	if err != nil {
		return nil, err
	}
	cluster.ChunkManager = chunkManager

	cluster.RootCoord, err = grpcrootcoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.DataCoord = grpcdatacoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.QueryCoord, err = grpcquerycoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.Proxy, err = grpcproxy.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.DataNode, err = grpcdatanode.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.QueryNode, err = grpcquerynode.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.IndexNode, err = grpcindexnode.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	return cluster, nil
}

func (cluster *MiniClusterV2) AddQueryNode() *grpcquerynode.Server {
	cluster.ptmu.Lock()
	defer cluster.ptmu.Unlock()
	cluster.qnid.Inc()
	id := cluster.qnid.Load()
	oid := paramtable.GetNodeID()
	log.Info(fmt.Sprintf("adding extra querynode with id:%d", id))
	paramtable.SetNodeID(id)
	node, err := grpcquerynode.NewServer(context.TODO(), cluster.factory)
	if err != nil {
		return nil
	}
	err = node.Run()
	if err != nil {
		return nil
	}
	paramtable.SetNodeID(oid)

	req := &milvuspb.GetComponentStatesRequest{}
	resp, err := node.GetComponentStates(context.TODO(), req)
	if err != nil {
		return nil
	}
	log.Info(fmt.Sprintf("querynode %d ComponentStates:%v", id, resp))
	cluster.querynodes = append(cluster.querynodes, node)
	return node
}

func (cluster *MiniClusterV2) AddDataNode() *grpcdatanode.Server {
	cluster.ptmu.Lock()
	defer cluster.ptmu.Unlock()
	cluster.qnid.Inc()
	id := cluster.qnid.Load()
	oid := paramtable.GetNodeID()
	log.Info(fmt.Sprintf("adding extra datanode with id:%d", id))
	paramtable.SetNodeID(id)
	node, err := grpcdatanode.NewServer(context.TODO(), cluster.factory)
	if err != nil {
		return nil
	}
	err = node.Run()
	if err != nil {
		return nil
	}
	paramtable.SetNodeID(oid)

	req := &milvuspb.GetComponentStatesRequest{}
	resp, err := node.GetComponentStates(context.TODO(), req)
	if err != nil {
		return nil
	}
	log.Info(fmt.Sprintf("datanode %d ComponentStates:%v", id, resp))
	cluster.datanodes = append(cluster.datanodes, node)
	return node
}

func (cluster *MiniClusterV2) Start() error {
	log.Info("mini cluster start")
	err := cluster.RootCoord.Run()
	if err != nil {
		return err
	}

	err = cluster.DataCoord.Run()
	if err != nil {
		return err
	}

	err = cluster.QueryCoord.Run()
	if err != nil {
		return err
	}

	err = cluster.DataNode.Run()
	if err != nil {
		return err
	}

	err = cluster.QueryNode.Run()
	if err != nil {
		return err
	}

	err = cluster.IndexNode.Run()
	if err != nil {
		return err
	}

	err = cluster.Proxy.Run()
	if err != nil {
		return err
	}

	ctx2, cancel := context.WithTimeout(context.Background(), time.Second*120)
	defer cancel()
	healthy := false
	for !healthy {
		checkHealthResp, _ := cluster.Proxy.CheckHealth(ctx2, &milvuspb.CheckHealthRequest{})
		healthy = checkHealthResp.IsHealthy
		time.Sleep(time.Second * 1)
	}
	if !healthy {
		return errors.New("minicluster is not healthy after 120s")
	}
	log.Info("minicluster started")
	return nil
}

func (cluster *MiniClusterV2) Stop() error {
	log.Info("mini cluster stop")
	cluster.RootCoord.Stop()
	log.Info("mini cluster rootCoord stopped")
	cluster.DataCoord.Stop()
	log.Info("mini cluster dataCoord stopped")
	cluster.QueryCoord.Stop()
	log.Info("mini cluster queryCoord stopped")
	cluster.Proxy.Stop()
	log.Info("mini cluster proxy stopped")

	cluster.StopAllDataNodes()
	cluster.StopAllQueryNodes()
	cluster.IndexNode.Stop()
	log.Info("mini cluster indexNode stopped")

	cluster.EtcdCli.KV.Delete(cluster.ctx, params.EtcdCfg.RootPath.GetValue(), clientv3.WithPrefix())
	defer cluster.EtcdCli.Close()

	if cluster.ChunkManager == nil {
		chunkManager, err := cluster.factory.NewPersistentStorageChunkManager(cluster.ctx)
		if err != nil {
			log.Warn("fail to create chunk manager to clean test data", zap.Error(err))
		} else {
			cluster.ChunkManager = chunkManager
		}
	}
	cluster.ChunkManager.RemoveWithPrefix(cluster.ctx, cluster.ChunkManager.RootPath())
	return nil
}

func (cluster *MiniClusterV2) StopAllQueryNodes() {
	cluster.QueryNode.Stop()
	log.Info("mini cluster main queryNode stopped")
	numExtraQN := len(cluster.querynodes)
	for _, node := range cluster.querynodes {
		node.Stop()
	}
	log.Info(fmt.Sprintf("mini cluster stoped %d extra querynode", numExtraQN))
}

func (cluster *MiniClusterV2) StopAllDataNodes() {
	cluster.DataNode.Stop()
	log.Info("mini cluster main dataNode stopped")
	numExtraQN := len(cluster.datanodes)
	for _, node := range cluster.datanodes {
		node.Stop()
	}
	log.Info(fmt.Sprintf("mini cluster stoped %d extra datanode", numExtraQN))
}

func (cluster *MiniClusterV2) GetContext() context.Context {
	return cluster.ctx
}

func (cluster *MiniClusterV2) GetFactory() dependency.Factory {
	return cluster.factory
}

func GetAvailablePorts(n int) ([]int, error) {
	ports := make([]int, n)
	for i := range ports {
		port, err := GetAvailablePort()
		if err != nil {
			return nil, err
		}
		ports[i] = port
	}
	return ports, nil
}

func GetAvailablePort() (int, error) {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
