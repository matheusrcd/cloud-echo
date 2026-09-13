package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func collectLambda(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "lambda")
	out := &captureEmitter{}
	if err := (&Lambda{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func TestLambdaCollectsExpectedResources(t *testing.T) {
	out, _ := collectLambda(t)

	want := []string{
		"lambda/audit-writer",
		"lambda/esm/0b6f2a3c-1111-4d2e-9f00-000000000001",
		"lambda/esm/0b6f2a3c-2222-4d2e-9f00-000000000002",
		"lambda/esm/0b6f2a3c-3333-4d2e-9f00-000000000003",
		"lambda/legacy-report",
		"lambda/order-processor",
		"lambda/webhook-receiver",
	}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Errorf("resource ids:\n got %q\nwant %q", got, want)
	}
}

// TestLambdaFollowsPagination is the first test in the suite that exercises a
// real SDK paginator across more than one page. Two of the four functions exist
// only on page two; a collector that trusted the first page would lose them and
// report a clean, complete-looking scan.
func TestLambdaFollowsPagination(t *testing.T) {
	out, tr := collectLambda(t)

	if n := tr.count("Lambda", "ListFunctions"); n != 2 {
		t.Errorf("ListFunctions called %d times, want 2 (one per page)", n)
	}
	for _, id := range []string{"lambda/legacy-report", "lambda/webhook-receiver"} {
		if _, ok := out.byID(id); !ok {
			t.Errorf("%s is on page two and was lost", id)
		}
	}
}

// TestLambdaSecretsNeverReachTheInventory: Lambda env vars are where plaintext
// secrets are found most often, which is why redaction landed before this
// collector did.
func TestLambdaSecretsNeverReachTheInventory(t *testing.T) {
	out, _ := collectLambda(t)

	for _, r := range out.resources {
		blob, _ := json.Marshal(r)
		for _, s := range []string{"not-a-real-payments-token-0000", "Xq7Lm2Pz9Rt4Wn6Ks1Vb8Hd3"} {
			if bytes.Contains(blob, []byte(s)) {
				t.Errorf("%s: secret %q survived into the serialized resource", r.ID, s)
			}
		}
	}

	fn, _ := out.byID("lambda/order-processor")
	var spec functionSpec
	specOf(t, fn, &spec)

	if got := spec.Env["SLACK_WEBHOOK_URL"]; !strings.HasPrefix(got, "https://hooks.slack.com/services/") {
		t.Errorf("the webhook's host is the integration signal and was lost: %q", got)
	}
	if got := spec.Env["TABLE_NAME"]; got != "orders" {
		t.Errorf("TABLE_NAME altered: %q", got)
	}
	if want := []string{"env:PAYMENTS_TOKEN", "env:SLACK_WEBHOOK_URL"}; !reflect.DeepEqual(spec.Redacted, want) {
		t.Errorf("redacted: got %q want %q", spec.Redacted, want)
	}
}

// TestLambdaReportsUnreadableEnvironment covers a function whose variables are
// encrypted with a customer KMS key. The scanner policy deliberately grants no
// kms:Decrypt, so AWS returns an error in place of the values — the policy doing
// its job. But this function's Tier-2 signal is now invisible, and a silent gap
// would read to the user as "this function talks to nothing".
func TestLambdaReportsUnreadableEnvironment(t *testing.T) {
	out, _ := collectLambda(t)

	fn, _ := out.byID("lambda/legacy-report")
	var spec functionSpec
	specOf(t, fn, &spec)

	if spec.EnvUnreadable != "KMSAccessDeniedException" {
		t.Errorf("envUnreadable: got %q", spec.EnvUnreadable)
	}
	if len(out.warnings) != 1 || out.warnings[0].Kind != "unreadable" ||
		!strings.Contains(out.warnings[0].Message, "legacy-report") {
		t.Errorf("want one 'unreadable' warning naming legacy-report, got %+v", out.warnings)
	}
}

// TestLambdaMissingResourcePolicyIsNormal: GetPolicy answers 404 for every
// function without a resource policy, which is most of them. Treating that as a
// warning would bury the real ones.
func TestLambdaMissingResourcePolicyIsNormal(t *testing.T) {
	out, _ := collectLambda(t)

	for _, w := range out.warnings {
		if w.Op == "GetPolicy" {
			t.Errorf("a missing resource policy was reported as a problem: %+v", w)
		}
	}

	fn, _ := out.byID("lambda/webhook-receiver")
	var spec functionSpec
	specOf(t, fn, &spec)
	if !bytes.Contains(spec.ResourcePolicy, []byte("apigateway.amazonaws.com")) {
		t.Errorf("resource policy lost its principal: %s", spec.ResourcePolicy)
	}

	none, _ := out.byID("lambda/order-processor")
	var noneSpec functionSpec
	specOf(t, none, &noneSpec)
	if len(noneSpec.ResourcePolicy) != 0 {
		t.Errorf("a function without a policy reported one: %s", noneSpec.ResourcePolicy)
	}
}

func TestLambdaResolvesRoleID(t *testing.T) {
	out, _ := collectLambda(t)

	for id, want := range map[string]string{
		"lambda/order-processor": "iam/role/order-processor-role",
		// The role has an IAM path; role names are unique regardless of path,
		// so the id must not include it.
		"lambda/legacy-report": "iam/role/legacy-report-role",
	} {
		fn, _ := out.byID(id)
		var spec functionSpec
		specOf(t, fn, &spec)
		if spec.RoleID != want {
			t.Errorf("%s roleId: got %q want %q", id, spec.RoleID, want)
		}
	}
}

// TestLambdaMappingsResolveBothEnds checks the Tier-1 edges this collector
// exists to provide. Each mapping must name its source and its function as
// inventory ids, so the linker can draw a `certain` edge with no parsing.
func TestLambdaMappingsResolveBothEnds(t *testing.T) {
	out, _ := collectLambda(t)

	for _, tc := range []struct {
		id, source, sourceType, function, qualifier string
		enabled                                     bool
	}{
		{"lambda/esm/0b6f2a3c-1111-4d2e-9f00-000000000001", "sqs/orders-events", "sqs", "lambda/order-processor", "live", true},
		// A DynamoDB stream ARN resolves to its table: the stream is not a
		// separate node.
		{"lambda/esm/0b6f2a3c-2222-4d2e-9f00-000000000002", "ddb/orders", "dynamodb-stream", "lambda/audit-writer", "", true},
		// Disabled mappings are still recorded — the linker decides what a
		// disabled edge means — but must not read as live.
		{"lambda/esm/0b6f2a3c-3333-4d2e-9f00-000000000003", "sqs/orders", "sqs", "lambda/order-processor", "", false},
	} {
		r, ok := out.byID(tc.id)
		if !ok {
			t.Errorf("missing %s", tc.id)
			continue
		}
		var spec mappingSpec
		specOf(t, r, &spec)

		if spec.Source.ID != tc.source || spec.SourceType != tc.sourceType {
			t.Errorf("%s source: got %q (%s) want %q (%s)", tc.id, spec.Source.ID, spec.SourceType, tc.source, tc.sourceType)
		}
		if spec.FunctionID != tc.function || spec.Qualifier != tc.qualifier {
			t.Errorf("%s function: got %q:%q want %q:%q", tc.id, spec.FunctionID, spec.Qualifier, tc.function, tc.qualifier)
		}
		if spec.Enabled == nil || *spec.Enabled != tc.enabled {
			t.Errorf("%s enabled: got %v want %v", tc.id, spec.Enabled, tc.enabled)
		}
		if r.Source.API != "lambda:ListEventSourceMappings" {
			t.Errorf("%s provenance: %q", tc.id, r.Source.API)
		}
	}

	r, _ := out.byID("lambda/esm/0b6f2a3c-2222-4d2e-9f00-000000000002")
	var spec mappingSpec
	specOf(t, r, &spec)
	if spec.OnFailure == nil || spec.OnFailure.ID != "sqs/orders-events-dlq" {
		t.Errorf("on-failure destination not resolved: %+v", spec.OnFailure)
	}
}

func TestResourceIDFromARNNeverInventsIDs(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:sqs:us-east-1:123456789012:orders":                              "sqs/orders",
		"arn:aws:dynamodb:us-east-1:123456789012:table/orders":                   "ddb/orders",
		"arn:aws:dynamodb:us-east-1:123456789012:table/orders/stream/2026-08-01": "ddb/orders",
		"arn:aws:lambda:us-east-1:123456789012:function:fn:prod":                 "lambda/fn",
		// No collector exists for these yet. An invented id would look like a
		// real node to the linker and dangle silently.
		"arn:aws:kinesis:us-east-1:123456789012:stream/clicks": "",
		"arn:aws:sns:us-east-1:123456789012:alerts":            "",
		"not-an-arn": "",
	} {
		if got := resourceIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}

func TestLambdaUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectLambda(t)
	assertOpsMatchAllowList(t, "Lambda", tr)
}

// TestMappingEnabledNeverGuesses pins the fix for a failure seen against a real
// account: a live mapping scanned while its batch size was being changed reported
// "Updating", and a two-state rule recorded it as disabled.
func TestMappingEnabledNeverGuesses(t *testing.T) {
	for state, want := range map[string]struct {
		enabled      string // "true" | "false" | "unknown"
		transitional bool
	}{
		"Enabled":   {"true", false},
		"Disabled":  {"false", false},
		"Enabling":  {"true", true},
		"Disabling": {"false", true},
		"Deleting":  {"false", true},
		"Creating":  {"unknown", true},
		"Updating":  {"unknown", true},
	} {
		e, tr := mappingEnabled(state)
		got := "unknown"
		if e != nil {
			got = map[bool]string{true: "true", false: "false"}[*e]
		}
		if got != want.enabled || tr != want.transitional {
			t.Errorf("%s: got enabled=%s transitional=%v, want %s/%v", state, got, tr, want.enabled, want.transitional)
		}
	}
}
