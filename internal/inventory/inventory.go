package inventory

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// FormatVersion is bumped when the on-disk shape changes incompatibly. A newer
// binary reading an older inventory is a hard error, never a silent upgrade.
const FormatVersion = 1

// Inventory is the persisted result of one scan.
type Inventory struct {
	FormatVersion int    `json:"formatVersion"`
	ScanID        string `json:"scanId"`
	GeneratedBy   string `json:"generatedBy"`

	AccountID string    `json:"accountId"`
	Region    string    `json:"region"`
	ScannedAt time.Time `json:"scannedAt"`

	// Partial is true when any collector reported a warning, meaning the graph
	// built from this inventory may be missing edges. Downstream stages surface
	// it rather than pretending the picture is complete.
	Partial  bool      `json:"partial"`
	Warnings []Warning `json:"warnings,omitempty"`

	Resources []Resource `json:"resources"`
}

// Add appends a resource. Ordering is fixed later by Normalize.
func (inv *Inventory) Add(r Resource) { inv.Resources = append(inv.Resources, r) }

// Warn records a non-fatal problem and marks the inventory partial.
func (inv *Inventory) Warn(w Warning) {
	inv.Warnings = append(inv.Warnings, w)
	inv.Partial = true
}

// Normalize sorts resources and warnings into a deterministic order.
//
// Discovery runs collectors concurrently, so emission order is a function of
// which API responded first. Without this, two scans of an unchanged account
// produce different files and `scan --diff` reports noise. Determinism is not a
// nicety here: it is what makes diffing meaningful.
func (inv *Inventory) Normalize() {
	sort.SliceStable(inv.Resources, func(i, j int) bool {
		return inv.Resources[i].ID < inv.Resources[j].ID
	})
	sort.SliceStable(inv.Warnings, func(i, j int) bool {
		a, b := inv.Warnings[i], inv.Warnings[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Op != b.Op {
			return a.Op < b.Op
		}
		return a.Message < b.Message
	})
}

// Duplicates returns IDs claimed by more than one resource.
//
// A duplicate ID is a collector bug that would otherwise cause one resource to
// silently overwrite another downstream — the kind of failure that shows up much
// later as a mysteriously missing node in the graph.
func (inv *Inventory) Duplicates() []string {
	seen := make(map[string]int, len(inv.Resources))
	for _, r := range inv.Resources {
		seen[r.ID]++
	}
	var dup []string
	for id, n := range seen {
		if n > 1 {
			dup = append(dup, id)
		}
	}
	sort.Strings(dup)
	return dup
}

// Write serializes the inventory deterministically.
func (inv *Inventory) Write(w io.Writer) error {
	inv.Normalize()
	if dup := inv.Duplicates(); len(dup) > 0 {
		return fmt.Errorf("refusing to write inventory: duplicate resource ids %v", dup)
	}
	inv.FormatVersion = FormatVersion

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(inv)
}

// Read loads an inventory, rejecting formats this binary does not understand.
func Read(r io.Reader) (*Inventory, error) {
	var inv Inventory
	if err := json.NewDecoder(r).Decode(&inv); err != nil {
		return nil, fmt.Errorf("decoding inventory: %w", err)
	}
	if inv.FormatVersion != FormatVersion {
		return nil, fmt.Errorf(
			"inventory format version %d, this build understands %d — re-run `cloud-echo scan`",
			inv.FormatVersion, FormatVersion)
	}
	return &inv, nil
}

// ByType returns the resources of one normalized type, in ID order.
func (inv *Inventory) ByType(typ string) []Resource {
	var out []Resource
	for _, r := range inv.Resources {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
