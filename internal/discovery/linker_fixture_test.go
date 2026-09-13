package discovery

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

var updateLinkerFixture = flag.Bool("update-linker-fixture", false,
	"rewrite internal/linker/testdata/orders/inventory.json from the discovery fixtures")

// TestLinkerFixtureMatchesDiscovery chains the pipeline's test stages. The
// linker's "orders" golden account is exactly what every collector produces
// from the discovery fixtures, so a change to a collector's spec cannot leave the
// linker testing against a shape discovery no longer writes. Regenerate with:
//
//	go test ./internal/discovery -run TestLinkerFixtureMatchesDiscovery -update-linker-fixture
//
// then see what it did to the graph with `go test ./internal/linker`.
func TestLinkerFixtureMatchesDiscovery(t *testing.T) {
	tr := loadFixtures(t, "orders", "ecs", "sqs", "dynamodb", "lambda", "apigateway", "apigatewayv2", "iam")
	inv, err := NewRegistry().Scan(context.Background(), fixtureSession(tr), Options{GeneratedBy: "cloud-echo/fixture"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	known := map[string]bool{}
	for _, typ := range spec.Types {
		known[typ] = true
	}
	for _, r := range inv.Resources {
		if !known[r.Type] {
			t.Errorf("%s has type %q, which spec.Types does not declare", r.ID, r.Type)
		}
	}

	got := fixtureInventory(t, inv)
	path := filepath.Join("..", "linker", "testdata", "orders", "inventory.json")
	if *updateLinkerFixture {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with -update-linker-fixture", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale: discovery now produces a different inventory.\n"+
			"regenerate with: go test ./internal/discovery -run %s -update-linker-fixture", path, t.Name())
	}
}

// fixtureInventory makes an inventory stable enough to commit: fixed ids and
// timestamps, and no Raw. The linker reads specs only; a fixture without Raw
// keeps that contract honest — a rule reaching into Raw would find nothing.
func fixtureInventory(t *testing.T, inv *inventory.Inventory) []byte {
	t.Helper()
	at := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	inv.ScanID, inv.ScannedAt = "fixture", at
	for i := range inv.Resources {
		inv.Resources[i].Raw = nil
		inv.Resources[i].Source.CollectedAt = at
	}
	var buf bytes.Buffer
	if err := inv.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
