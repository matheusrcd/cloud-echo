package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

func TestWriteInventoryCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cloud-echo", "inventory.json")

	inv := &inventory.Inventory{AccountID: "123456789012", Region: "us-east-1"}
	inv.Add(inventory.Resource{ID: "ecs/main/orders-api", Type: "ecs.service"})

	if err := writeInventory(path, inv); err != nil {
		t.Fatalf("writeInventory: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening result: %v", err)
	}
	defer f.Close()

	got, err := inventory.Read(f)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(got.Resources) != 1 {
		t.Errorf("want 1 resource, got %d", len(got.Resources))
	}
}

// TestWriteInventoryLeavesPreviousFileIntactOnFailure covers the reason the write
// is staged through a temp file: a scan that fails at serialization time must not
// destroy the inventory from the last successful scan. Overwriting in place would
// leave a truncated file that the next command reads as authoritative.
func TestWriteInventoryLeavesPreviousFileIntactOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")

	good := &inventory.Inventory{AccountID: "123456789012", Region: "us-east-1"}
	good.Add(inventory.Resource{ID: "ecs/main/orders-api", Type: "ecs.service"})
	if err := writeInventory(path, good); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading seed: %v", err)
	}

	// Duplicate IDs make Write fail after the temp file is already open.
	bad := &inventory.Inventory{AccountID: "123456789012", Region: "us-east-1"}
	bad.Add(inventory.Resource{ID: "dup", Type: "ecs.service"})
	bad.Add(inventory.Resource{ID: "dup", Type: "ecs.service"})

	if err := writeInventory(path, bad); err == nil {
		t.Fatal("writeInventory accepted an invalid inventory")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("previous inventory was destroyed: %v", err)
	}
	if string(before) != string(after) {
		t.Error("a failed write modified the previous inventory")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("failed write left temp files behind: %v", entries)
	}
}
