package linker

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

var update = flag.Bool("update", false, "rewrite testdata/*/expected-graph.json")

// TestGolden links every fixture account and compares the result byte for byte.
//
// The accounts are complementary: "orders" is what discovery produces from its
// own fixtures (kept in sync by a test in discovery), "tier1-cases" and
// "tier2-cases" are written by hand so every rule and pattern has a case and a
// negative case, and "real-m1" is a real account's inventory, sanitized.
// Regenerate with -update, and read the diff: a changed expected-graph.json is a
// changed claim about somebody's architecture.
func TestGolden(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join("testdata", "*"))
	if len(dirs) < 3 {
		t.Errorf("want at least three fixture accounts (M1 exit criterion), have %d", len(dirs))
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			g := linkFixture(t, dir)
			var buf bytes.Buffer
			if err := g.Write(&buf); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "expected-graph.json")
			if *update {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — generate with: go test ./internal/linker -update", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("%s changed; review with `git diff` after: go test ./internal/linker -update", path)
			}
		})
	}
}

func linkFixture(t *testing.T, dir string) *Graph {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inv, err := inventory.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return Link(inv, Options{GeneratedBy: "cloud-echo/golden"})
}
