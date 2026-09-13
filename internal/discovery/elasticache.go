package discovery

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// ElastiCache collects caches from all three APIs that describe them.
//
// The design listed two: DescribeReplicationGroups (Valkey and Redis) and
// DescribeCacheClusters (memcached). Serverless caches answer only on a third,
// DescribeServerlessCaches — a collector reading the first two misses every one
// of them, silently, exactly as reading only DescribeCacheClusters misses every
// Redis replication group.
//
// A replication group is the node, like an Aurora cluster: its member cache
// clusters are recorded as its nodes. It carries neither an engine version nor
// security groups, so those come from its members, which DescribeCacheClusters
// lists beside the standalone ones.
//
// None of the three responses carries tags, so each cache costs one
// ListTagsForResource; a denial there costs the tags, never the cache.
type ElastiCache struct{}

func (*ElastiCache) Service() string { return "ElastiCache" }

func (c *ElastiCache) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := elasticache.NewFromConfig(s.Config())

	var groups []ectypes.ReplicationGroup
	rp := elasticache.NewDescribeReplicationGroupsPaginator(api, &elasticache.DescribeReplicationGroupsInput{})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "ElastiCache", "DescribeReplicationGroups", err); err != nil {
				return err
			}
			break
		}
		groups = append(groups, page.ReplicationGroups...)
	}

	// ShowCacheNodeInfo: without it the node endpoints are not returned.
	var clusters []ectypes.CacheCluster
	cp := elasticache.NewDescribeCacheClustersPaginator(api, &elasticache.DescribeCacheClustersInput{ShowCacheNodeInfo: aws.Bool(true)})
	for cp.HasMorePages() {
		page, err := cp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "ElastiCache", "DescribeCacheClusters", err); err != nil {
				return err
			}
			break
		}
		clusters = append(clusters, page.CacheClusters...)
	}

	var serverless []ectypes.ServerlessCache
	sp := elasticache.NewDescribeServerlessCachesPaginator(api, &elasticache.DescribeServerlessCachesInput{})
	for sp.HasMorePages() {
		page, err := sp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "ElastiCache", "DescribeServerlessCaches", err); err != nil {
				return err
			}
			break
		}
		serverless = append(serverless, page.ServerlessCaches...)
	}

	members := map[string][]ectypes.CacheCluster{}
	for _, cl := range clusters {
		if rg := aws.ToString(cl.ReplicationGroupId); rg != "" {
			members[rg] = append(members[rg], cl)
			continue
		}
		if err := c.emit(ctx, api, s, out, cacheCluster(cl)); err != nil {
			return err
		}
	}
	for _, g := range groups {
		if err := c.emit(ctx, api, s, out, replicationGroup(g, members[aws.ToString(g.ReplicationGroupId)])); err != nil {
			return err
		}
	}
	for _, sc := range serverless {
		if err := c.emit(ctx, api, s, out, serverlessCache(sc)); err != nil {
			return err
		}
	}
	return nil
}

type cacheResource struct {
	id, arn, api string
	spec         spec.Cache
	raw          any
}

func (c *ElastiCache) emit(ctx context.Context, api *elasticache.Client, s *awsx.Session, out Emitter, r cacheResource) error {
	var tags map[string]string
	t, err := api.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: aws.String(r.arn)})
	if err != nil {
		if err := warnOrFail(out, "ElastiCache", "ListTagsForResource", err); err != nil {
			return err
		}
	} else if len(t.TagList) > 0 {
		tags = map[string]string{}
		for _, tg := range t.TagList {
			tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
	}
	out.Emit(newResource(s, resourceArgs{
		ID: r.id, Type: spec.TypeCache, ARN: r.arn, Name: r.spec.Identifier,
		Tags: tags, API: r.api, Spec: r.spec, Raw: r.raw,
	}))
	return nil
}

func replicationGroup(g ectypes.ReplicationGroup, members []ectypes.CacheCluster) cacheResource {
	id := aws.ToString(g.ReplicationGroupId)
	sp := spec.Cache{
		Kind:              "replication-group",
		Identifier:        id,
		Engine:            aws.ToString(g.Engine),
		Status:            aws.ToString(g.Status),
		NodeType:          aws.ToString(g.CacheNodeType),
		ClusterMode:       aws.ToBool(g.ClusterEnabled),
		MultiAZ:           g.MultiAZ == ectypes.MultiAZStatusEnabled,
		TransitEncryption: aws.ToBool(g.TransitEncryptionEnabled),
		AuthToken:         aws.ToBool(g.AuthTokenEnabled),
		UserGroups:        append([]string(nil), g.UserGroupIds...),
	}
	if e := g.ConfigurationEndpoint; e != nil {
		sp.ConfigurationEndpoint, sp.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
	}
	roles := map[string]string{}
	for _, ng := range g.NodeGroups {
		// Without cluster mode there is one node group, and its primary and
		// reader endpoints are the group's.
		if e := ng.PrimaryEndpoint; e != nil && sp.PrimaryEndpoint == "" {
			sp.PrimaryEndpoint, sp.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
		}
		if e := ng.ReaderEndpoint; e != nil && sp.ReaderEndpoint == "" {
			sp.ReaderEndpoint = aws.ToString(e.Address)
		}
		for _, m := range ng.NodeGroupMembers {
			roles[aws.ToString(m.CacheClusterId)] = aws.ToString(m.CurrentRole)
		}
	}
	net := &spec.CacheNetwork{}
	for _, m := range members {
		if sp.EngineVersion == "" {
			sp.EngineVersion = aws.ToString(m.EngineVersion)
		}
		addClusterNetwork(net, m)
		for _, n := range m.CacheNodes {
			node := spec.CacheNode{ID: aws.ToString(m.CacheClusterId), Role: roles[aws.ToString(m.CacheClusterId)]}
			if e := n.Endpoint; e != nil {
				node.Endpoint = aws.ToString(e.Address)
			}
			sp.Nodes = append(sp.Nodes, node)
		}
	}
	// A member the cache cluster listing did not return is still a node.
	for _, mid := range g.MemberClusters {
		if !hasNode(sp.Nodes, mid) {
			sp.Nodes = append(sp.Nodes, spec.CacheNode{ID: mid, Role: roles[mid]})
		}
	}
	sortNodes(sp.Nodes)
	sp.Network = finishNetwork(net)
	return cacheResource{id: "cache/" + id, arn: aws.ToString(g.ARN), api: "elasticache:DescribeReplicationGroups",
		spec: sp, raw: map[string]any{"replicationGroup": g, "members": members}}
}

func cacheCluster(cl ectypes.CacheCluster) cacheResource {
	id := aws.ToString(cl.CacheClusterId)
	sp := spec.Cache{
		Kind:              "cache-cluster",
		Identifier:        id,
		Engine:            aws.ToString(cl.Engine),
		EngineVersion:     aws.ToString(cl.EngineVersion),
		Status:            aws.ToString(cl.CacheClusterStatus),
		NodeType:          aws.ToString(cl.CacheNodeType),
		TransitEncryption: aws.ToBool(cl.TransitEncryptionEnabled),
		AuthToken:         aws.ToBool(cl.AuthTokenEnabled),
	}
	if e := cl.ConfigurationEndpoint; e != nil {
		sp.ConfigurationEndpoint, sp.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
	}
	for _, n := range cl.CacheNodes {
		node := spec.CacheNode{ID: aws.ToString(n.CacheNodeId)}
		if e := n.Endpoint; e != nil {
			node.Endpoint = aws.ToString(e.Address)
			if sp.Port == 0 {
				sp.Port = aws.ToInt32(e.Port)
			}
		}
		sp.Nodes = append(sp.Nodes, node)
	}
	// A single Redis node outside a replication group is reached at its node
	// endpoint: that is its primary.
	if sp.ConfigurationEndpoint == "" && len(sp.Nodes) == 1 {
		sp.PrimaryEndpoint = sp.Nodes[0].Endpoint
	}
	sortNodes(sp.Nodes)
	net := &spec.CacheNetwork{}
	addClusterNetwork(net, cl)
	sp.Network = finishNetwork(net)
	return cacheResource{id: "cache/cluster/" + id, arn: aws.ToString(cl.ARN), api: "elasticache:DescribeCacheClusters", spec: sp, raw: cl}
}

func serverlessCache(sc ectypes.ServerlessCache) cacheResource {
	name := aws.ToString(sc.ServerlessCacheName)
	sp := spec.Cache{
		Kind:          "serverless",
		Identifier:    name,
		Engine:        aws.ToString(sc.Engine),
		EngineVersion: aws.ToString(sc.FullEngineVersion),
		Status:        aws.ToString(sc.Status),
		// Serverless caches always require TLS.
		TransitEncryption: true,
	}
	if g := aws.ToString(sc.UserGroupId); g != "" {
		sp.UserGroups = []string{g}
	}
	if e := sc.Endpoint; e != nil {
		sp.PrimaryEndpoint, sp.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
	}
	// The reader endpoint is the same host on another port (6380): recorded,
	// though a host alone cannot tell a reader from a writer here.
	if e := sc.ReaderEndpoint; e != nil {
		sp.ReaderEndpoint = aws.ToString(e.Address)
	}
	if l := sc.CacheUsageLimits; l != nil {
		sp.Limits = &spec.CacheLimits{}
		if l.DataStorage != nil {
			sp.Limits.MaxStorageGB = aws.ToInt32(l.DataStorage.Maximum)
		}
		if l.ECPUPerSecond != nil {
			sp.Limits.MaxECPUPerSecond = aws.ToInt32(l.ECPUPerSecond.Maximum)
		}
	}
	net := &spec.CacheNetwork{Subnets: append([]string(nil), sc.SubnetIds...), SecurityGroups: append([]string(nil), sc.SecurityGroupIds...)}
	sp.Network = finishNetwork(net)
	return cacheResource{id: "cache/serverless/" + name, arn: aws.ToString(sc.ARN), api: "elasticache:DescribeServerlessCaches", spec: sp, raw: sc}
}

func addClusterNetwork(n *spec.CacheNetwork, cl ectypes.CacheCluster) {
	if g := aws.ToString(cl.CacheSubnetGroupName); g != "" {
		n.SubnetGroup = g
	}
	for _, sg := range cl.SecurityGroups {
		if id := aws.ToString(sg.SecurityGroupId); id != "" && !slices.Contains(n.SecurityGroups, id) {
			n.SecurityGroups = append(n.SecurityGroups, id)
		}
	}
}

func finishNetwork(n *spec.CacheNetwork) *spec.CacheNetwork {
	sort.Strings(n.Subnets)
	sort.Strings(n.SecurityGroups)
	if n.SubnetGroup == "" && len(n.Subnets) == 0 && len(n.SecurityGroups) == 0 {
		return nil
	}
	return n
}

func hasNode(nodes []spec.CacheNode, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func sortNodes(nodes []spec.CacheNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].ID != nodes[j].ID {
			return nodes[i].ID < nodes[j].ID
		}
		return strings.Compare(nodes[i].Endpoint, nodes[j].Endpoint) < 0
	})
}
