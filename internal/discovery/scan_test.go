package discovery

import (
	"bytes"
	"context"
	"errors"
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
