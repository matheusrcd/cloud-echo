package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written Tier-2 account. As with tier1-cases, the
// golden file pins the output and these say why each behaviour matters.

func cases2(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "tier2-cases"))
}

func rulesOf(e Edge) []string {
	var out []string
	for _, ev := range e.Evidence {
		out = append(out, ev.Rule)
	}
	return out
}

// TestReferenceSaysNothingAboutIntent: a queue URL in a worker's configuration
// is how it polls the queue as often as how it sends to it. Drawing publish
// would make every polling worker a producer.
func TestReferenceSaysNothingAboutIntent(t *testing.T) {
	g := cases2(t)
	for _, from := range []string{"lambda/front-fn", "ecs/main/worker"} {
		if _, ok := edge(g, from, "sqs/jobs", KindReferences); !ok {
			t.Errorf("%s → sqs/jobs: no reference", from)
		}
		if _, ok := edge(g, from, "sqs/jobs", KindPublish); ok {
			t.Errorf("%s → sqs/jobs: a configuration value was read as a send", from)
		}
	}
}

// TestReferenceFlowFollowsTheTarget: whatever a workload does with a queue, the
// queue decouples it; a table or a function it names is waited on.
func TestReferenceFlowFollowsTheTarget(t *testing.T) {
	g := cases2(t)
	for id, want := range map[string]Flow{
		"sqs/jobs":           Async, // referenced from the request path
		"lambda/jobs-worker": Async, // consumes it
		"ddb/ledger":         Sync,
		"lambda/notify-fn":   Sync,
		"ecs/main/worker":    Unreached, // its reference points out of it, not into it
	} {
		if got := flowOf(t, g, id); got != want {
			t.Errorf("%s: got %s, want %s", id, got, want)
		}
	}
}

// TestAWSEndpointsAreNeverThirdParties: the gateway sends AWS traffic to Floci.
// An ext/ node for an AWS hostname would be a mock standing in for AWS, and one
// for a sidecar or an internal name would be a mock of the user's own system.
func TestAWSEndpointsAreNeverThirdParties(t *testing.T) {
	g := cases2(t)
	for _, n := range g.Nodes {
		if !n.External {
			continue
		}
		if isAWSHost(n.Name) || isLocalHost(n.Name) || !strings.Contains(n.Name, ".") {
			t.Errorf("%s became a third party", n.ID)
		}
	}
	for _, want := range []string{"ext/api.partner-pay.example.com", "ext/hooks.partner.example.com"} {
		if _, ok := g.Node(want); !ok {
			t.Errorf("third party %s not found", want)
		}
	}
	// A sidecar is part of the workload: neither a dependency nor a finding.
	for _, f := range g.Findings {
		if strings.Contains(f.Target, "localhost") || strings.Contains(f.Target, "127.0.0.1") {
			t.Errorf("a sidecar was reported: %+v", f)
		}
	}
	for target, why := range map[string]string{
		"rds.amazonaws.com":          "an RDS endpoint",
		"search":                     "an internal name",
		"amqp://mq.vendor.example":   "a non-HTTP third party",
		"s3://front-config":          "an S3 bucket",
		"eu-west-1.amazonaws.com":    "an API in another region",
		"secretsmanager:us-east-1:1": "a resource type with no node",
	} {
		if !finding(g, "unresolved", "lambda/front-fn", target) {
			t.Errorf("%s (%s) was not reported", target, why)
		}
	}
}

// TestRedactedURLKeepsItsHost: postgres://app:<redacted>@host is still a
// dependency on its host — redaction keeps the host for exactly this — and the
// user name in front of the withheld password must not be read as a host.
func TestRedactedURLKeepsItsHost(t *testing.T) {
	g := cases2(t)
	var db bool
	for _, f := range g.Findings {
		if f.Node != "ecs/main/api" {
			continue
		}
		db = db || strings.HasPrefix(f.Detail, "DATABASE_URL names an RDS endpoint")
		if strings.Contains(f.Detail, "no domain") {
			t.Errorf("the redacted URL was misread: %s", f.Detail)
		}
	}
	if !db {
		t.Error("the database behind a redacted URL was not reported")
	}
	if _, ok := edge(g, "lambda/front-fn", "ext/hooks.partner.example.com", KindHTTP); !ok {
		t.Error("a webhook whose token was withheld from the path lost its host")
	}
}

// TestKeyNameBreaksATieButNeverSilently: TABLE_NAME=orders in an account with a
// table and a queue called orders means the table, and says so; with no hint
// both stay candidates; a key naming a type no candidate has lowers them all.
func TestKeyNameBreaksATieButNeverSilently(t *testing.T) {
	g := cases2(t)
	conf := func(from, to string) Confidence {
		e, ok := edge(g, from, to, KindReferences)
		if !ok {
			t.Fatalf("%s → %s: no reference", from, to)
		}
		return e.Confidence
	}
	if c := conf("lambda/front-fn", "ddb/orders"); c != Medium {
		t.Errorf("TABLE_NAME=orders → table: %s, want medium", c)
	}
	if c := conf("lambda/front-fn", "sqs/orders"); c != Low {
		t.Errorf("TABLE_NAME=orders → queue: %s, want low", c)
	}
	if !finding(g, "ambiguous", "lambda/front-fn", "orders") {
		t.Error("an ambiguity settled by the key name was not reported")
	}
	for _, to := range []string{"ddb/orders", "sqs/orders"} {
		if c := conf("ecs/main/api", to); c != Low {
			t.Errorf("TARGET=orders → %s: %s, want low", to, c)
		}
	}
	if !finding(g, "ambiguous", "ecs/main/api", "orders") {
		t.Error("an unsettled ambiguity was not reported")
	}
	// A specific name does not settle an ambiguity either: the real account's
	// ce-test-orders is a table and a queue.
	for _, to := range []string{"ddb/order-archive", "sqs/order-archive"} {
		if c := conf("ecs/main/api", to); c != Low {
			t.Errorf("ARCHIVE=order-archive → %s: %s, want low", to, c)
		}
	}
	if c := conf("lambda/front-fn", "ddb/order-events-store"); c != Low {
		t.Errorf("QUEUE_NAME naming only a table: %s, want low", c)
	}
	if c := conf("lambda/front-fn", "lambda/notify-fn"); c != Medium {
		t.Errorf("NOTIFY_FUNCTION=notify-fn: %s, want medium", c)
	}
}

// TestLowCandidatesDoNotDriveFlow: low is a suggestion the user has not
// accepted. LOG_LEVEL=info must not put a queue called info on the request path.
func TestLowCandidatesDoNotDriveFlow(t *testing.T) {
	g := cases2(t)
	if _, ok := edge(g, "lambda/front-fn", "sqs/info", KindReferences); !ok {
		t.Fatal("setup: the LOG_LEVEL=info candidate is missing")
	}
	for _, id := range []string{"sqs/info", "sqs/orders"} {
		if f := flowOf(t, g, id); f != Unreached {
			t.Errorf("%s reached only through low candidates: got %s, want unreached", id, f)
		}
	}
}

// TestConfigReferencesPassTheAccountCheck: the namesake trap from Tier 1, in a
// queue URL. PARTNER_QUEUE_URL points at a queue called jobs in another account;
// the local jobs queue must not gain it as evidence.
func TestConfigReferencesPassTheAccountCheck(t *testing.T) {
	g := cases2(t)
	e, _ := edge(g, "lambda/front-fn", "sqs/jobs", KindReferences)
	if len(e.Evidence) != 1 || !strings.Contains(e.Evidence[0].Source, "JOBS_QUEUE_URL") {
		t.Errorf("front-fn → sqs/jobs evidence: %+v", e.Evidence)
	}
	if !finding(g, "unresolved", "lambda/front-fn", ":999999999999:jobs") {
		t.Error("the foreign queue was not reported")
	}
}

// TestReferenceIsAbsorbedByTheEdgeThatStatesIntent: DLQ_URL naming the
// function's dead-letter queue is evidence for the publish edge, not a second
// edge beside it. A consumer holding its own queue's URL is not folded into the
// consume edge, which points the other way.
func TestReferenceIsAbsorbedByTheEdgeThatStatesIntent(t *testing.T) {
	g := cases2(t)
	if _, ok := edge(g, "lambda/dlq-fn", "sqs/fn-dlq", KindReferences); ok {
		t.Error("a reference survived beside the edge that states its intent")
	}
	e, _ := edge(g, "lambda/dlq-fn", "sqs/fn-dlq", KindPublish)
	if got := strings.Join(rulesOf(e), ","); got != "config.value-scan,lambda.dead-letter" {
		t.Errorf("publish evidence: %s", got)
	}
	if _, ok := edge(g, "lambda/jobs-worker", "sqs/jobs", KindReferences); !ok {
		t.Error("a consumer's reference to its queue was folded into the reverse consume edge")
	}
}

// TestLowReferenceIsNotCorroboration: a candidate from an ambiguous name, when
// a typed edge already links the pair, is dropped — attached, it would read as
// a second source confirming the edge.
func TestLowReferenceIsNotCorroboration(t *testing.T) {
	b := newBuilder()
	b.addEdge("a", "q", KindPublish, Certain, Active, Evidence{Rule: "lambda.dead-letter", Source: "s", Detail: "d"})
	b.addEdge("a", "q", KindReferences, Low, Active, Evidence{Rule: "config.value-scan", Source: "s", Detail: "candidate"})
	b.addEdge("a", "t", KindWrite, Medium, Active, Evidence{Rule: "iam", Source: "s", Detail: "d"})
	b.addEdge("a", "t", KindReferences, High, Active, Evidence{Rule: "config.value-scan", Source: "s", Detail: "arn"})
	b.absorbReferences()
	if len(b.edges) != 2 {
		t.Fatalf("want the two typed edges only, have %d", len(b.edges))
	}
	if p := b.edges[edgeKey{"a", "q", KindPublish}]; len(p.Evidence) != 1 {
		t.Errorf("a low candidate was attached as evidence: %+v", p.Evidence)
	}
	if w := b.edges[edgeKey{"a", "t", KindWrite}]; len(w.Evidence) != 2 || w.Confidence != High {
		t.Errorf("an agreeing high reference should add evidence and raise confidence: %+v", w)
	}
}

// TestStageVariablesAreReadOnce: a variable an integration URI uses was
// interpreted per stage by apigw.integration; the rest are config the API
// hands its backends.
func TestStageVariablesAreReadOnce(t *testing.T) {
	g := cases2(t)
	e, _ := edge(g, "apigw/api0000009", "lambda/templated-fn", KindInvoke)
	if got := strings.Join(rulesOf(e), ","); got != "apigw.integration" {
		t.Errorf("templated-fn evidence: %s", got)
	}
	if r, ok := edge(g, "apigw/api0000009", "ddb/ledger", KindReferences); !ok || r.Confidence != Medium {
		t.Errorf("stage variable table=ledger: %+v", r)
	}
}

// TestFlagsCarryTheirKey: --events-table order-events-store is read with the
// hint its flag gives, and --callback=<url> as a URL.
func TestFlagsCarryTheirKey(t *testing.T) {
	g := cases2(t)
	e, ok := edge(g, "ecs/main/api", "ddb/order-events-store", KindReferences)
	if !ok || e.Confidence != Medium || !strings.Contains(e.Evidence[0].Detail, "--events-table") {
		t.Errorf("--events-table: %+v", e)
	}
	if _, ok := edge(g, "ecs/main/api", "ext/hooks.partner.example.com", KindHTTP); !ok {
		t.Error("--callback=<url> was not read")
	}
}

// TestBlindSpotsAreReported: configuration the linker could not read is a
// finding, so an empty list of references is never mistaken for "names nothing".
func TestBlindSpotsAreReported(t *testing.T) {
	g := cases2(t)
	if !finding(g, "unscanned", "ecs/main/ghost", "ecs/taskdef/ghost:1") {
		t.Error("a service whose task definition is missing was not reported")
	}
	if !finding(g, "unscanned", "lambda/locked-fn", "environment") {
		t.Error("a function with an unreadable environment was not reported")
	}
	if _, ok := edge(g, "lambda/front-fn", "lambda/front-fn", KindReferences); ok {
		t.Error("a function naming itself produced a self-edge")
	}
}

func TestKeyHint(t *testing.T) {
	for key, want := range map[string]string{
		"TABLE_NAME":     "dynamodb.table",
		"ordersTable":    "dynamodb.table",
		"events-table":   "dynamodb.table",
		"DLQ_URL":        "sqs.queue",
		"NOTIFY_FN":      "lambda.function",
		"QUEUE_TABLE":    "",
		"TIMETABLE_NAME": "", // a whole token, not a substring
		"":               "",
	} {
		if got := keyHint(key); got != want {
			t.Errorf("%q: got %q want %q", key, got, want)
		}
	}
}

func TestSplitURL(t *testing.T) {
	for in, want := range map[string][3]string{
		"postgres://app:REDACTED@db.example.com:5432/x": {"postgres", "db.example.com", "5432"},
		"https://API.Example.com/v1?q=1":                {"https", "api.example.com", ""},
		"http://[::1]:8080/":                            {"http", "::1", "8080"},
		"amqp://mq.vendor.example.net:5671":             {"amqp", "mq.vendor.example.net", "5671"},
	} {
		s, h, p := splitURL(in)
		if [3]string{s, h, p} != want {
			t.Errorf("%s: got %s %s %s", in, s, h, p)
		}
	}
}
