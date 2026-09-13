package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func collectECS(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "ecs")
	out := &captureEmitter{}
	if err := (&ECS{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func TestECSCollectsExpectedResources(t *testing.T) {
	out, _ := collectECS(t)

	want := []string{
		"ecs/batch/orders-api",
		"ecs/cluster/batch",
		"ecs/cluster/main",
		"ecs/main/notifications",
		"ecs/main/orders-api",
		"ecs/taskdef/notifications:7",
		"ecs/taskdef/orders-api:41",
	}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Errorf("resource ids:\n got %q\nwant %q", got, want)
	}
	if len(out.warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", out.warnings)
	}
}

// TestECSDistinguishesSameNamedServicesAcrossClusters is the reason service IDs
// are scoped by cluster.
//
// The fixture has an "orders-api" service in both the main and batch clusters —
// legal in AWS, since service names are only unique per cluster. A bare
// "ecs/orders-api" ID would silently collapse them into one node, and the loss
// would only surface much later as a service missing from the graph.
func TestECSDistinguishesSameNamedServicesAcrossClusters(t *testing.T) {
	out, _ := collectECS(t)

	main, ok := out.byID("ecs/main/orders-api")
	if !ok {
		t.Fatal("missing ecs/main/orders-api")
	}
	batch, ok := out.byID("ecs/batch/orders-api")
	if !ok {
		t.Fatal("missing ecs/batch/orders-api")
	}

	var mainSpec, batchSpec serviceSpec
	specOf(t, main, &mainSpec)
	specOf(t, batch, &batchSpec)

	if mainSpec.DesiredCount != 4 || batchSpec.DesiredCount != 1 {
		t.Errorf("the two services were conflated: main=%d batch=%d",
			mainSpec.DesiredCount, batchSpec.DesiredCount)
	}
	if main.ARN == batch.ARN {
		t.Error("both services resolved to the same ARN")
	}
}

// TestECSDescribesEachTaskDefinitionOnce guards the dedup path: both orders-api
// services run task definition 41, and describing it twice would be a wasted
// call against every account that shares definitions across clusters.
func TestECSDescribesEachTaskDefinitionOnce(t *testing.T) {
	_, tr := collectECS(t)

	if n := tr.count("ECS", "DescribeTaskDefinition"); n != 2 {
		t.Errorf("DescribeTaskDefinition called %d times, want 2 (one per distinct definition)", n)
	}
}

func TestECSNormalizesContainerDefinitions(t *testing.T) {
	out, _ := collectECS(t)

	td, ok := out.byID("ecs/taskdef/orders-api:41")
	if !ok {
		t.Fatal("missing ecs/taskdef/orders-api:41")
	}

	var spec taskDefinitionSpec
	specOf(t, td, &spec)

	if spec.TaskRoleARN != "arn:aws:iam::123456789012:role/orders-api-task" {
		t.Errorf("task role: got %q", spec.TaskRoleARN)
	}
	if len(spec.Containers) != 2 {
		t.Fatalf("want 2 containers, got %d", len(spec.Containers))
	}

	app := spec.Containers[0]
	if app.Name != "app" || !app.Essential {
		t.Errorf("first container: %+v", app)
	}
	if got := app.Env["QUEUE_URL"]; got != "https://sqs.us-east-1.amazonaws.com/123456789012/orders-events" {
		t.Errorf("QUEUE_URL: got %q", got)
	}
	if got := app.LogGroup; got != "/ecs/orders-api" {
		t.Errorf("log group: got %q", got)
	}
	if len(app.PortMappings) != 1 || app.PortMappings[0].ContainerPort != 8080 {
		t.Errorf("port mappings: %+v", app.PortMappings)
	}

	// The sidecar is non-essential; conflating that with the app container would
	// make the materializer treat its exit as a task failure.
	if spec.Containers[1].Essential {
		t.Error("otel sidecar should not be essential")
	}
}

// TestECSRecordsSecretReferencesNotValues pins Guarantee 2 at the collector
// level: a secret contributes its ARN, because that ARN is a linking signal, and
// nothing else.
func TestECSRecordsSecretReferencesNotValues(t *testing.T) {
	out, _ := collectECS(t)

	td, _ := out.byID("ecs/taskdef/orders-api:41")
	var spec taskDefinitionSpec
	specOf(t, td, &spec)

	got := spec.Containers[0].Secrets["DB_PASSWORD"]
	want := "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/orders/db-AbCdEf"
	if got != want {
		t.Errorf("DB_PASSWORD reference: got %q want %q", got, want)
	}
}

// TestECSLinksServiceToTaskDefinitionID checks the cross-reference the linker
// will follow: a service must name the inventory ID of its task definition, not
// just the ARN, so traversal never has to re-parse ARNs.
func TestECSLinksServiceToTaskDefinitionID(t *testing.T) {
	out, _ := collectECS(t)

	svc, _ := out.byID("ecs/main/orders-api")
	var spec serviceSpec
	specOf(t, svc, &spec)

	if spec.TaskDefinitionID != "ecs/taskdef/orders-api:41" {
		t.Fatalf("task definition id: got %q", spec.TaskDefinitionID)
	}
	if _, ok := out.byID(spec.TaskDefinitionID); !ok {
		t.Errorf("service points at %q, which was never emitted", spec.TaskDefinitionID)
	}
}

func TestECSRecordsProvenance(t *testing.T) {
	out, _ := collectECS(t)

	for _, tc := range []struct{ id, api string }{
		{"ecs/cluster/main", "ecs:DescribeClusters"},
		{"ecs/main/orders-api", "ecs:DescribeServices"},
		{"ecs/taskdef/orders-api:41", "ecs:DescribeTaskDefinition"},
	} {
		r, ok := out.byID(tc.id)
		if !ok {
			t.Errorf("missing %s", tc.id)
			continue
		}
		if r.Source.API != tc.api {
			t.Errorf("%s provenance: got %q want %q", tc.id, r.Source.API, tc.api)
		}
		if r.Source.CollectedAt.IsZero() {
			t.Errorf("%s has no collection timestamp", tc.id)
		}
		if len(r.Raw) == 0 {
			t.Errorf("%s kept no raw response — future linker rules need it", tc.id)
		}
	}
}

// TestECSUsesExactlyTheAllowListedOperations is the drift test ADR-0006 layer 3
// calls for, run against observed behaviour rather than a declaration.
func TestECSUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectECS(t)
	assertOpsMatchAllowList(t, "ECS", tr)
}

// TestECSSecretsNeverReachTheInventory checks the property, not the mechanism.
//
// The fixture plants three secrets in places an operator might really put them:
// a plain env var with a vendor-shaped key, a connection URL with an embedded
// password, and a command-line flag. The assertion is on the *serialized
// resource* — Spec and Raw both — because Raw is written to disk too, and a
// redaction applied only to the normalized spec would be a guarantee with a hole
// in it.
func TestECSSecretsNeverReachTheInventory(t *testing.T) {
	out, _ := collectECS(t)

	secrets := []string{"not-a-real-payments-key-7f3a9c", "hunter2", "correcthorsebattery"}
	for _, r := range out.resources {
		blob, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if bytes.Contains(blob, []byte(s)) {
				t.Errorf("%s: secret %q survived into the serialized resource", r.ID, s)
			}
		}
	}
}

// TestECSRedactionKeepsWhatTheLinkerNeeds is the counterweight: removing the
// password from DATABASE_URL must not remove the RDS hostname, which is the only
// thing in that value the linker cares about.
func TestECSRedactionKeepsWhatTheLinkerNeeds(t *testing.T) {
	out, _ := collectECS(t)

	td, _ := out.byID("ecs/taskdef/orders-api:41")
	var spec taskDefinitionSpec
	specOf(t, td, &spec)
	app := spec.Containers[0]

	if got := app.Env["DATABASE_URL"]; !strings.Contains(got, "@orders-db.cluster-abc123.us-east-1.rds.amazonaws.com:5432/orders") {
		t.Errorf("DATABASE_URL lost its host: %q", got)
	}
	if got := app.Env["TABLE_NAME"]; got != "orders" {
		t.Errorf("TABLE_NAME was altered: %q", got)
	}

	want := []string{"env:DATABASE_URL", "env:PAYMENTS_API_KEY"}
	if !reflect.DeepEqual(app.Redacted, want) {
		t.Errorf("redacted list: got %q want %q", app.Redacted, want)
	}

	notif, _ := out.byID("ecs/taskdef/notifications:7")
	var nspec taskDefinitionSpec
	specOf(t, notif, &nspec)
	if got := nspec.Containers[0].Redacted; !reflect.DeepEqual(got, []string{"command[2]"}) {
		t.Errorf("command redaction not recorded: %q", got)
	}
}
