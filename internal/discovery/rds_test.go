package discovery

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

func collectRDS(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "rds")
	out := &captureEmitter{}
	if err := (&RDS{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func rdsSpec[T any](t *testing.T, out *captureEmitter, id string) T {
	t.Helper()
	r, ok := out.byID(id)
	if !ok {
		t.Fatalf("no resource %s; have %v", id, out.ids())
	}
	var s T
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRDSClusterIsTheNode: applications connect to a cluster's endpoints, so
// the cluster is the resource and its instances are members. Emitting the
// members as instances would draw one Aurora database as three.
func TestRDSClusterIsTheNode(t *testing.T) {
	out, _ := collectRDS(t)
	want := []string{"rds/cluster/orders-db", "rds/legacy-db", "rds/legacy-db-replica", "rds/reports-db"}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids:\n got %q\nwant %q", got, want)
	}
	c := rdsSpec[spec.RDSCluster](t, out, "rds/cluster/orders-db")
	if len(c.Members) != 2 || c.Members[1].Identifier != "orders-db-writer" || !c.Members[1].Writer ||
		!strings.HasPrefix(c.Members[1].Endpoint, "orders-db-writer.abc123.") {
		t.Errorf("members: %+v", c.Members)
	}
	if !strings.HasPrefix(c.Endpoint, "orders-db.cluster-abc123.") || !strings.HasPrefix(c.ReaderEndpoint, "orders-db.cluster-ro-") ||
		len(c.CustomEndpoints) != 1 {
		t.Errorf("endpoints: %q %q %q", c.Endpoint, c.ReaderEndpoint, c.CustomEndpoints)
	}
	if c.Network == nil || c.Network.VpcID != "vpc-0fixture0000000a" || len(c.Network.SecurityGroups) != 1 {
		t.Errorf("a cluster's VPC comes from its members' subnet group, its groups from itself: %+v", c.Network)
	}
}

// TestRDSSkipsOtherServices: DocumentDB and Neptune answer on the RDS API. A
// DocumentDB cluster recorded as a relational database would be wrong in the
// graph and, later, in the local environment.
func TestRDSSkipsOtherServices(t *testing.T) {
	out, _ := collectRDS(t)
	for _, id := range out.ids() {
		if strings.Contains(id, "docs-db") {
			t.Errorf("a DocumentDB resource was collected as RDS: %s", id)
		}
	}
	if len(out.warnings) != 1 || out.warnings[0].Kind != "out-of-scope" || !strings.Contains(out.warnings[0].Message, "DocumentDB") {
		t.Errorf("the skipped cluster was not reported once: %+v", out.warnings)
	}
}

// TestRDSRecordsWhatLinksToIt: the endpoint (Tier 2), the managed secret's ARN
// (a workload holding it connects), and the resource id IAM authentication
// names — and never a password, which the API does not return anyway.
func TestRDSRecordsWhatLinksToIt(t *testing.T) {
	out, _ := collectRDS(t)
	c := rdsSpec[spec.RDSCluster](t, out, "rds/cluster/orders-db")
	if !strings.Contains(c.MasterSecretARN, ":secret:rds!cluster-") || !c.DataAPI || !c.IAMAuth ||
		c.Serverless == nil || c.Serverless.MaxACU != 4 || c.DBName != "orders" {
		t.Errorf("cluster: %+v", c)
	}
	i := rdsSpec[spec.RDSInstance](t, out, "rds/legacy-db")
	if i.Endpoint != "legacy-db.abc123.us-east-1.rds.amazonaws.com" || i.Port != 5432 ||
		!strings.Contains(i.MasterSecretARN, ":secret:rds!db-") || i.ResourceID == "" || len(i.Network.Subnets) != 2 {
		t.Errorf("instance: %+v", i)
	}
	if r := rdsSpec[spec.RDSInstance](t, out, "rds/legacy-db-replica"); r.ReplicaOf != "legacy-db" {
		t.Errorf("replica source: %q", r.ReplicaOf)
	}
	if n := rdsSpec[spec.RDSInstance](t, out, "rds/reports-db"); n.Endpoint != "" || n.Status != "creating" {
		t.Errorf("an instance being created: %+v", n)
	}
}

// TestRDSPaginates: both listings arrive in two pages, and the second only
// answers a request carrying the first page's marker.
func TestRDSPaginates(t *testing.T) {
	_, tr := collectRDS(t)
	for _, op := range []string{"DescribeDBClusters", "DescribeDBInstances"} {
		if n := tr.count("RDS", op); n != 2 {
			t.Errorf("%s called %d times, want 2", op, n)
		}
	}
}

func TestRDSUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectRDS(t)
	assertOpsMatchAllowList(t, "RDS", tr)
}

// TestRDSIDsKeepInstancesAndClustersApart: RDS keeps the two identifiers in
// separate namespaces, so a cluster and an instance may share a name.
func TestRDSIDsKeepInstancesAndClustersApart(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:rds:us-east-1:123456789012:db:orders":      "rds/orders",
		"arn:aws:rds:us-east-1:123456789012:cluster:orders": "rds/cluster/orders",
		"arn:aws:rds:us-east-1:123456789012:snapshot:x":     "",
	} {
		if got := resourceIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}
