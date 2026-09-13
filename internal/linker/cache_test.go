package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written ElastiCache account.

func casesCache(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "cache-cases"))
}

// TestEveryCacheEndpointNamesItsCache: a primary, a configuration endpoint, a
// node's own endpoint, a memcached node list, a serverless host on its reader
// port — each connects to the one cache it belongs to.
func TestEveryCacheEndpointNamesItsCache(t *testing.T) {
	g := casesCache(t)
	for _, c := range []struct{ from, to, says string }{
		{"lambda/api-fn", "cache/sessions", "primary endpoint"},
		{"lambda/api-fn", "cache/serverless/ratelimit", "primary endpoint"},
		{"ecs/main/worker", "cache/carts", "configuration endpoint"},
		{"ecs/main/worker", "cache/cluster/memo", "node 0001"},
		{"ecs/main/worker", "cache/sessions", "node sessions-002"},
	} {
		e := mustEdge(t, g, c.from, c.to, KindConnect)
		var ok bool
		for _, ev := range e.Evidence {
			ok = ok || strings.Contains(ev.Detail, c.says)
		}
		if !ok {
			t.Errorf("%s → %s: no evidence naming its %s: %+v", c.from, c.to, c.says, e.Evidence)
		}
	}
	if m := mustEdge(t, g, "ecs/main/worker", "cache/cluster/memo", KindConnect); len(m.Evidence) != 2 {
		t.Errorf("both memcached nodes in the list should be evidence: %+v", m.Evidence)
	}
}

// TestACacheEndpointMatchesExactly: the same namesake trap as databases, in
// both endpoint shapes — sessions.other9.ng.0001… and ratelimit-zzz999.serverless…
// name this scan's caches with another account's suffix.
func TestACacheEndpointMatchesExactly(t *testing.T) {
	g := casesCache(t)
	for _, e := range g.Edges {
		if e.From == "ecs/main/api" {
			t.Errorf("an endpoint with another account's suffix was linked: %+v", e)
		}
	}
	for _, c := range []struct{ target, says string }{
		{"sessions.other9", "namesake"},
		{"ratelimit-zzz999", "namesake"},
		{"nothere.", "matches no cache"},
	} {
		var found bool
		for _, f := range g.Findings {
			found = found || (f.Kind == "unresolved" && strings.Contains(f.Target, c.target) && strings.Contains(f.Detail, c.says))
		}
		if !found {
			t.Errorf("%s: no finding saying %q", c.target, c.says)
		}
	}
}

// TestCacheIAMAuthAgreesWithConfig: elasticache:Connect on the replication
// group and the configuration naming it are two sources: high. A Connect on *
// reaches every cache and draws nothing.
func TestCacheIAMAuthAgreesWithConfig(t *testing.T) {
	g := casesCache(t)
	e := mustEdge(t, g, "lambda/api-fn", "cache/sessions", KindConnect)
	if e.Confidence != High || !contains(rulesOf(e), ruleIAM) {
		t.Errorf("config + elasticache:Connect: %s from %v", e.Confidence, rulesOf(e))
	}
	if !finding(g, "broad-access", "lambda/admin-fn", "elasticache") {
		t.Error("Connect on * was not reported as broad")
	}
	for _, e := range g.Edges {
		if e.From == "lambda/admin-fn" {
			t.Errorf("a broad grant drew an edge: %+v", e)
		}
	}
}

// TestCachesAreNotIAMGated: most caches take a password (or nothing) over a
// route; a role with no grant proves nothing, so no reference is unpermitted.
func TestCachesAreNotIAMGated(t *testing.T) {
	g := casesCache(t)
	mustEdge(t, g, "lambda/probe-fn", "cache/serverless/ratelimit", KindReferences) // setup: a reference, no grant
	for _, f := range g.Findings {
		if f.Kind == "unpermitted" {
			t.Errorf("a cache reference was called unpermitted: %+v", f)
		}
	}
	if f := flowOf(t, g, "cache/sessions"); f != Sync {
		t.Errorf("a cache the request path connects to: %s, want sync", f)
	}
}
