package linker

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written Tier-3 account. As with the other
// hand-written accounts, the golden file pins the output and these say why.

func cases3(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "tier3-cases"))
}

func mustEdge(t *testing.T, g *Graph, from, to string, kind Kind) Edge {
	t.Helper()
	e, ok := edge(g, from, to, kind)
	if !ok {
		t.Fatalf("no %s edge %s → %s", kind, from, to)
	}
	return e
}

// TestAPermissionIsNotAUse: a role granting GetItem on a table the workload's
// configuration never names is medium — granted, not shown to be used. When the
// configuration names the table too, two independent sources agree: high.
func TestAPermissionIsNotAUse(t *testing.T) {
	g := cases3(t)
	ledger := mustEdge(t, g, "lambda/api-fn", "ddb/ledger", KindRead)
	if ledger.Confidence != Medium {
		t.Errorf("permission alone: %s, want medium", ledger.Confidence)
	}
	// Evidence is what a user audits: GetItem on an index is not an action,
	// so a statement granting it there is evidence for Query only.
	for _, ev := range ledger.Evidence {
		if strings.Contains(ev.Detail, "/index/") && strings.Contains(ev.Detail, "GetItem") {
			t.Errorf("evidence claims GetItem on an index: %s", ev.Detail)
		}
	}
	e := mustEdge(t, g, "lambda/api-fn", "ddb/orders", KindWrite)
	if e.Confidence != High || strings.Join(rulesOf(e), ",") != "config.value-scan,iam.policy-resource" {
		t.Errorf("configuration and permission agreeing: %s from %v, want high from both", e.Confidence, rulesOf(e))
	}
	if _, ok := edge(g, "lambda/api-fn", "ddb/orders", KindReferences); ok {
		t.Error("the reference survived beside the write that states its intent")
	}
}

// TestIntentComesFromNamedActions: dynamodb:* on a table permits reading and
// writing and says which neither. Drawing both would be picking; sqs:* would
// invent a consumer.
func TestIntentComesFromNamedActions(t *testing.T) {
	g := cases3(t)
	mustEdge(t, g, "lambda/api-fn", "ddb/profiles", KindReferences)
	for _, k := range []Kind{KindRead, KindWrite} {
		if _, ok := edge(g, "lambda/api-fn", "ddb/profiles", k); ok {
			t.Errorf("dynamodb:* was read as %s", k)
		}
	}
}

// TestPatternsReachingSeveralAreCandidates: jobs-* matches two queues; that does
// not say the function sends to both. A pattern matching one names it.
func TestPatternsReachingSeveralAreCandidates(t *testing.T) {
	g := cases3(t)
	for _, q := range []string{"sqs/jobs-a", "sqs/jobs-b"} {
		if e := mustEdge(t, g, "lambda/api-fn", q, KindPublish); e.Confidence != Low {
			t.Errorf("%s: %s, want low", q, e.Confidence)
		}
		if f := flowOf(t, g, q); f != Unreached {
			t.Errorf("%s reached only through a low candidate: %s", q, f)
		}
	}
	if e := mustEdge(t, g, "lambda/api-fn", "sqs/single-q", KindPublish); e.Confidence != Medium {
		t.Errorf("single-* matches one queue: %s, want medium", e.Confidence)
	}
}

// TestAPermissionNeverChangesState: the role behind a disabled mapping can still
// receive from the queue. Merging by "most active status" would have re-enabled
// the edge — the real validation account has exactly this mapping.
func TestAPermissionNeverChangesState(t *testing.T) {
	g := cases3(t)
	e := mustEdge(t, g, "sqs/events", "lambda/poller-fn", KindConsume)
	if e.Status != Disabled || !contains(rulesOf(e), ruleIAM) {
		t.Errorf("disabled mapping with a receive grant: status %q, rules %v", e.Status, rulesOf(e))
	}
}

// TestLambdaReceivesThroughMappings: Lambda polls for its event source mappings
// with the function's role. A function that may receive from a queue no mapping
// connects it to is most likely holding a leftover grant.
func TestLambdaReceivesThroughMappings(t *testing.T) {
	g := cases3(t)
	if e := mustEdge(t, g, "sqs/work", "lambda/poller-fn", KindConsume); e.Confidence != Low {
		t.Errorf("receive grant without a mapping: %s, want low", e.Confidence)
	}
}

// TestDenyAndBoundaryCancel: an unconditional Deny and a boundary that does not
// allow the action cancel the grant, and the finding explains the missing edge.
// A conditional Deny may not apply, so it cancels nothing.
func TestDenyAndBoundaryCancel(t *testing.T) {
	g := cases3(t)
	if _, ok := edge(g, "lambda/api-fn", "ddb/audit", KindWrite); ok {
		t.Error("a write an explicit Deny cancels became an edge")
	}
	if !finding(g, "blocked", "lambda/api-fn", "ddb/audit") {
		t.Error("the cancelled grant was not reported")
	}
	if _, ok := edge(g, "ecs/main/gated", "ddb/orders", KindWrite); ok {
		t.Error("a write the permissions boundary does not allow became an edge")
	}
	if !finding(g, "blocked", "ecs/main/gated", "ddb/orders") {
		t.Error("the grant the boundary trims was not reported")
	}
	mustEdge(t, g, "ecs/main/gated", "sqs/events", KindPublish) // what the boundary allows survives
	mustEdge(t, g, "lambda/api-fn", "sqs/events", KindPublish)  // a conditional Deny cancels nothing
	if finding(g, "blocked", "lambda/api-fn", "ddb/profiles") {
		t.Error("dynamodb:* minus a Deny on deletes is a guardrail, not a blocked grant")
	}
}

// TestBroadGrantsDrawNothing: a grant reaching every table — Resource "*" in an
// AWS managed policy, or table/* in an inline one — would link the workload to
// the whole account.
func TestBroadGrantsDrawNothing(t *testing.T) {
	g := cases3(t)
	for _, holder := range []string{"ecs/main/worker", "lambda/poller-fn"} {
		if !finding(g, "broad-access", holder, "dynamodb") {
			t.Errorf("%s: broad DynamoDB access not reported", holder)
		}
		for _, e := range g.Edges {
			if e.From == holder && strings.HasPrefix(e.To, "ddb/") && contains(rulesOf(e), ruleIAM) {
				t.Errorf("a broad grant drew %s → %s", e.From, e.To)
			}
		}
	}
	if finding(g, "unpermitted", "ecs/main/worker", "ddb/ledger") {
		t.Error("a table a broad grant reaches was called unpermitted")
	}
}

// TestAWorkerThatOnlyReceivesNamesItsQueueToPollIt is Q17: the reference folds
// into the consume edge IAM backs, instead of pointing the other way beside it.
func TestAWorkerThatOnlyReceivesNamesItsQueueToPollIt(t *testing.T) {
	g := cases3(t)
	e := mustEdge(t, g, "sqs/work", "ecs/main/worker", KindConsume)
	if e.Confidence != High || !contains(rulesOf(e), "config.value-scan") {
		t.Errorf("work → worker: %s from %v", e.Confidence, rulesOf(e))
	}
	if _, ok := edge(g, "ecs/main/worker", "sqs/work", KindReferences); ok {
		t.Error("the worker's QUEUE_URL still points the other way")
	}
}

// TestUnpermittedSettlesTheAmbiguity: TABLE_NAME=orders names a table and a
// queue; the role writes the table and cannot touch the queue. Only a complete
// reading of the role, with no resource policy naming it, supports saying so.
func TestUnpermittedSettlesTheAmbiguity(t *testing.T) {
	g := cases3(t)
	if !finding(g, "unpermitted", "lambda/api-fn", "sqs/orders") {
		t.Error("the queue the role cannot touch was not reported")
	}
	if finding(g, "unpermitted", "lambda/api-fn", "sqs/reports") {
		t.Error("a queue whose policy names the role was called unpermitted")
	}
	if finding(g, "unpermitted", "ecs/main/partial", "sqs/events") {
		t.Error("an absence was concluded from a partly read role")
	}
	if !finding(g, "unscanned", "ecs/main/partial", "iam/role/partial-task") {
		t.Error("the partly read role was not reported")
	}
	if e := mustEdge(t, g, "ecs/main/blind", "sqs/events", KindPublish); e.Confidence != Low {
		t.Errorf("a role whose boundary could not be read: %s, want low", e.Confidence)
	}
	if !finding(g, "unscanned", "lambda/orphan-fn", "iam/role/gone-role") {
		t.Error("a workload whose role is missing was not reported")
	}
}

// TestPoliciesNamingOutsideResourcesAreReported: the namesake trap in a policy.
// A grant on jobs-a in another account must not become evidence for the local
// jobs-a, and a grant on a queue that no longer exists is worth knowing.
func TestPoliciesNamingOutsideResourcesAreReported(t *testing.T) {
	g := cases3(t)
	if !finding(g, "unresolved", "lambda/api-fn", ":999999999999:jobs-a") {
		t.Error("a grant on a queue in another account was not reported")
	}
	if !finding(g, "unresolved", "lambda/api-fn", ":deleted-q") {
		t.Error("a grant on a queue outside the inventory was not reported")
	}
	e := mustEdge(t, g, "lambda/api-fn", "sqs/jobs-a", KindPublish)
	if len(e.Evidence) != 1 || !strings.Contains(e.Evidence[0].Detail, "jobs-*") {
		t.Errorf("jobs-a evidence: %+v", e.Evidence)
	}
	for _, f := range g.Findings {
		if strings.Contains(f.Detail, "logs:") {
			t.Errorf("an infrastructure grant was reported: %+v", f)
		}
	}
}

// TestAnAPIsRoleOnlyCorroborates: an API's behaviour is its routes, read in full
// by Tier 1. The role it assumes confirms them and draws nothing new.
func TestAnAPIsRoleOnlyCorroborates(t *testing.T) {
	g := cases3(t)
	e := mustEdge(t, g, "apigw/api0000020", "sqs/inbox", KindPublish)
	if !contains(rulesOf(e), ruleIAM) {
		t.Error("the integration's role did not corroborate it")
	}
	for _, e := range g.Edges {
		if e.From == "apigw/api0000020" && e.To == "ddb/ledger" {
			t.Errorf("an API gained an edge from its role alone: %+v", e)
		}
	}
}

func TestWildMatch(t *testing.T) {
	for _, c := range []struct {
		p, s string
		fold bool
		want bool
	}{
		{"sqs:*", "sqs:SendMessage", false, true},
		{"sqs:Send*", "sqs:ReceiveMessage", false, false},
		{"SQS:sendmessage", "sqs:SendMessage", true, true},
		{"SQS:sendmessage", "sqs:SendMessage", false, false},
		{"arn:aws:sqs:*:*:jobs-?", "arn:aws:sqs:us-east-1:1:jobs-a", false, true},
		{"arn:aws:sqs:*:*:jobs-?", "arn:aws:sqs:us-east-1:1:jobs-ab", false, false},
		{"*", "", false, true},
		{"a*b*c", "aXbYbZc", false, true},
		{"a*b*c", "aXbYbZ", false, false},
	} {
		if got := wildMatch(c.p, c.s, c.fold); got != c.want {
			t.Errorf("wildMatch(%q, %q, %v) = %v", c.p, c.s, c.fold, got)
		}
	}
}

func TestStatementShapes(t *testing.T) {
	one, err := parseStatements(json.RawMessage(`{"Statement":{"Effect":"Allow","Action":"sqs:SendMessage","Resource":"*"}}`), "s", "l")
	if err != nil || len(one) != 1 || one[0].Action[0] != "sqs:SendMessage" {
		t.Errorf("single statement object: %+v %v", one, err)
	}
	not, _ := parseStatements(json.RawMessage(`{"Statement":[{"Effect":"Allow","NotAction":["iam:*"],"Resource":"*"}]}`), "s", "l")
	if _, ok := not[0].action("sqs:SendMessage"); !ok {
		t.Error("NotAction iam:* does not cover sqs:SendMessage")
	}
	if _, ok := not[0].action("iam:PassRole"); ok {
		t.Error("NotAction iam:* covers iam:PassRole")
	}
	if _, err := parseStatements(json.RawMessage(`{"Statement":[{"Effect":"Maybe"}]}`), "s", "l"); err == nil {
		t.Error("an unknown effect was accepted")
	}
	for entry, want := range map[string]string{
		"*": "*", "arn:aws:dynamodb:*:*:table/*": "*", "arn:aws:dynamodb:us-east-1:1:table/orders/index/*": "orders",
		"arn:aws:lambda:us-east-1:1:function:fn:*": "fn", "arn:aws:sqs:us-east-1:1:jobs-*": "jobs-*",
	} {
		if got := nameSegment(entry); got != want {
			t.Errorf("nameSegment(%s) = %s, want %s", entry, got, want)
		}
	}
}
