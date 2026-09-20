// Package registry 读 etcd 里的集群拓扑，是 manager「看」的一侧。
//
// 数据源（口径与 clover-server-engine/internal/app 的登记侧一致）：
//
//	clover/nodes/<nodeID>          = {"type":"game","tags":["room"],"admin":"127.0.0.1:8041"}
//	clover/services/<role>/<实例>  = "127.0.0.1:8011"
//
// 前缀常量在此**复制**而非 import：引擎侧那两个常量定义在 internal 包里，
// 外部工具按 Go 规则无法引用。引擎若改前缀，这里必须同步。
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcd 键前缀（与 clover-server-engine/internal/app/node_registry.go、discovery.go 对应）。
const (
	nodeKeyRoot    = "clover/nodes/"
	serviceKeyRoot = "clover/services/"
)

// Node 节点目录中的一条记录。
type Node struct {
	ID    string   `json:"id"`              // etcd 键后缀：引擎侧登记的 g.Addr()，通常是逻辑服地址
	Type  string   `json:"type"`            // 节点类型：game / battle
	Tags  []string `json:"tags,omitempty"`  // 业务标签
	Admin string   `json:"admin,omitempty"` // admin 控制面地址（老版本节点可能为空）
}

// Instance 服务发现中的一条实例记录。
type Instance struct {
	Role string `json:"role"` // logic / auth / log
	Key  string `json:"key"`  // 实例唯一键：<role>-<host>-<pid>
	Addr string `json:"addr"` // 可调用地址
}

// Registry etcd 只读视图。
type Registry struct {
	ec *clientv3.Client
}

// New 连接 etcd。endpoints 为空直接报错——没有 etcd 就没有拓扑可读。
func New(endpoints []string, username, password string, dialTimeout time.Duration) (*Registry, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("registry: etcd endpoints required")
	}
	cfg := clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	}
	if username != "" {
		cfg.Username = username
		cfg.Password = password
	}
	ec, err := clientv3.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("registry: connect etcd %v: %w", endpoints, err)
	}
	return &Registry{ec: ec}, nil
}

// Close 关闭 etcd 连接。
func (r *Registry) Close() error {
	if r == nil || r.ec == nil {
		return nil
	}
	return r.ec.Close()
}

// Nodes 拉取节点目录全量（按节点 ID 升序）。
func (r *Registry) Nodes(ctx context.Context) ([]Node, error) {
	resp, err := r.ec.Get(ctx, nodeKeyRoot, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("registry: list %s: %w", nodeKeyRoot, err)
	}
	out := make([]Node, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		id := strings.TrimPrefix(string(kv.Key), nodeKeyRoot)
		if id == "" {
			continue
		}
		// 单条坏值跳过，不让一个损坏的节点记录拖垮整张列表
		//（与引擎侧 nodeDirectory.refresh 同口径）。
		var meta struct {
			Type  string   `json:"type"`
			Tags  []string `json:"tags"`
			Admin string   `json:"admin"`
		}
		if err := json.Unmarshal(kv.Value, &meta); err != nil {
			continue
		}
		out = append(out, Node{ID: id, Type: meta.Type, Tags: meta.Tags, Admin: meta.Admin})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Instances 拉取全部服务实例（按角色、地址升序）。
func (r *Registry) Instances(ctx context.Context) ([]Instance, error) {
	resp, err := r.ec.Get(ctx, serviceKeyRoot, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("registry: list %s: %w", serviceKeyRoot, err)
	}
	out := make([]Instance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		rest := strings.TrimPrefix(string(kv.Key), serviceKeyRoot)
		role, key, ok := strings.Cut(rest, "/")
		if !ok || role == "" {
			continue
		}
		out = append(out, Instance{Role: role, Key: key, Addr: string(kv.Value)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Addr < out[j].Addr
	})
	return out, nil
}

// FindNode 按节点 ID 查找单条节点记录。
func (r *Registry) FindNode(ctx context.Context, id string) (Node, error) {
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return Node{}, err
	}
	for _, n := range nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("registry: node %q not found (%d node(s) registered)", id, len(nodes))
}
