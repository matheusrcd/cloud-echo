package linker

import (
	"fmt"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// dbIndex maps what configuration uses to reach a database — an endpoint, or
// the master secret RDS manages for it — to the database's node.
//
// Endpoints match exactly or not at all. An RDS endpoint is
// <name>.<suffix>.<region>.rds.amazonaws.com, and the suffix belongs to the
// account and region: ce-test-db.cluster-abc123… is not this account's
// ce-test-db, whatever its name says. Matching on the name would be the
// namesake trap, in a host.
type dbIndex struct {
	hosts   map[string]dbHost
	secrets map[string]string // master secret ARN, without a key/stage suffix → node
	names   map[string]string // identifier (cluster, instance, member) → node, to explain a near miss
}

type dbHost struct{ id, what string }

func newDBIndex(c *Context) *dbIndex {
	d := &dbIndex{hosts: map[string]dbHost{}, secrets: map[string]string{}, names: map[string]string{}}
	host := func(h, id, what string) {
		if h != "" {
			d.hosts[strings.ToLower(h)] = dbHost{id, what}
		}
	}
	Each(c, spec.TypeRDSCluster, func(r inventory.Resource, cl *spec.RDSCluster) {
		if !c.HasNode(r.ID) {
			return
		}
		host(cl.Endpoint, r.ID, "writer endpoint")
		host(cl.ReaderEndpoint, r.ID, "reader endpoint")
		for _, e := range cl.CustomEndpoints {
			host(e, r.ID, "custom endpoint")
		}
		d.names[cl.Identifier] = r.ID
		for _, m := range cl.Members {
			host(m.Endpoint, r.ID, "member "+m.Identifier)
			d.names[m.Identifier] = r.ID
		}
		if cl.MasterSecretARN != "" {
			d.secrets[secretBase(cl.MasterSecretARN)] = r.ID
		}
	})
	Each(c, spec.TypeRDSInstance, func(r inventory.Resource, in *spec.RDSInstance) {
		if !c.HasNode(r.ID) {
			return
		}
		host(in.Endpoint, r.ID, "endpoint")
		d.names[in.Identifier] = r.ID
		if in.MasterSecretARN != "" {
			d.secrets[secretBase(in.MasterSecretARN)] = r.ID
		}
	})
	return d
}

// secretBase drops what may follow a secret's ARN where it is consumed — ECS
// accepts arn…:secret:name-AbCdEf:password:: to inject one JSON key.
func secretBase(arn string) string {
	p := strings.SplitN(arn, ":", 8)
	if len(p) < 7 || p[2] != "secretsmanager" {
		return arn
	}
	return strings.Join(p[:7], ":")
}

// rdsHost links an RDS hostname to its database, or says why it cannot.
// Connecting is the only thing an endpoint is for, so the edge is connect —
// and it is not subject to IAM: a database takes a password and a route.
func (s *scanner) rdsHost(holder string, v configValue, host string) {
	if d, ok := s.db.hosts[host]; ok {
		if d.id != holder {
			s.c.Edge(holder, d.id, KindConnect, High, Active, v.Source,
				fmt.Sprintf("%s connects to %s (its %s)", v.Label, d.id, d.what))
		}
		return
	}
	labels := strings.Split(strings.TrimSuffix(host, ".rds.amazonaws.com"), ".")
	region := labels[len(labels)-1]
	report := func(what string) { s.c.Unresolved(holder, host, fmt.Sprintf("%s names %s", v.Label, what)) }
	switch {
	case len(labels) >= 3 && strings.HasPrefix(labels[1], "proxy-"):
		report("an RDS Proxy endpoint; resolving it needs rds:DescribeDBProxies, which is not collected")
	case region != s.c.inv.Region:
		report(fmt.Sprintf("an RDS endpoint in region %s, outside this scan", region))
	case s.db.names[labels[0]] != "":
		report(fmt.Sprintf("an endpoint called %s, like %s in this scan, with another account's suffix: a namesake, not this database",
			labels[0], s.db.names[labels[0]]))
	default:
		report("an RDS endpoint that matches no database in the inventory (deleted, or in another account)")
	}
}

// dbSecret links a value holding a database's managed master secret.
func (s *scanner) dbSecret(holder string, v configValue, arn string) bool {
	id, ok := s.db.secrets[secretBase(arn)]
	if !ok {
		return false
	}
	if id != holder {
		s.c.Edge(holder, id, KindConnect, High, Active, v.Source,
			fmt.Sprintf("%s holds the master secret of %s", v.Label, id))
	}
	return true
}
