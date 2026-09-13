package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written RDS account.

func casesRDS(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "rds-cases"))
}

// TestEveryEndpointNamesItsDatabase: a cluster is reached through its writer,
// its reader, a custom endpoint, or a member's own endpoint — all one node. A
// database endpoint is only good for connecting, so the edge says connect.
func TestEveryEndpointNamesItsDatabase(t *testing.T) {
	g := casesRDS(t)
	e := mustEdge(t, g, "lambda/api-fn", "rds/cluster/orders-db", KindConnect)
	if !strings.Contains(e.Evidence[0].Detail, "reader endpoint") {
		t.Errorf("reader endpoint: %+v", e.Evidence)
	}
	w := mustEdge(t, g, "ecs/main/worker", "rds/cluster/orders-db", KindConnect)
	var member, custom bool
	for _, ev := range w.Evidence {
		member = member || strings.Contains(ev.Detail, "member orders-db-2")
		custom = custom || strings.Contains(ev.Detail, "custom endpoint")
	}
	if !member || !custom {
		t.Errorf("a member's and a custom endpoint must both name the cluster: %+v", w.Evidence)
	}
	if f := flowOf(t, g, "rds/cluster/orders-db"); f != Sync {
		t.Errorf("a database the request path connects to: %s, want sync", f)
	}
}

// TestAnEndpointMatchesExactlyOrNotAtAll: the suffix in an RDS endpoint belongs
// to the account. orders-db.cluster-other9… is another account's orders-db;
// matching on the name would link a service to a database it cannot reach.
func TestAnEndpointMatchesExactlyOrNotAtAll(t *testing.T) {
	g := casesRDS(t)
	if _, ok := edge(g, "ecs/main/api", "rds/cluster/orders-db", KindConnect); ok {
		t.Error("an endpoint with another account's suffix was linked by its name")
	}
	// Each case says why, not only that: the reason is what the user acts on.
	for _, c := range []struct{ node, target, says string }{
		{"ecs/main/api", "cluster-other9", "namesake"},
		{"ecs/main/worker", "proxy-fix001", "RDS Proxy"},
		{"ecs/main/worker", "eu-west-1", "region eu-west-1"},
	} {
		var found bool
		for _, f := range g.Findings {
			found = found || (f.Kind == "unresolved" && f.Node == c.node && strings.Contains(f.Target, c.target) && strings.Contains(f.Detail, c.says))
		}
		if !found {
			t.Errorf("%s: no unresolved finding saying %q", c.target, c.says)
		}
	}
}

// TestTheMasterSecretIsALink: a workload handed a database's password — as an
// env var holding the secret's ARN, injected through secrets[] (with a JSON key
// suffix), or read through IAM — connects to that database. Other secrets say
// nothing yet.
func TestTheMasterSecretIsALink(t *testing.T) {
	g := casesRDS(t)
	e := mustEdge(t, g, "ecs/main/api", "rds/legacy-db", KindConnect)
	if !strings.Contains(e.Evidence[0].Source, "secrets DB_PASSWORD") {
		t.Errorf("secrets[] injection: %+v", e.Evidence)
	}
	for _, f := range g.Findings {
		if strings.Contains(f.Target, "prod/api/key") {
			t.Errorf("a secret that is no database's was reported: %+v", f)
		}
	}
	if r := mustEdge(t, g, "lambda/report-fn", "rds/legacy-db", KindConnect); r.Confidence != Medium {
		t.Errorf("GetSecretValue on the master secret alone: %s, want medium", r.Confidence)
	}
}

// TestConfigAndDataAPIAgree: the configuration names the cluster and the role
// may run SQL on it through the Data API — two independent sources, high.
func TestConfigAndDataAPIAgree(t *testing.T) {
	g := casesRDS(t)
	e := mustEdge(t, g, "lambda/api-fn", "rds/cluster/orders-db", KindConnect)
	if e.Confidence != High || !contains(rulesOf(e), ruleIAM) {
		t.Errorf("config + rds-data:ExecuteStatement: %s from %v", e.Confidence, rulesOf(e))
	}
}

// TestDatabasesAreNotIAMGated: a role with no grant on a database proves
// nothing — it connects with a password over a route. A reference to one is
// never unpermitted. And a GetSecretValue on * is broad access to secrets,
// not to databases.
func TestDatabasesAreNotIAMGated(t *testing.T) {
	g := casesRDS(t)
	mustEdge(t, g, "ecs/main/worker", "rds/legacy-db", KindReferences)
	for _, f := range g.Findings {
		if f.Kind == "unpermitted" {
			t.Errorf("a database reference was called unpermitted: %+v", f)
		}
	}
	if !finding(g, "broad-access", "lambda/report-fn", "secretsmanager") || finding(g, "broad-access", "lambda/report-fn", "rds") {
		t.Error("broad access must be named by the grant's service (secretsmanager), not the target's")
	}
}

// TestNamesAreNotIdentifiers: DB_NAME=orders and CLUSTER=orders-db draw
// nothing. A database name repeats everywhere ("postgres"), and identifiers are
// not how applications reach a database.
func TestNamesAreNotIdentifiers(t *testing.T) {
	g := casesRDS(t)
	e := mustEdge(t, g, "lambda/api-fn", "rds/cluster/orders-db", KindConnect)
	for _, ev := range e.Evidence {
		if strings.Contains(ev.Source, "DB_NAME") || strings.Contains(ev.Source, "CLUSTER") {
			t.Errorf("a name was read as a link: %+v", ev)
		}
	}
}
