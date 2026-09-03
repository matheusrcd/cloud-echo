package inventory

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func res(id string) Resource {
	return Resource{ID: id, Type: "ecs.service", Name: id, Region: "us-east-1", AccountID: "123456789012"}
}

// TestWriteIsDeterministic is what makes `scan --diff` mean anything.
//
// Collectors run concurrently, so emission order is a function of which AWS API
// answered first. Without a canonical order, two scans of an unchanged account
// produce different bytes and every diff is noise.
func TestWriteIsDeterministic(t *testing.T) {
	at := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)

	build := func(ids ...string) []byte {
		inv := &Inventory{AccountID: "123456789012", Region: "us-east-1", ScannedAt: at}
		for _, id := range ids {
			inv.Add(res(id))
		}
		inv.Warn(Warning{Service: "SQS", Op: "ListQueues", Kind: "access-denied", Message: "denied"})
		inv.Warn(Warning{Service: "ECS", Op: "ListClusters", Kind: "access-denied", Message: "denied"})

		var buf bytes.Buffer
		if err := inv.Write(&buf); err != nil {
			t.Fatalf("Write: %v", err)
		}
		return buf.Bytes()
	}

	a := build("ecs/main/zeta", "ecs/main/alpha", "ecs/cluster/main")
	b := build("ecs/cluster/main", "ecs/main/zeta", "ecs/main/alpha")

	if !bytes.Equal(a, b) {
		t.Errorf("same resources in a different emission order produced different bytes:\n%s\n---\n%s", a, b)
	}
}

// TestWriteRejectsDuplicateIDs turns a collector bug into an immediate, named
// failure instead of one resource silently overwriting another downstream.
func TestWriteRejectsDuplicateIDs(t *testing.T) {
	inv := &Inventory{}
	inv.Add(res("ecs/main/orders-api"))
	inv.Add(res("ecs/main/orders-api"))

	err := inv.Write(&bytes.Buffer{})
	if err == nil {
		t.Fatal("wrote an inventory with a duplicate id")
	}
	if !strings.Contains(err.Error(), "ecs/main/orders-api") {
		t.Errorf("error should name the duplicate, got: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	inv := &Inventory{
		AccountID: "123456789012",
		Region:    "us-east-1",
		ScannedAt: time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC),
	}
	inv.Add(res("ecs/main/orders-api"))

	var buf bytes.Buffer
	if err := inv.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Resources) != 1 || got.Resources[0].ID != "ecs/main/orders-api" {
		t.Errorf("resources did not survive the round trip: %+v", got.Resources)
	}
	if got.AccountID != "123456789012" {
		t.Errorf("accountId: got %q", got.AccountID)
	}
}

// TestReadRejectsUnknownFormatVersion pins the "hard error, never a silent
// upgrade" rule from docs/09-open-questions.md Q4.
func TestReadRejectsUnknownFormatVersion(t *testing.T) {
	_, err := Read(strings.NewReader(`{"formatVersion": 99, "resources": []}`))
	if err == nil {
		t.Fatal("accepted an inventory from an unknown format version")
	}
	if !strings.Contains(err.Error(), "cloud-echo scan") {
		t.Errorf("error should tell the user what to do, got: %v", err)
	}
}

func TestWarnMarksInventoryPartial(t *testing.T) {
	inv := &Inventory{}
	if inv.Partial {
		t.Error("a fresh inventory should not be partial")
	}
	inv.Warn(Warning{Service: "ECS", Kind: "access-denied"})
	if !inv.Partial {
		t.Error("a warning must mark the inventory partial — downstream stages rely on it")
	}
}
