package discovery

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// TestScanProducesAWritableInventory walks the whole discovery stage: registry →
// collector → emitter → normalize → serialize. It is the test that would catch a
// break between two pieces that each pass their own unit tests.
func TestScanProducesAWritableInventory(t *testing.T) {
	tr := loadFixture(t, "orders", "ecs")
	reg := &Registry{collectors: []Collector{&ECS{}}}

	inv, err := reg.Scan(context.Background(), fixtureSession(tr), Options{
		Concurrency: 4,
		GeneratedBy: "cloud-echo/test",
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if inv.AccountID != "123456789012" || inv.Region != "us-east-1" {
		t.Errorf("provenance: account=%q region=%q", inv.AccountID, inv.Region)
	}
	if inv.Partial {
		t.Errorf("a clean scan should not be partial: %+v", inv.Warnings)
	}
	if len(inv.ByType("ecs.service")) != 3 {
		t.Errorf("want 3 services, got %d", len(inv.ByType("ecs.service")))
	}
	if len(inv.ByType("ecs.taskdefinition")) != 2 {
		t.Errorf("want 2 task definitions, got %d", len(inv.ByType("ecs.taskdefinition")))
	}

	var buf bytes.Buffer
	if err := inv.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := inventory.Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Resources) != len(inv.Resources) {
		t.Errorf("round trip lost resources: %d → %d", len(inv.Resources), len(got.Resources))
	}
}

// TestScanSurvivesACollectorThatFails checks the isolation the scan runner
// promises: losing one service must not cost the user the services that already
// succeeded.
func TestScanSurvivesACollectorThatFails(t *testing.T) {
	tr := loadFixture(t, "orders", "ecs")
	reg := &Registry{collectors: []Collector{&ECS{}, exploding{}}}

	inv, err := reg.Scan(context.Background(), fixtureSession(tr), Options{})
	if err != nil {
		t.Fatalf("one failing collector aborted the scan: %v", err)
	}

	if len(inv.ByType("ecs.service")) != 3 {
		t.Errorf("ECS results were lost: got %d services", len(inv.ByType("ecs.service")))
	}
	if !inv.Partial {
		t.Error("a failed collector must mark the inventory partial")
	}

	var found bool
	for _, w := range inv.Warnings {
		if w.Service == "Exploding" && strings.Contains(w.Message, "boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure was swallowed without a warning: %+v", inv.Warnings)
	}
}

// TestScanRefusesDuplicateIDsAcrossCollectors catches an ID-scheme collision
// between two collectors, which is a bug no single collector's tests can see.
func TestScanRefusesDuplicateIDsAcrossCollectors(t *testing.T) {
	reg := &Registry{collectors: []Collector{
		staticCollector{"A", "ecs/main/orders-api"},
		staticCollector{"B", "ecs/main/orders-api"},
	}}

	_, err := reg.Scan(context.Background(), fixtureSession(loadFixture(t, "orders", "ecs")), Options{})
	if err == nil {
		t.Fatal("scan accepted two resources claiming the same id")
	}
	if !strings.Contains(err.Error(), "ecs/main/orders-api") {
		t.Errorf("error should name the collision, got: %v", err)
	}
}

// exploding stands in for a collector that hits an unrecoverable API error.
type exploding struct{}

func (exploding) Service() string { return "Exploding" }
func (exploding) Collect(context.Context, *awsx.Session, Emitter) error {
	return errors.New("boom")
}

// staticCollector emits one resource with a fixed id, for collision testing.
type staticCollector struct {
	service string
	id      string
}

func (s staticCollector) Service() string { return s.service }
func (s staticCollector) Collect(_ context.Context, _ *awsx.Session, out Emitter) error {
	out.Emit(inventory.Resource{ID: s.id, Type: "ecs.service", Name: s.id})
	return nil
}

// TestScanAcrossCollectorsProducesTheLinkerFixture drives every registered
// collector against one account, which is the input the linker will consume.
//
// The assertions here are about the *shape* the linker depends on, not about
// collector internals — those have their own tests. If this breaks, the linker's
// golden fixtures break with it.
func TestScanAcrossCollectorsProducesTheLinkerFixture(t *testing.T) {
	tr := loadFixtures(t, "orders", "ecs", "sqs", "dynamodb")
	reg := &Registry{collectors: []Collector{&ECS{}, &SQS{}, &DynamoDB{}}}

	inv, err := reg.Scan(context.Background(), fixtureSession(tr), Options{Concurrency: 4})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if inv.Partial {
		t.Fatalf("clean scan reported partial: %+v", inv.Warnings)
	}

	counts := map[string]int{
		"ecs.cluster":        2,
		"ecs.service":        3,
		"ecs.taskdefinition": 2,
		"sqs.queue":          4,
		"dynamodb.table":     2,
	}
	for typ, want := range counts {
		if got := len(inv.ByType(typ)); got != want {
			t.Errorf("%s: got %d want %d", typ, got, want)
		}
	}
}

// TestBareNameReferenceIsAmbiguous locks in a problem the linker must solve
// rather than guess at.
//
// The orders-api task definition sets TABLE_NAME=orders. The account contains a
// DynamoDB table named "orders" *and* an SQS queue named "orders" — legal, and
// not unusual. Tier 2 config scanning matches the bare string against both.
//
// docs/03-linker.md is explicit that ambiguity must downgrade confidence and emit
// candidates, never silently pick a winner. This test exists so that rule has a
// concrete failing case waiting for it: if a future linker resolves TABLE_NAME to
// exactly one resource without recording the other candidate, it is guessing.
//
// The env var *name* is a further signal (TABLE_NAME suggests a table), but it is
// a heuristic on top of an ambiguity, not a resolution of it.
func TestBareNameReferenceIsAmbiguous(t *testing.T) {
	tr := loadFixtures(t, "orders", "ecs", "sqs", "dynamodb")
	reg := &Registry{collectors: []Collector{&ECS{}, &SQS{}, &DynamoDB{}}}

	inv, err := reg.Scan(context.Background(), fixtureSession(tr), Options{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var envValue string
	for _, r := range inv.ByType("ecs.taskdefinition") {
		var spec taskDefinitionSpec
		specOf(t, r, &spec)
		for _, c := range spec.Containers {
			if v, ok := c.Env["TABLE_NAME"]; ok {
				envValue = v
			}
		}
	}
	if envValue != "orders" {
		t.Fatalf("fixture no longer sets TABLE_NAME=orders (got %q) — "+
			"the ambiguity case this test guards has been removed", envValue)
	}

	var matches []string
	for _, r := range inv.Resources {
		if r.Name == envValue {
			matches = append(matches, r.ID)
		}
	}
	sort.Strings(matches)

	want := []string{"ddb/orders", "sqs/orders"}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("the ambiguity this fixture exists to create is gone:\n got %q\nwant %q",
			matches, want)
	}
}
