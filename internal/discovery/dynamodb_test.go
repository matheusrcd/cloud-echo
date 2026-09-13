package discovery

import (
	"context"
	"reflect"
	"testing"
)

func collectDDB(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "dynamodb")
	out := &captureEmitter{}
	if err := (&DynamoDB{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func TestDynamoDBCollectsExpectedTables(t *testing.T) {
	out, _ := collectDDB(t)

	want := []string{"ddb/orders", "ddb/orders-audit"}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Errorf("table ids:\n got %q\nwant %q", got, want)
	}
	if len(out.warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", out.warnings)
	}
}

// TestDynamoDBJoinsKeySchemaWithAttributeTypes covers the one place this
// collector can produce a table that looks right and behaves wrong.
//
// AWS returns the key schema and the attribute types as two separate lists. A
// local table built from the schema alone has key names but no types, which
// accepts writes in production and rejects them locally — the exact failure a
// fidelity tool must not have.
func TestDynamoDBJoinsKeySchemaWithAttributeTypes(t *testing.T) {
	out, _ := collectDDB(t)

	tbl, ok := out.byID("ddb/orders")
	if !ok {
		t.Fatal("missing ddb/orders")
	}
	var spec tableSpec
	specOf(t, tbl, &spec)

	want := []keyPart{
		{Name: "pk", Type: "S", Role: "HASH"},
		{Name: "sk", Type: "S", Role: "RANGE"},
	}
	if !reflect.DeepEqual(spec.KeySchema, want) {
		t.Errorf("key schema:\n got %+v\nwant %+v", spec.KeySchema, want)
	}

	if len(spec.GSIs) != 1 {
		t.Fatalf("want 1 GSI, got %d", len(spec.GSIs))
	}
	gsi := spec.GSIs[0]
	if gsi.Name != "gsi1" {
		t.Errorf("GSI name: got %q", gsi.Name)
	}
	// The GSI's range key is numeric while the table's is a string — a shared
	// type lookup that ignored the attribute definitions would get this wrong.
	wantGSI := []keyPart{
		{Name: "gsi1pk", Type: "S", Role: "HASH"},
		{Name: "createdAt", Type: "N", Role: "RANGE"},
	}
	if !reflect.DeepEqual(gsi.KeySchema, wantGSI) {
		t.Errorf("GSI key schema:\n got %+v\nwant %+v", gsi.KeySchema, wantGSI)
	}
	if gsi.Projection != "INCLUDE" || len(gsi.NonKeyAttr) != 2 {
		t.Errorf("GSI projection lost: %q %v", gsi.Projection, gsi.NonKeyAttr)
	}
}

func TestDynamoDBRecordsStream(t *testing.T) {
	out, _ := collectDDB(t)

	tbl, _ := out.byID("ddb/orders")
	var spec tableSpec
	specOf(t, tbl, &spec)

	if spec.Stream == nil || !spec.Stream.Enabled {
		t.Fatal("stream was not recorded")
	}
	if spec.Stream.ViewType != "NEW_AND_OLD_IMAGES" {
		t.Errorf("stream view type: got %q", spec.Stream.ViewType)
	}
}

// TestDynamoDBRecordsTTL pins a second API call whose absence is silent: TTL is
// not in DescribeTable, and a local table missing it keeps rows the real one
// would have expired.
func TestDynamoDBRecordsTTL(t *testing.T) {
	out, _ := collectDDB(t)

	orders, _ := out.byID("ddb/orders")
	var withTTL tableSpec
	specOf(t, orders, &withTTL)
	if withTTL.TTL == nil || withTTL.TTL.Attribute != "expiresAt" {
		t.Errorf("TTL not recorded: %+v", withTTL.TTL)
	}

	audit, _ := out.byID("ddb/orders-audit")
	var noTTL tableSpec
	specOf(t, audit, &noTTL)
	if noTTL.TTL != nil {
		t.Errorf("a DISABLED TTL was reported as enabled: %+v", noTTL.TTL)
	}
}

// TestDynamoDBDefaultsBillingModeForOlderTables covers tables created before
// BillingModeSummary existed, which report nothing and are provisioned by
// definition. Reporting an empty string there would leak into the blueprint.
func TestDynamoDBDefaultsBillingModeForOlderTables(t *testing.T) {
	out, _ := collectDDB(t)

	audit, _ := out.byID("ddb/orders-audit")
	var spec tableSpec
	specOf(t, audit, &spec)

	if spec.BillingMode != "PROVISIONED" {
		t.Errorf("billing mode: got %q want PROVISIONED", spec.BillingMode)
	}

	orders, _ := out.byID("ddb/orders")
	var ordersSpec tableSpec
	specOf(t, orders, &ordersSpec)
	if ordersSpec.BillingMode != "PAY_PER_REQUEST" {
		t.Errorf("billing mode: got %q want PAY_PER_REQUEST", ordersSpec.BillingMode)
	}
}

func TestDynamoDBUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectDDB(t)
	assertOpsMatchAllowList(t, "DynamoDB", tr)
}

// TestDynamoDBRecordsProvisionedThroughput: a provisioned table cannot be
// recreated without its capacity, and the M1 round trip had to read it from Raw.
// On-demand tables return a zeroed block that must not read as capacity.
func TestDynamoDBRecordsProvisionedThroughput(t *testing.T) {
	out, _ := collectDDB(t)

	audit, _ := out.byID("ddb/orders-audit")
	var a tableSpec
	specOf(t, audit, &a)
	if a.Throughput == nil || a.Throughput.Read != 5 || a.Throughput.Write != 5 {
		t.Errorf("provisioned throughput: %+v", a.Throughput)
	}

	orders, _ := out.byID("ddb/orders")
	var o tableSpec
	specOf(t, orders, &o)
	if o.Throughput != nil {
		t.Errorf("on-demand table reported capacity: %+v", o.Throughput)
	}
}
