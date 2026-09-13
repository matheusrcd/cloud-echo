// Package linker infers what talks to what from an inventory, and classifies
// every node by the flow it sits in.
//
// It runs offline: its only input is the inventory discovery wrote, and it
// reads resources through their normalized specs (internal/inventory/spec),
// never Raw. See docs/03-linker.md for the design and the rules' contracts.
package linker

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// FormatVersion is bumped when graph.json changes incompatibly.
const FormatVersion = 1

// Kind is what an edge does. Edges point in the direction of causality: from
// the side that acts to the side that is acted on or triggered. A service that
// writes a table points at the table; a queue that triggers a function through
// an event source mapping points at the function.
type Kind string

const (
	KindInvoke  Kind = "invoke"  // synchronous call: API → function, authorizer
	KindHTTP    Kind = "http"    // synchronous HTTP call
	KindRead    Kind = "read"    // reads a data store
	KindWrite   Kind = "write"   // writes a data store
	KindConnect Kind = "connect" // opens a connection (databases, caches)
	KindPublish Kind = "publish" // hands a message to a queue or topic
	KindConsume Kind = "consume" // a queue or stream triggers its consumer

	// KindReferences says the source's configuration names the target — and
	// nothing about what it does with it. A worker holding QUEUE_URL may send
	// to the queue or poll it; a service holding TABLE_NAME may read or write.
	// Tier 2 cannot tell, and choosing publish or write would silently pick a
	// winner. When another tier states the intent for the same pair, the
	// reference becomes evidence on that edge instead (see absorbReferences).
	KindReferences Kind = "references"
)

// Synchronous reports whether the edge keeps the caller waiting. The flow
// boundary is the first asynchronous edge: a queue, a topic, a stream.
//
// A reference is decided by its target, not its kind (Graph.Synchronous):
// whatever a workload does with a queue, the queue decouples it.
func (k Kind) Synchronous() bool {
	switch k {
	case KindInvoke, KindHTTP, KindRead, KindWrite, KindConnect, KindReferences:
		return true
	}
	return false
}

// Confidence maps to a policy (docs/03-linker.md): certain and high enter a
// plan automatically, medium is flagged, low is only ever a suggestion.
type Confidence string

const (
	Certain Confidence = "certain"
	High    Confidence = "high"
	Medium  Confidence = "medium"
	Low     Confidence = "low"
)

var confidenceRank = map[Confidence]int{Low: 1, Medium: 2, High: 3, Certain: 4}

// Status says whether an edge carries traffic now.
type Status string

const (
	Active Status = ""
	// Unsettled edges are kept and traversed: the source state could not say
	// whether they are enabled (a mapping caught mid-update). Dropping them
	// would lose a real edge; asserting them silently would overclaim.
	Unsettled Status = "unsettled"
	// Disabled edges are recorded but not traversed when classifying flow.
	Disabled Status = "disabled"
)

var statusRank = map[Status]int{Disabled: 1, Unsettled: 2, Active: 3}

// Evidence names the exact API response behind an edge. An edge without
// evidence is a bug.
type Evidence struct {
	Rule   string `json:"rule"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

type Edge struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Kind       Kind       `json:"kind"`
	Confidence Confidence `json:"confidence"`
	Status     Status     `json:"status,omitempty"`
	Evidence   []Evidence `json:"evidence"`
}

// Flow is where a node sits relative to the account's entrypoints.
type Flow string

const (
	Entrypoint Flow = "entrypoint" // triggered from outside the graph
	Sync       Flow = "sync"       // on an all-synchronous path from an entrypoint
	Async      Flow = "async"      // reachable, but only across a queue/topic/stream
	Scheduled  Flow = "scheduled"  // reachable only from a schedule (needs EventBridge)
	// Unreached replaces the design's "orphan". cloud-echo cannot tell dead
	// infrastructure from a relationship it failed to infer — a worker polling
	// a queue through the SDK is invisible to Tier 1 — and "orphan" asserts
	// the first. Unreached says only what is known.
	Unreached Flow = "unreached"
)

type Node struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Flow Flow   `json:"flow"`
	// Triggers explain why a node is an entrypoint, one line per reason.
	Triggers []string `json:"triggers,omitempty"`
	// External nodes (ext/<host>) are outside the account: third parties the
	// Echo Gateway will mock.
	External bool `json:"external,omitempty"`
}

// Finding is something a rule saw but could not turn into an edge, reported
// rather than dropped: a target outside the inventory, a permission nothing
// uses, a name that fits several resources, configuration that could not be
// read. Never an invented node.
type Finding struct {
	Kind   string `json:"kind"` // unresolved | stale-permission | ambiguous | unscanned
	Node   string `json:"node"`
	Target string `json:"target"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

type Graph struct {
	FormatVersion   int    `json:"formatVersion"`
	GeneratedBy     string `json:"generatedBy,omitempty"`
	InventoryScanID string `json:"inventoryScanId,omitempty"`
	AccountID       string `json:"accountId"`
	Region          string `json:"region"`

	// Partial mirrors the inventory: a partial inventory makes a partial graph,
	// and the gaps must not read as facts about the architecture.
	Partial bool `json:"partial,omitempty"`

	Nodes    []Node    `json:"nodes"`
	Edges    []Edge    `json:"edges"`
	Findings []Finding `json:"findings,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Node returns the node with the given id.
func (g *Graph) Node(id string) (Node, bool) {
	i := sort.Search(len(g.Nodes), func(i int) bool { return g.Nodes[i].ID >= id })
	if i < len(g.Nodes) && g.Nodes[i].ID == id {
		return g.Nodes[i], true
	}
	return Node{}, false
}

// Synchronous reports whether an edge keeps its caller waiting. It is the
// kind's answer, except for a reference, which is synchronous unless it names a
// queue.
func (g *Graph) Synchronous(e Edge) bool {
	if e.Kind == KindReferences {
		n, _ := g.Node(e.To)
		return n.Type != spec.TypeSQSQueue
	}
	return e.Kind.Synchronous()
}

// Between returns every edge from one node to another, in any kind.
func (g *Graph) Between(from, to string) []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			out = append(out, e)
		}
	}
	return out
}

// Write serializes the graph deterministically.
func (g *Graph) Write(w io.Writer) error {
	g.FormatVersion = FormatVersion
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(g)
}

// Read loads a graph, rejecting formats this build does not understand.
func Read(r io.Reader) (*Graph, error) {
	var g Graph
	if err := json.NewDecoder(r).Decode(&g); err != nil {
		return nil, fmt.Errorf("decoding graph: %w", err)
	}
	if g.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("graph format version %d, this build understands %d — re-run `cloud-echo graph`",
			g.FormatVersion, FormatVersion)
	}
	return &g, nil
}

// ---------------------------------------------------------------- building

type edgeKey struct {
	from, to string
	kind     Kind
}

// builder accumulates edges from every rule and merges them: one edge per
// (from, to, kind), the highest confidence and the most active status among its
// sources, and the union of their evidence.
type builder struct {
	nodes    map[string]*Node
	edges    map[edgeKey]*Edge
	findings []Finding
	warnings []string
	// corroborations add evidence to an edge only if another rule created it.
	corroborations []corroboration
}

type corroboration struct {
	key    edgeKey
	ev     Evidence
	orElse *Finding
}

func newBuilder() *builder {
	return &builder{nodes: map[string]*Node{}, edges: map[edgeKey]*Edge{}}
}

func (b *builder) addEdge(from, to string, kind Kind, conf Confidence, status Status, ev Evidence) {
	k := edgeKey{from, to, kind}
	e, ok := b.edges[k]
	if !ok {
		e = &Edge{From: from, To: to, Kind: kind, Confidence: conf, Status: status}
		b.edges[k] = e
	} else {
		if confidenceRank[conf] > confidenceRank[e.Confidence] {
			e.Confidence = conf
		}
		if statusRank[status] > statusRank[e.Status] {
			e.Status = status
		}
	}
	e.Evidence = append(e.Evidence, ev)
}

// resolveCorroborations applies deferred evidence. A corroboration never
// creates an edge; when its edge does not exist it becomes a finding, if the
// rule supplied one.
func (b *builder) resolveCorroborations() {
	for _, c := range b.corroborations {
		if e, ok := b.edges[c.key]; ok {
			e.Evidence = append(e.Evidence, c.ev)
		} else if c.orElse != nil {
			b.findings = append(b.findings, *c.orElse)
		}
	}
}

// absorbReferences folds a reference into the edges that already say what the
// source does with the target. "The Lambda's DLQ_URL names the queue" is
// evidence for the Lambda → DLQ publish edge the dead-letter rule drew, not a
// second, vaguer edge beside it.
//
// Only same-direction edges absorb: a consumer holding its own queue's URL
// still references it, and folding that into queue → consumer would claim the
// configuration explains the consumption. Disabled edges do not absorb either,
// or a live reference would vanish into an edge that carries nothing.
//
// A low-confidence reference is a candidate from an ambiguous name, not
// evidence: attached to an edge it would read as corroboration. When a typed
// edge exists it is dropped; the ambiguous finding still records it.
func (b *builder) absorbReferences() {
	type pair struct{ from, to string }
	typedBetween := map[pair][]*Edge{}
	for k, e := range b.edges {
		if k.kind != KindReferences && e.Status != Disabled {
			typedBetween[pair{k.from, k.to}] = append(typedBetween[pair{k.from, k.to}], e)
		}
	}
	for k, ref := range b.edges {
		if k.kind != KindReferences {
			continue
		}
		typed := typedBetween[pair{k.from, k.to}]
		if len(typed) == 0 {
			continue
		}
		if ref.Confidence != Low {
			for _, e := range typed {
				if confidenceRank[ref.Confidence] > confidenceRank[e.Confidence] {
					e.Confidence = ref.Confidence
				}
				e.Evidence = append(e.Evidence, ref.Evidence...)
			}
		}
		delete(b.edges, k)
	}
}

func (b *builder) graph() *Graph {
	g := &Graph{}
	for _, n := range b.nodes {
		sort.Strings(n.Triggers)
		n.Triggers = uniq(n.Triggers)
		g.Nodes = append(g.Nodes, *n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })

	for _, e := range b.edges {
		sort.Slice(e.Evidence, func(i, j int) bool {
			a, c := e.Evidence[i], e.Evidence[j]
			if a.Rule != c.Rule {
				return a.Rule < c.Rule
			}
			if a.Source != c.Source {
				return a.Source < c.Source
			}
			return a.Detail < c.Detail
		})
		e.Evidence = uniqEvidence(e.Evidence)
		g.Edges = append(g.Edges, *e)
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		a, c := g.Edges[i], g.Edges[j]
		if a.From != c.From {
			return a.From < c.From
		}
		if a.To != c.To {
			return a.To < c.To
		}
		return a.Kind < c.Kind
	})

	sort.Slice(b.findings, func(i, j int) bool {
		a, c := b.findings[i], b.findings[j]
		if a.Node != c.Node {
			return a.Node < c.Node
		}
		if a.Target != c.Target {
			return a.Target < c.Target
		}
		return a.Rule < c.Rule
	})
	g.Findings = uniqFindings(b.findings)
	g.Warnings = uniq(sortedCopy(b.warnings))
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	return g
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func uniq(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func uniqEvidence(s []Evidence) []Evidence {
	if len(s) == 0 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func uniqFindings(s []Finding) []Finding {
	if len(s) == 0 {
		return nil
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
