package discovery

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

func collectElastiCache(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "elasticache")
	out := &captureEmitter{}
	if err := (&ElastiCache{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

// TestElastiCacheReadsAllThreeAPIs: replication groups, standalone cache
// clusters and serverless caches each answer on their own API. The design
// named two; reading only those misses every serverless cache, silently.
func TestElastiCacheReadsAllThreeAPIs(t *testing.T) {
	out, _ := collectElastiCache(t)
	want := []string{"cache/carts", "cache/cluster/memo", "cache/serverless/ratelimit", "cache/sessions"}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids:\n got %q\nwant %q", got, want)
	}
	for _, id := range want {
		r, _ := out.byID(id)
		if r.Type != spec.TypeCache {
			t.Errorf("%s: type %q", id, r.Type)
		}
	}
}

// TestReplicationGroupIsTheNode: a replication group's member clusters are its
// nodes, not caches of their own. The group carries neither an engine version
// nor security groups; both come from its members.
func TestReplicationGroupIsTheNode(t *testing.T) {
	out, _ := collectElastiCache(t)
	s := rdsSpec[spec.Cache](t, out, "cache/sessions")
	if s.Kind != "replication-group" || s.EngineVersion != "8.0.1" || !s.TransitEncryption || !s.AuthToken || !s.MultiAZ {
		t.Errorf("group: %+v", s)
	}
	if !strings.HasPrefix(s.PrimaryEndpoint, "sessions.abc123.ng.0001.") || !strings.HasPrefix(s.ReaderEndpoint, "sessions-ro.") || s.Port != 6379 {
		t.Errorf("endpoints: %q %q %d", s.PrimaryEndpoint, s.ReaderEndpoint, s.Port)
	}
	if len(s.Nodes) != 2 || s.Nodes[0].Role != "primary" || s.Nodes[1].Role != "replica" || !strings.HasPrefix(s.Nodes[1].Endpoint, "sessions-002.") {
		t.Errorf("nodes: %+v", s.Nodes)
	}
	if s.Network == nil || s.Network.SubnetGroup != "orders-cache" || len(s.Network.SecurityGroups) != 1 {
		t.Errorf("network from members: %+v", s.Network)
	}
	c := rdsSpec[spec.Cache](t, out, "cache/carts")
	if !c.ClusterMode || !strings.HasPrefix(c.ConfigurationEndpoint, "clustercfg.carts.") || c.PrimaryEndpoint != "" {
		t.Errorf("a sharded group is reached through its configuration endpoint: %+v", c)
	}
}

// TestServerlessReaderSharesItsHost: a serverless cache's reader endpoint is
// the writer's host on another port — recorded as it is, since the linker must
// not expect a host to tell them apart.
func TestServerlessReaderSharesItsHost(t *testing.T) {
	out, _ := collectElastiCache(t)
	s := rdsSpec[spec.Cache](t, out, "cache/serverless/ratelimit")
	if s.Kind != "serverless" || s.PrimaryEndpoint == "" || s.PrimaryEndpoint != s.ReaderEndpoint || s.Port != 6379 || !s.TransitEncryption {
		t.Errorf("serverless: %+v", s)
	}
	if s.Limits == nil || s.Limits.MaxStorageGB != 5 || s.Limits.MaxECPUPerSecond != 5000 || len(s.Network.Subnets) != 2 {
		t.Errorf("limits and subnets: %+v %+v", s.Limits, s.Network)
	}
	m := rdsSpec[spec.Cache](t, out, "cache/cluster/memo")
	if m.Engine != "memcached" || !strings.HasPrefix(m.ConfigurationEndpoint, "memo.abc123.cfg.") || len(m.Nodes) != 2 || m.Nodes[0].ID != "0001" {
		t.Errorf("memcached: %+v", m)
	}
}

// TestNodeInfoIsRequested: DescribeCacheClusters returns no node endpoints
// unless asked. The fixture answers only a request carrying ShowCacheNodeInfo.
func TestNodeInfoIsRequested(t *testing.T) {
	_, tr := collectElastiCache(t)
	if n := tr.count("ElastiCache", "DescribeReplicationGroups"); n != 2 {
		t.Errorf("DescribeReplicationGroups called %d times, want 2 (one per page)", n)
	}
}

// TestTagsDenialCostsOnlyTags: tags are a convenience; a refused
// ListTagsForResource must not cost the cache.
func TestTagsDenialCostsOnlyTags(t *testing.T) {
	out, _ := collectElastiCache(t)
	c, ok := out.byID("cache/carts")
	if !ok || len(c.Tags) != 0 {
		t.Errorf("carts: kept=%v tags=%v", ok, c.Tags)
	}
	if s, _ := out.byID("cache/sessions"); s.Tags["team"] != "orders" {
		t.Errorf("sessions tags: %v", s.Tags)
	}
	if len(out.warnings) != 1 || out.warnings[0].Op != "ListTagsForResource" {
		t.Errorf("warnings: %+v", out.warnings)
	}
}

func TestElastiCacheUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectElastiCache(t)
	assertOpsMatchAllowList(t, "ElastiCache", tr)
}

func TestElastiCacheIDsKeepTheThreeApart(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:elasticache:us-east-1:123456789012:replicationgroup:x": "cache/x",
		"arn:aws:elasticache:us-east-1:123456789012:cluster:x":          "cache/cluster/x",
		"arn:aws:elasticache:us-east-1:123456789012:serverlesscache:x":  "cache/serverless/x",
		"arn:aws:elasticache:us-east-1:123456789012:user:x":             "",
	} {
		if got := resourceIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}

// TestAnUnsupportedAPICostsOnlyItsCaches: ElastiCache Serverless is not in every
// region, and emulators implement a subset. A round trip through Floci lost all
// three caches to one unsupported DescribeServerlessCaches; the other two APIs'
// caches must survive it, and the gap must be said.
func TestAnUnsupportedAPICostsOnlyItsCaches(t *testing.T) {
	tr := loadFixture(t, "orders", "elasticache")
	tr.exchanges = append([]exchange{{Op: "DescribeServerlessCaches", Status: 400, service: "elasticache",
		Body: `<ErrorResponse xmlns="http://elasticache.amazonaws.com/doc/2015-02-02/"><Error><Type>Sender</Type>` +
			`<Code>UnsupportedOperation</Code><Message>Operation DescribeServerlessCaches is not supported.</Message></Error></ErrorResponse>`}},
		tr.exchanges...)
	out := &captureEmitter{}
	if err := (&ElastiCache{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("an unsupported operation aborted the collector: %v", err)
	}
	if got := out.ids(); !reflect.DeepEqual(got, []string{"cache/carts", "cache/cluster/memo", "cache/sessions"}) {
		t.Errorf("caches from the other two APIs: %q", got)
	}
	var said bool
	for _, w := range out.warnings {
		said = said || (w.Kind == "unsupported" && w.Op == "DescribeServerlessCaches")
	}
	if !said {
		t.Errorf("the unsupported operation was not reported: %+v", out.warnings)
	}
}
