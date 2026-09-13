package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written account. The golden file pins the exact
// output; these say why each behaviour matters, so a golden diff cannot be
// approved without reading what it breaks.

func cases(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "tier1-cases"))
}

func edge(g *Graph, from, to string, kind Kind) (Edge, bool) {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return e, true
		}
	}
	return Edge{}, false
}

func flowOf(t *testing.T, g *Graph, id string) Flow {
	t.Helper()
	n, ok := g.Node(id)
	if !ok {
		t.Fatalf("no node %s", id)
	}
	return n.Flow
}

func finding(g *Graph, kind, node, targetPart string) bool {
	for _, f := range g.Findings {
		if f.Kind == kind && f.Node == node && strings.Contains(f.Target, targetPart) {
			return true
		}
	}
	return false
}

// TestEveryEdgeHasEvidence is the design's first rule: an edge without evidence
// is a bug.
func TestEveryEdgeHasEvidence(t *testing.T) {
	for _, dir := range []string{"orders", "tier1-cases"} {
		g := linkFixture(t, filepath.Join("testdata", dir))
		for _, e := range g.Edges {
			if len(e.Evidence) == 0 {
				t.Errorf("%s: %s → %s (%s) has no evidence", dir, e.From, e.To, e.Kind)
			}
			for _, ev := range e.Evidence {
				if ev.Rule == "" || ev.Source == "" || ev.Detail == "" {
					t.Errorf("%s: incomplete evidence on %s → %s: %+v", dir, e.From, e.To, ev)
				}
			}
		}
	}
}

// TestCrossAccountNamesakeIsNotLinked: sqs/jobs' dead-letter queue is in another
// account and shares its name with a local queue. Ids come from names, so an
// unchecked rule would draw jobs → orders-dlq — wrong, silent, and plausible.
func TestCrossAccountNamesakeIsNotLinked(t *testing.T) {
	g := cases(t)
	if _, ok := edge(g, "sqs/jobs", "sqs/orders-dlq", KindPublish); ok {
		t.Fatal("linked a queue to a same-named queue in another account")
	}
	if !finding(g, "unresolved", "sqs/jobs", ":999999999999:orders-dlq") {
		t.Error("the foreign dead-letter queue was not reported")
	}
	if _, ok := edge(g, "apigw/api0000001", "lambda/items-fn", KindInvoke); !ok {
		t.Fatal("setup: GET /items → items-fn edge missing")
	}
	if !finding(g, "unresolved", "apigw/api0000001", "us-west-2") {
		t.Error("a function in another region was not reported as outside the scan")
	}
}

// TestResourcePolicyCorroboratesButNeverCreates: a resource policy is a
// permission, not a use.
func TestResourcePolicyCorroboratesButNeverCreates(t *testing.T) {
	g := cases(t)
	e, _ := edge(g, "apigw/api0000001", "lambda/items-fn", KindInvoke)
	var corroborated bool
	for _, ev := range e.Evidence {
		corroborated = corroborated || ev.Rule == "lambda.resource-policy"
	}
	if !corroborated {
		t.Error("the resource policy did not add evidence to an edge it confirms")
	}
	if _, ok := edge(g, "apigw/api0000001", "lambda/stale-fn", KindInvoke); ok {
		t.Error("a permission no route uses created an edge")
	}
	if !finding(g, "stale-permission", "lambda/stale-fn", "apigw/api0000001") {
		t.Error("the unused permission was not reported")
	}
	if flowOf(t, g, "lambda/s3-fn") != Entrypoint {
		t.Error("a function S3 may invoke is not an entrypoint")
	}
}

// TestSyncTakesPrecedence: shared-fn is on the request path (API → shared-fn)
// and also consumes the events queue. Classification must not depend on which
// path a traversal happens to find first.
func TestSyncTakesPrecedence(t *testing.T) {
	g := cases(t)
	if f := flowOf(t, g, "lambda/shared-fn"); f != Sync {
		t.Errorf("shared-fn: got %s, want sync", f)
	}
	if f := flowOf(t, g, "lambda/events-consumer"); f != Async {
		t.Errorf("events-consumer: got %s, want async — it is only reached across a queue", f)
	}
	if f := flowOf(t, g, "sqs/consumer-dlq"); f != Async {
		t.Errorf("consumer-dlq: got %s, want async", f)
	}
	if f := flowOf(t, g, "lambda/auth-fn"); f != Sync {
		t.Errorf("an authorizer is in the request path: got %s, want sync", f)
	}
}

// TestMappingStatusDrivesFlow: a disabled mapping is recorded and carries
// nothing; an unsettled one is kept and followed.
func TestMappingStatusDrivesFlow(t *testing.T) {
	g := cases(t)
	d, ok := edge(g, "sqs/events", "lambda/disabled-consumer", KindConsume)
	if !ok || d.Status != Disabled {
		t.Errorf("disabled mapping: %+v", d)
	}
	if f := flowOf(t, g, "lambda/disabled-consumer"); f != Unreached {
		t.Errorf("reached only through a disabled mapping: got %s, want unreached", f)
	}
	u, ok := edge(g, "sqs/events", "lambda/unsettled-consumer", KindConsume)
	if !ok || u.Status != Unsettled {
		t.Errorf("unsettled mapping: %+v", u)
	}
	if f := flowOf(t, g, "lambda/unsettled-consumer"); f != Async {
		t.Errorf("an unsettled edge must still be followed: got %s", f)
	}
}

// TestTemplatedIntegrationResolvesPerStage: prod defines the variable, dev does
// not, and a second route names a variable no stage defines.
func TestTemplatedIntegrationResolvesPerStage(t *testing.T) {
	g := cases(t)
	e, ok := edge(g, "apigw/api0000001", "lambda/worker-fn", KindInvoke)
	if !ok || len(e.Evidence) != 1 || !strings.Contains(e.Evidence[0].Detail, "stage prod") {
		t.Errorf("templated route not resolved through stage prod only: %+v", e)
	}
	if !finding(g, "unresolved", "apigw/api0000001", "stageVariables.missing") {
		t.Error("a variable no stage defines was not reported")
	}
}

// TestNotEveryURLIsAThirdParty: an HTTP integration through a VPC link targets
// an internal load balancer. Making it ext/ would have the gateway mock a piece
// of the user's own system.
func TestNotEveryURLIsAThirdParty(t *testing.T) {
	g := cases(t)
	if _, ok := g.Node("ext/internal-nlb.example.internal"); ok {
		t.Error("a VPC-linked backend became an external node")
	}
	if n, ok := g.Node("ext/partner.example.com"); !ok || !n.External {
		t.Error("the partner API is not an external node")
	}
	if !finding(g, "unresolved", "apigw/api0000001", "internal-nlb") {
		t.Error("the VPC link target was not reported")
	}
}

// TestUnusedThingsProduceNothing: an authorizer guarding no route, a mock, and
// a service with no collector behind it.
func TestUnusedThingsProduceNothing(t *testing.T) {
	g := cases(t)
	if _, ok := edge(g, "apigw/api0000001", "lambda/unused-auth-fn", KindInvoke); ok {
		t.Error("an authorizer that guards no route produced an edge")
	}
	if len(g.Between("apigw/api0000002", "")) != 0 {
		t.Error("unexpected edges")
	}
	for _, e := range g.Edges {
		if e.From == "apigw/api0000002" {
			t.Errorf("a mock-only API has an edge: %+v", e)
		}
	}
	if flowOf(t, g, "apigw/api0000002") != Entrypoint {
		t.Error("an API is an entrypoint even when every route is a mock")
	}
	if !finding(g, "unresolved", "lambda/kinesis-fn", "kinesis") {
		t.Error("a Kinesis source (no collector) was not reported")
	}
	for _, id := range []string{"sqs/idle", "ddb/unused", "sqs/orders-dlq", "lambda/unused-auth-fn"} {
		if f := flowOf(t, g, id); f != Unreached {
			t.Errorf("%s: got %s, want unreached", id, f)
		}
	}
}

func TestLoadBalancedServiceIsAnEntrypoint(t *testing.T) {
	g := cases(t)
	n, _ := g.Node("ecs/main/web")
	if n.Flow != Entrypoint || len(n.Triggers) == 0 || !strings.Contains(n.Triggers[0], "load balancer") {
		t.Errorf("load-balanced service: %+v", n)
	}
}

func TestExternalID(t *testing.T) {
	for in, want := range map[string]string{
		"https://API.Payments.example.com/v1?x=1": "ext/api.payments.example.com",
		"https://api.example.com:443/":            "ext/api.example.com",
		"http://api.example.com:8080/":            "ext/api.example.com:8080",
		"arn:aws:lambda:us-east-1:1:function:x":   "",
		"not a url":                               "",
	} {
		if got := ExternalID(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
