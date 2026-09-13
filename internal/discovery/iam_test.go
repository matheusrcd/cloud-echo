package discovery

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// collectIAM runs the phase-1 collectors that reference roles, then IAM over
// their output — the same shape as a real scan, without the scan runner.
func collectIAM(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixtures(t, "orders", "ecs", "lambda", "iam")
	sess := fixtureSession(tr)

	phase1 := &captureEmitter{}
	for _, c := range []Collector{&ECS{}, &Lambda{}} {
		if err := c.Collect(context.Background(), sess, phase1); err != nil {
			t.Fatalf("%s: %v", c.Service(), err)
		}
	}

	out := &captureEmitter{}
	if err := (&IAM{}).CollectFrom(context.Background(), sess, phase1.resources, out); err != nil {
		t.Fatalf("IAM: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func TestIAMCollectsOnlyRolesWorkloadsAssume(t *testing.T) {
	out, _ := collectIAM(t)

	want := []string{
		"iam/aws-policy/AWSLambdaBasicExecutionRole",
		"iam/aws-policy/AmazonSQSFullAccess",
		"iam/policy/orders-observability",
		"iam/policy/workload-boundary",
		"iam/role/audit-writer-role",
		"iam/role/notifications-task",
		"iam/role/order-processor-role",
		"iam/role/orders-api-task",
		"iam/role/webhook-receiver-role",
	}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Errorf("resource ids:\n got %q\nwant %q", got, want)
	}
}

// TestIAMSkipsECSExecutionRoles pins a scoping decision. The execution role
// belongs to the ECS agent, which uses it to pull images and inject secrets[].
// Reading it as the application's permissions would make every service appear
// to read every secret the agent fetches on its behalf.
//
// The fixture has no exchange for ecsTaskExecutionRole, so a request for it
// would also fail assertAllMatched; this states the intent by name.
func TestIAMSkipsECSExecutionRoles(t *testing.T) {
	out, _ := collectIAM(t)
	if _, ok := out.byID("iam/role/ecsTaskExecutionRole"); ok {
		t.Error("the ECS execution role was collected as if it were an application identity")
	}
}

// TestIAMReadsSharedPoliciesOnce: AWSLambdaBasicExecutionRole is attached to
// three roles here and to most Lambda roles in most real accounts.
func TestIAMReadsSharedPoliciesOnce(t *testing.T) {
	_, tr := collectIAM(t)
	for _, op := range []string{"GetPolicy", "GetPolicyVersion"} {
		if n := tr.count("IAM", op); n != 4 {
			t.Errorf("%s called %d times, want 4 (one per distinct policy)", op, n)
		}
	}
}

// TestIAMDecodesPolicyDocuments covers the URL encoding IAM applies to every
// document it returns, end to end through the real SDK. orders-observability
// contains "orders+payments team", arriving as %2B and %20; both must round-trip.
//
// This does not distinguish PathUnescape from QueryUnescape — both decode %2B to
// '+'. They differ only on a *literal* '+' in the encoded text, which is pinned
// separately in TestDecodePolicyDocument.
func TestIAMDecodesPolicyDocuments(t *testing.T) {
	out, _ := collectIAM(t)

	pol, _ := out.byID("iam/policy/orders-observability")
	var ps policySpec
	specOf(t, pol, &ps)
	if !strings.Contains(string(ps.Document), `"orders+payments team"`) {
		t.Errorf("document decoded wrongly — '+' and space must both survive:\n%s", ps.Document)
	}
	if ps.AWSManaged || ps.DefaultVersion != "v2" {
		t.Errorf("policy metadata: %+v", ps)
	}

	role, _ := out.byID("iam/role/orders-api-task")
	var rs roleSpec
	specOf(t, role, &rs)
	if !strings.Contains(string(rs.TrustPolicy), "ecs-tasks.amazonaws.com") {
		t.Errorf("trust policy not decoded: %s", rs.TrustPolicy)
	}
	if len(rs.InlinePolicies) != 1 || rs.InlinePolicies[0].Name != "orders-data" {
		t.Fatalf("inline policies: %+v", rs.InlinePolicies)
	}
	var doc struct{ Statement []json.RawMessage }
	if err := json.Unmarshal(rs.InlinePolicies[0].Document, &doc); err != nil || len(doc.Statement) != 3 {
		t.Errorf("inline document is not usable JSON with 3 statements: %v %s", err, rs.InlinePolicies[0].Document)
	}
}

func TestIAMRecordsWhoAssumesEachRole(t *testing.T) {
	out, _ := collectIAM(t)

	for id, want := range map[string][]string{
		"iam/role/orders-api-task":      {"ecs/taskdef/orders-api:41"},
		"iam/role/order-processor-role": {"lambda/order-processor"},
	} {
		r, _ := out.byID(id)
		var spec roleSpec
		specOf(t, r, &spec)
		if !reflect.DeepEqual(spec.AssumedBy, want) {
			t.Errorf("%s assumedBy: got %q want %q", id, spec.AssumedBy, want)
		}
		if r.Region != "global" {
			t.Errorf("%s region: got %q — IAM is global", id, r.Region)
		}
	}
}

// TestIAMRecordsPermissionsBoundary: effective permission is the intersection
// of the role's policies with its boundary, so Tier 3 cannot be right without it.
func TestIAMRecordsPermissionsBoundary(t *testing.T) {
	out, _ := collectIAM(t)

	r, _ := out.byID("iam/role/orders-api-task")
	var spec roleSpec
	specOf(t, r, &spec)
	if spec.PermissionsBoundary == nil || spec.PermissionsBoundary.ID != "iam/policy/workload-boundary" {
		t.Fatalf("boundary not recorded: %+v", spec.PermissionsBoundary)
	}
	if _, ok := out.byID(spec.PermissionsBoundary.ID); !ok {
		t.Error("the boundary policy's document was not collected")
	}
}

func TestIAMKeepsDenyStatements(t *testing.T) {
	out, _ := collectIAM(t)

	r, _ := out.byID("iam/role/webhook-receiver-role")
	var spec roleSpec
	specOf(t, r, &spec)
	if len(spec.InlinePolicies) != 1 || !strings.Contains(string(spec.InlinePolicies[0].Document), `"Deny"`) {
		t.Errorf("the explicit Deny was lost — the linker needs it to suppress edges: %+v", spec.InlinePolicies)
	}
}

// TestIAMReportsDanglingRoles: legacy-report's role was deleted while the
// function still points at it. That function cannot start, which is worth
// knowing — and it must not stop the other roles from being read.
func TestIAMReportsDanglingRoles(t *testing.T) {
	out, _ := collectIAM(t)

	var found bool
	for _, w := range out.warnings {
		if w.Kind == "dangling-reference" &&
			strings.Contains(w.Message, "legacy-report-role") &&
			strings.Contains(w.Message, "lambda/legacy-report") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a dangling-reference warning naming the role and its referrer, got %+v", out.warnings)
	}
	if _, ok := out.byID("iam/role/legacy-report-role"); ok {
		t.Error("emitted a role that does not exist")
	}
}

// TestIAMSkipsCrossAccountRoles: GetRole takes a name, not an ARN. Looking up a
// foreign account's role by name would silently return a *different* role if one
// with the same name existed here.
// TestIAMRecordsWhatItCouldNotRead: a denied read must be visible on the role
// itself, not only as a scan warning. Tier 3 reports a workload that names a
// queue its role cannot touch; with the policy unread, the role would look like
// it grants nothing, and that claim would come from a blind spot.
func TestIAMRecordsWhatItCouldNotRead(t *testing.T) {
	tr := loadFixtures(t, "orders", "ecs", "lambda", "iam")
	denied := exchange{Op: "GetRolePolicy", Match: "RoleName=orders-api-task&", Status: 403, service: "iam",
		Body: `<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><Error><Type>Sender</Type>` +
			`<Code>AccessDenied</Code><Message>not authorized to perform: iam:GetRolePolicy</Message></Error></ErrorResponse>`}
	// The transport answers with the first match, so this shadows the recorded one.
	tr.exchanges = append([]exchange{denied}, tr.exchanges...)
	sess := fixtureSession(tr)

	phase1 := &captureEmitter{}
	for _, c := range []Collector{&ECS{}, &Lambda{}} {
		if err := c.Collect(context.Background(), sess, phase1); err != nil {
			t.Fatal(err)
		}
	}
	out := &captureEmitter{}
	if err := (&IAM{}).CollectFrom(context.Background(), sess, phase1.resources, out); err != nil {
		t.Fatalf("a denied GetRolePolicy aborted IAM: %v", err)
	}
	var got, other roleSpec
	r, _ := out.byID("iam/role/orders-api-task")
	_ = json.Unmarshal(r.Spec, &got)
	if len(got.InlinePolicies) != 0 || len(got.Unread) != 1 || !strings.HasPrefix(got.Unread[0], "inline policy ") {
		t.Errorf("the unread inline policy is not recorded on the role: inline=%d unread=%q", len(got.InlinePolicies), got.Unread)
	}
	r, _ = out.byID("iam/role/order-processor-role")
	_ = json.Unmarshal(r.Spec, &other)
	if len(other.Unread) != 0 {
		t.Errorf("a fully read role reports unread parts: %q", other.Unread)
	}
}

func TestIAMSkipsCrossAccountRoles(t *testing.T) {
	spec, _ := json.Marshal(functionSpec{RoleARN: "arn:aws:iam::999999999999:role/shared-reader"})
	prior := []inventory.Resource{{ID: "lambda/cross", Type: "lambda.function", Spec: spec}}

	out := &captureEmitter{}
	refs := assumedRoles(prior, "123456789012", out)

	if len(refs) != 0 {
		t.Errorf("a role in another account was queued for lookup by name: %v", refs)
	}
	if len(out.warnings) != 1 || out.warnings[0].Kind != "out-of-scope" {
		t.Errorf("want one out-of-scope warning, got %+v", out.warnings)
	}
}

func TestPolicyIDsKeepAWSAndCustomerManagedApart(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:iam::aws:policy/AmazonSQSFullAccess":                      "iam/aws-policy/AmazonSQSFullAccess",
		"arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole": "iam/aws-policy/AWSLambdaBasicExecutionRole",
		// Legal: a customer policy with an AWS-managed policy's name. Same
		// name, different document — it must not share the id.
		"arn:aws:iam::123456789012:policy/AmazonSQSFullAccess": "iam/policy/AmazonSQSFullAccess",
		"arn:aws:iam::123456789012:policy/team/orders-rw":      "iam/policy/orders-rw",
		"arn:aws:iam::123456789012:role/not-a-policy":          "",
	} {
		if got := policyIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}

func TestDecodePolicyDocument(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"%7B%22a%22%3A%22x%2By%20z%22%7D", `{"a":"x+y z"}`},
		// The case that decides between PathUnescape and QueryUnescape. Under
		// RFC 3986 — which IAM follows — a literal '+' means '+'. Form decoding
		// would turn it into a space and change what the document says.
		{"%7B%22a%22%3A%22x+y%22%7D", `{"a":"x+y"}`},
		// Already-decoded JSON with a literal '%' must pass through untouched;
		// a second decode would fail on "%\"".
		{`{"a":"100%"}`, `{"a":"100%"}`},
		{"", ""},
	} {
		got, err := decodePolicyDocument(tc.in)
		if err != nil || string(got) != tc.want {
			t.Errorf("%q: got %q, %v want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := decodePolicyDocument("%7Bnot-json"); err == nil {
		t.Error("accepted a document that is not JSON after decoding")
	}
}

func TestIAMUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectIAM(t)
	assertOpsMatchAllowList(t, "IAM", tr)
}
