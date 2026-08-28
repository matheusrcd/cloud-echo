// Package inventory holds the normalized resource model produced by discovery.
//
// It is the only artifact that touches AWS, and every stage downstream — linker,
// planner, materializer — works offline from it. That is what makes the linker
// testable against recorded fixtures and what makes re-planning free.
package inventory

import (
	"encoding/json"
	"time"
)

// Resource is one AWS resource, normalized.
type Resource struct {
	// ID is the stable local identifier, e.g. "ecs/main/orders-api". Stability
	// across scans matters more than brevity: an ID that changes turns every
	// re-plan into a meaningless diff and detaches user overrides from their
	// targets. See docs/09-open-questions.md Q5.
	ID string `json:"id"`

	// Type is the normalized type tag, e.g. "ecs.service", "dynamodb.table".
	Type string `json:"type"`

	ARN       string `json:"arn,omitempty"`
	Region    string `json:"region"`
	AccountID string `json:"accountId"`
	Name      string `json:"name"`

	Tags map[string]string `json:"tags,omitempty"`

	// Spec is the type-specific normalized shape the planner and materializer
	// consume.
	Spec json.RawMessage `json:"spec,omitempty"`

	// Raw is the untouched API response.
	//
	// Keeping it is what lets a new linker heuristic be developed and tested
	// against inventories captured months ago, without re-scanning anyone's
	// account. It costs disk and buys the ability to iterate on the riskiest
	// part of the system offline.
	Raw json.RawMessage `json:"raw,omitempty"`

	Source Provenance `json:"source"`
}

// Provenance records where a resource came from. The linker's evidence chains
// bottom out here: an edge that cannot name the API response field that implies
// it is a bug, not a heuristic.
type Provenance struct {
	// API is the call that produced this resource, e.g. "ecs:DescribeServices".
	API         string    `json:"api"`
	CollectedAt time.Time `json:"collectedAt"`
}

// Warning is a non-fatal problem encountered during a scan.
//
// Real accounts have partial permissions. A scanner that aborts on the first
// AccessDenied is useless, so denials become warnings and the inventory is marked
// partial — but never silently: every warning surfaces in the scan summary and is
// persisted alongside the resources.
type Warning struct {
	Service string `json:"service"`
	Op      string `json:"op,omitempty"`
	Kind    string `json:"kind"` // "access-denied", "throttled", "api-error"
	Message string `json:"message"`
}
