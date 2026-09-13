package linker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// nodeTypes are the resource types that become graph nodes: things that run or
// hold data. Task definitions, roles, policies and clusters are configuration
// and identity — an ECS service carries its task definition's configuration,
// and rules read it through the service — so they are not nodes.
var nodeTypes = map[string]bool{
	spec.TypeECSService:     true,
	spec.TypeLambdaFunction: true,
	spec.TypeSQSQueue:       true,
	spec.TypeDynamoDBTable:  true,
	spec.TypeRESTAPI:        true,
	spec.TypeHTTPAPI:        true,
	spec.TypeWebSocketAPI:   true,
	spec.TypeRDSInstance:    true,
	spec.TypeRDSCluster:     true,
}

// Rule turns what an inventory declares into edges, triggers and findings.
// Each rule lives in its own rule_*.go file and ships with golden fixtures,
// including a negative case (docs/03-linker.md).
type Rule interface {
	Name() string
	Tier() int
	Apply(c *Context)
}

// Tier1 rules read relationships the account states outright.
func Tier1() []Rule {
	return []Rule{
		eventSourceMappingRule{},
		redriveRule{},
		deadLetterRule{},
		apiIntegrationRule{},
		apiAuthorizerRule{},
		lambdaResourcePolicyRule{},
		apiEntrypointRule{},
		loadBalancerRule{},
	}
}

// Tier2 rules read configuration values: what a workload's env vars, command
// line and stage variables name.
func Tier2() []Rule {
	return []Rule{configValueRule{}}
}

// Tier3 rules read what workloads' roles permit: the only tier that says what
// a workload does with what it names.
func Tier3() []Rule {
	return []Rule{iamPolicyRule{}}
}

// Rules is every implemented rule, in tier order.
func Rules() []Rule {
	return append(append(Tier1(), Tier2()...), Tier3()...)
}

// Options configures a link run.
type Options struct {
	GeneratedBy string
	Rules       []Rule // defaults to Rules()
}

// Link builds the graph for an inventory.
func Link(inv *inventory.Inventory, opts Options) *Graph {
	rules := opts.Rules
	if rules == nil {
		rules = Rules()
	}
	b := newBuilder()
	for _, r := range inv.Resources {
		if nodeTypes[r.Type] {
			b.nodes[r.ID] = &Node{ID: r.ID, Type: r.Type, Name: r.Name}
		}
	}
	c := &Context{inv: inv, b: b}
	for _, r := range rules {
		c.rule = r.Name()
		r.Apply(c)
	}
	b.resolveCorroborations()
	b.resolvePermissions()
	b.absorbReferences()
	b.checkUnpermitted()

	g := b.graph()
	classify(g)
	g.GeneratedBy = opts.GeneratedBy
	g.InventoryScanID = inv.ScanID
	g.AccountID = inv.AccountID
	g.Region = inv.Region
	g.Partial = inv.Partial
	return g
}

// Context is what a rule sees: typed access to the inventory, and the only
// ways to affect the graph.
type Context struct {
	inv  *inventory.Inventory
	b    *builder
	rule string
	byID map[string]int // resource index by id, built on first lookup
}

// Each calls fn for every resource of one type, with its spec decoded into v's
// type. A spec that does not decode is a collector bug; it is reported as a
// warning and skipped rather than aborting the whole graph.
func Each[T any](c *Context, typ string, fn func(r inventory.Resource, s *T)) {
	for _, r := range c.inv.Resources {
		if r.Type != typ {
			continue
		}
		var s T
		if err := json.Unmarshal(r.Spec, &s); err != nil {
			c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: cannot decode %s spec: %v", c.rule, r.ID, err))
			continue
		}
		fn(r, &s)
	}
}

// HasNode reports whether an id is a node of this graph.
func (c *Context) HasNode(id string) bool { _, ok := c.b.nodes[id]; return ok }

// Edge records an edge between two existing nodes. When either end is not a
// node — outside the inventory, or a type that is not a node — it records an
// unresolved finding instead: an edge to nowhere would look real and dangle.
func (c *Context) Edge(from, to string, kind Kind, conf Confidence, status Status, source, detail string) {
	switch {
	case !c.HasNode(from):
		c.Unresolved(to, from, fmt.Sprintf("%s (source is not in the inventory)", detail))
	case !c.HasNode(to):
		c.Unresolved(from, to, fmt.Sprintf("%s (target is not in the inventory)", detail))
	default:
		c.b.addEdge(from, to, kind, conf, status, Evidence{Rule: c.rule, Source: source, Detail: detail})
	}
}

// External returns the node for a third-party HTTP endpoint, creating it on
// first use. The id is derived from the host alone, so the same service reached
// from an API Gateway integration and from an env var (Tier 2) is one node.
func (c *Context) External(rawURL string) (string, bool) {
	id := ExternalID(rawURL)
	if id == "" {
		return "", false
	}
	if _, ok := c.b.nodes[id]; !ok {
		host := strings.TrimPrefix(id, "ext/")
		c.b.nodes[id] = &Node{ID: id, Type: "external.http", Name: host, External: true}
	}
	return id, true
}

// ExternalID maps a URL to ext/<host>, keeping a non-default port. Empty for
// anything that is not an absolute http(s) URL.
func ExternalID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && !(u.Scheme == "https" && p == "443") && !(u.Scheme == "http" && p == "80") {
		host += ":" + p
	}
	return "ext/" + host
}

// Trigger marks a node as reachable from outside the graph, with the reason.
func (c *Context) Trigger(id, reason string) {
	if n, ok := c.b.nodes[id]; ok {
		n.Triggers = append(n.Triggers, reason)
	}
}

// Corroborate adds evidence to an edge another rule created, and never creates
// one. If the edge does not exist and orElse is set, it becomes a finding.
func (c *Context) Corroborate(from, to string, kind Kind, source, detail string, orElse *Finding) {
	if orElse != nil {
		orElse.Rule = c.rule
	}
	c.b.corroborations = append(c.b.corroborations, corroboration{
		key: edgeKey{from, to, kind}, ev: Evidence{Rule: c.rule, Source: source, Detail: detail}, orElse: orElse,
	})
}

// Permit records what a workload's role allows: an edge when no rule drew one,
// and otherwise evidence and confidence for the edge that exists — never its
// status. Both ends must be nodes.
func (c *Context) Permit(from, to string, kind Kind, conf Confidence, source, detail string) {
	if !c.HasNode(from) || !c.HasNode(to) {
		c.Unresolved(from, to, detail+" (not a node)")
		return
	}
	c.b.permissions = append(c.b.permissions, permission{
		key: edgeKey{from, to, kind}, conf: conf, ev: Evidence{Rule: c.rule, Source: source, Detail: detail},
	})
}

// Unresolved records a reference a rule could not turn into an edge.
func (c *Context) Unresolved(node, target, detail string) {
	c.Finding("unresolved", node, target, detail)
}

// Finding records something a rule saw and could not turn into an edge.
func (c *Context) Finding(kind, node, target, detail string) {
	c.b.findings = append(c.b.findings, Finding{Kind: kind, Node: node, Target: target, Rule: c.rule, Detail: detail})
}

// ---------------------------------------------------------------- flow

// classify assigns every node its flow.
//
// The design once said a node is async if "first reached" across a queue. That
// depends on traversal order: a table written by the request path and by a
// worker would flip between runs. The rule here is order-free: a node is sync
// if *any* path from an entrypoint to it is all synchronous; otherwise async if
// it is reachable at all. Disabled edges carry nothing and are not followed.
//
// Neither are low-confidence edges. Low is a suggestion the user has not
// accepted (docs/03-linker.md, the confidence policy), and a flow resting on one
// would put a service on the request path because LOG_LEVEL=info happens to be
// the name of a queue.
func classify(g *Graph) {
	out := map[string][]Edge{}
	for _, e := range g.Edges {
		if e.Status == Disabled || e.Confidence == Low {
			continue
		}
		out[e.From] = append(out[e.From], e)
	}

	var entry []string
	for _, n := range g.Nodes {
		if len(n.Triggers) > 0 {
			entry = append(entry, n.ID)
		}
	}
	syncSet := reach(entry, out, g.Synchronous)
	anySet := reach(entry, out, func(Edge) bool { return true })

	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch {
		case len(n.Triggers) > 0:
			n.Flow = Entrypoint
		case syncSet[n.ID]:
			n.Flow = Sync
		case anySet[n.ID]:
			n.Flow = Async
		default:
			n.Flow = Unreached
		}
	}
}

func reach(from []string, out map[string][]Edge, follow func(Edge) bool) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), from...)
	for _, id := range from {
		seen[id] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range out[id] {
			if follow(e) && !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return seen
}
