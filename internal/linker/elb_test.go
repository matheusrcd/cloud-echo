package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written load balancer account.

func casesELB(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "elb-cases"))
}

// TestALoadBalancerIsTheEntrypoint: an internet-facing load balancer takes the
// traffic; the services behind it are reached through it — not entrypoints of
// their own, as they were when the load balancer could not be seen.
func TestALoadBalancerIsTheEntrypoint(t *testing.T) {
	g := casesELB(t)
	if f := flowOf(t, g, "elb/web"); f != Entrypoint {
		t.Errorf("internet-facing: %s, want entrypoint", f)
	}
	for _, id := range []string{"ecs/main/api", "ecs/main/canary", "lambda/fn-a"} {
		if f := flowOf(t, g, id); f != Sync {
			t.Errorf("%s behind it: %s, want sync", id, f)
		}
		if n, _ := g.Node(id); len(n.Triggers) > 0 {
			t.Errorf("%s is still an entrypoint of its own: %v", id, n.Triggers)
		}
	}
	mustEdge(t, g, "elb/web", "ecs/main/api", KindHTTP)
	mustEdge(t, g, "elb/web", "lambda/fn-a", KindInvoke)
}

// TestRulesSayWhereTrafficGoes: every rule's target group is followed — a
// weighted forward to both groups, with the weight in the evidence.
func TestRulesSayWhereTrafficGoes(t *testing.T) {
	g := casesELB(t)
	e := mustEdge(t, g, "elb/web", "ecs/main/canary", KindHTTP)
	if !strings.Contains(e.Evidence[0].Detail, "/canary/*") || !strings.Contains(e.Evidence[0].Detail, "weight 10") {
		t.Errorf("weighted rule: %+v", e.Evidence)
	}
	if api := mustEdge(t, g, "elb/web", "ecs/main/api", KindHTTP); len(api.Evidence) != 2 {
		t.Errorf("two rules reach api (/api/* and the canary's 90%%): %+v", api.Evidence)
	}
}

// TestAnInternalLoadBalancerIsReachedByItsCallers: internal is not an
// entrypoint — its callers are. Here a function on the request path names it,
// so it and its service are sync; an internal NLB nobody names stays unreached.
func TestAnInternalLoadBalancerIsReachedByItsCallers(t *testing.T) {
	g := casesELB(t)
	for id, want := range map[string]Flow{"elb/internal-api": Sync, "ecs/main/orders": Sync, "elb/front": Unreached} {
		if f := flowOf(t, g, id); f != want {
			t.Errorf("%s: %s, want %s", id, f, want)
		}
	}
	mustEdge(t, g, "elb/front", "elb/web", KindHTTP) // an NLB whose target is an ALB
}

// TestADNSNameMatchesExactly: ALB and NLB shapes, a Route 53 alias
// (dualstack.), and a namesake — web's name with another address — reported,
// never linked. An NLB is reached with connect, an ALB with http.
func TestADNSNameMatchesExactly(t *testing.T) {
	g := casesELB(t)
	mustEdge(t, g, "lambda/caller-fn", "elb/internal-api", KindHTTP)
	mustEdge(t, g, "lambda/caller-fn", "elb/tcp", KindConnect)
	if e := mustEdge(t, g, "lambda/caller-fn", "elb/web", KindHTTP); len(e.Evidence) != 1 || !strings.Contains(e.Evidence[0].Source, "DS") {
		t.Errorf("only the dualstack alias links web; the namesake must not: %+v", e.Evidence)
	}
	var said bool
	for _, f := range g.Findings {
		said = said || (f.Kind == "unresolved" && strings.Contains(f.Target, "web-999") && strings.Contains(f.Detail, "namesake"))
	}
	if !said {
		t.Error("the namesake DNS name was not reported")
	}
}

// TestVPCLinksReachTheirLoadBalancer: an HTTP API's VPC link names a listener,
// a REST API's an NLB's DNS name; both are the load balancer, never a third
// party the gateway would mock. A listener the load balancer no longer has is
// reported: its ARN still names the load balancer, but nothing answers.
func TestVPCLinksReachTheirLoadBalancer(t *testing.T) {
	g := casesELB(t)
	if e := mustEdge(t, g, "apigw/api0000050", "elb/internal-api", KindHTTP); len(e.Evidence) != 1 {
		t.Errorf("only the listener it has links it: %+v", e.Evidence)
	}
	if !finding(g, "unresolved", "apigw/api0000050", "l443") {
		t.Error("a VPC link to a deleted listener was not reported")
	}
	mustEdge(t, g, "apigw/api0000051", "elb/tcp", KindHTTP)
	for _, n := range g.Nodes {
		if n.External {
			t.Errorf("a VPC-linked load balancer became a third party: %s", n.ID)
		}
	}
}

// TestWhatTheLoadBalancerCannotReachIsSaid: a target group no load balancer
// forwards to, ip targets no ECS service registers, a target group outside the
// inventory (the one case left for the old trigger), and a Lambda permission
// for a target group the function is not in.
func TestWhatTheLoadBalancerCannotReachIsSaid(t *testing.T) {
	g := casesELB(t)
	if !finding(g, "unresolved", "ecs/main/idle", "targetgroup/idle") || flowOf(t, g, "ecs/main/idle") != Unreached {
		t.Error("a service in a target group nothing forwards to must be reported and unreached")
	}
	if !finding(g, "unresolved", "elb/tcp", "targetgroup/db-proxy") {
		t.Error("ip targets no ECS service registers were not reported")
	}
	if n, _ := g.Node("ecs/main/legacy"); len(n.Triggers) == 0 || !strings.Contains(n.Triggers[0], "not in the inventory") {
		t.Errorf("a target group outside the inventory keeps the declaration's trigger: %+v", n)
	}
	if !finding(g, "stale-permission", "lambda/stale-fn", "elb/web") {
		t.Error("a load balancer permission for a function not registered was not reported")
	}
	e := mustEdge(t, g, "elb/web", "lambda/fn-a", KindInvoke)
	if !contains(rulesOf(e), "lambda.resource-policy") {
		t.Error("the registered function's permission did not corroborate the edge")
	}
}
