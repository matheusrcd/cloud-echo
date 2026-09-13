package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matheusrcd/cloud-echo/internal/linker"
)

var casesInventory = filepath.Join("..", "..", "internal", "linker", "testdata", "tier1-cases", "inventory.json")

func TestGraphWritesTheArtifactAndPrints(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "graph.json")
	var stdout, stderr bytes.Buffer
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := graphCmd(&stdout, &stderr, []string{"--inventory", casesInventory, "--out", out}, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "ENTRYPOINT (4)") {
		t.Errorf("text output: %s", stdout.String())
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("graph.json not written: %v", err)
	}
	defer f.Close()
	if g, err := linker.Read(f); err != nil || len(g.Nodes) != 22 {
		t.Errorf("graph.json unreadable or wrong: %v", err)
	}
	if strings.Contains(stderr.String(), "refreshes it") {
		t.Errorf("a fresh inventory was reported stale: %s", stderr.String())
	}
}

// TestGraphSaysWhenTheInventoryIsStale — and does not rescan: a silent call to
// a production account is a surprise.
func TestGraphSaysWhenTheInventoryIsStale(t *testing.T) {
	var stderr bytes.Buffer
	later := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	args := []string{"--inventory", casesInventory, "--out", filepath.Join(t.TempDir(), "g.json"), "--format", "json"}
	if err := graphCmd(&bytes.Buffer{}, &stderr, args, later); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "`cloud-echo scan` refreshes it") {
		t.Errorf("stale inventory not flagged: %q", stderr.String())
	}
}

func TestGraphArgumentErrors(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"no inventory":     {"--inventory", filepath.Join(dir, "missing.json")},
		"explain one node": {"--inventory", casesInventory, "--out", filepath.Join(dir, "g.json"), "--explain", "a"},
		"unknown format":   {"--inventory", casesInventory, "--out", filepath.Join(dir, "g.json"), "--format", "svg"},
	} {
		if err := graphCmd(&bytes.Buffer{}, &bytes.Buffer{}, args, time.Now()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	err := graphCmd(&bytes.Buffer{}, &bytes.Buffer{}, []string{"--inventory", filepath.Join(dir, "missing.json")}, time.Now())
	if !strings.Contains(err.Error(), "run `cloud-echo scan` first") {
		t.Errorf("missing inventory should say what to do: %v", err)
	}
}
