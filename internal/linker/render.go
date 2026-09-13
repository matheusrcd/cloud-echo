package linker

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

var flowOrder = []Flow{Entrypoint, Sync, Async, Scheduled, Unreached}

var flowNote = map[Flow]string{
	Entrypoint: "triggered from outside the graph",
	Sync:       "on a synchronous path from an entrypoint — the request path",
	Async:      "reached only across a queue or stream — runs beside the request path",
	Scheduled:  "reached only from a schedule",
	Unreached: "no path from an entrypoint was found. The linker reads what the account declares, configures and permits, " +
		"not its traffic, so a caller it cannot see — a schedule, a client outside AWS, a grant too broad to link — " +
		"leaves this open; it is not a verdict that the resource is unused",
}

// WriteText renders the graph for a terminal: nodes grouped by flow, each with
// its outgoing edges, then findings.
func WriteText(w io.Writer, g *Graph) {
	out := map[string][]Edge{}
	for _, e := range g.Edges {
		out[e.From] = append(out[e.From], e)
	}
	fmt.Fprintf(w, "graph: %d nodes, %d edges, %d findings\n", len(g.Nodes), len(g.Edges), len(g.Findings))
	if g.Partial {
		fmt.Fprintln(w, "the inventory is partial: edges may be missing where the scan could not read")
	}

	for _, f := range flowOrder {
		var nodes []Node
		for _, n := range g.Nodes {
			if n.Flow == f {
				nodes = append(nodes, n)
			}
		}
		if len(nodes) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d) — %s\n", strings.ToUpper(string(f)), len(nodes), flowNote[f])
		for _, n := range nodes {
			label := n.ID
			if n.Name != "" && !strings.HasSuffix(n.ID, "/"+n.Name) {
				label += "  " + n.Name
			}
			fmt.Fprintf(w, "  %s\n", label)
			for _, t := range n.Triggers {
				fmt.Fprintf(w, "      ↳ %s\n", t)
			}
			for _, e := range out[n.ID] {
				fmt.Fprintf(w, "      ─%s→ %s  %s%s%s\n", e.Kind, e.To, e.Confidence, statusSuffix(e.Status), candidateSuffix(e))
			}
		}
	}

	writeFindings(w, g.Findings)
	for _, warn := range g.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

// candidateSuffix marks a low-confidence edge for what it is: a suggestion,
// followed by nothing and never planned unless the user accepts it.
func candidateSuffix(e Edge) string {
	if e.Confidence == Low {
		return "  (candidate)"
	}
	return ""
}

// writeFindings groups findings that say the same thing about the same target:
// twelve services sharing one task definition are one ElastiCache endpoint the
// linker could not resolve, not twelve problems. graph.json keeps every one.
func writeFindings(w io.Writer, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	type key struct{ kind, target, detail string }
	var order []key
	nodes := map[key][]string{}
	for _, f := range findings {
		k := key{f.Kind, f.Target, f.Detail}
		if _, ok := nodes[k]; !ok {
			order = append(order, k)
		}
		nodes[k] = append(nodes[k], f.Node)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].kind != order[j].kind {
			return order[i].kind < order[j].kind
		}
		return order[i].target < order[j].target
	})
	fmt.Fprintf(w, "\nFINDINGS (%d)\n", len(findings))
	for _, k := range order {
		ns := nodes[k]
		from := strings.Join(ns, ", ")
		if len(ns) > 4 {
			from = fmt.Sprintf("%s and %d more", strings.Join(ns[:3], ", "), len(ns)-3)
		}
		fmt.Fprintf(w, "  [%s] %s — %s\n", k.kind, from, k.detail)
		if k.target != "" && !strings.Contains(k.detail, k.target) {
			fmt.Fprintf(w, "      target: %s\n", k.target)
		}
	}
}

func statusSuffix(s Status) string {
	if s == Active {
		return ""
	}
	return " [" + string(s) + "]"
}

// Explain prints every edge between two nodes with its evidence — the audit
// trail behind a claim that one thing talks to another.
func Explain(w io.Writer, g *Graph, from, to string) error {
	for _, id := range []string{from, to} {
		if _, ok := g.Node(id); !ok {
			return fmt.Errorf("no node %q%s", id, suggest(g, id))
		}
	}
	edges := g.Between(from, to)
	if len(edges) == 0 {
		fmt.Fprintf(w, "no edge from %s to %s\n", from, to)
		if back := g.Between(to, from); len(back) > 0 {
			fmt.Fprintf(w, "there is one the other way: %s ─%s→ %s\n", to, back[0].Kind, from)
		}
		return nil
	}
	fmt.Fprintf(w, "%s → %s\n", from, to)
	for _, e := range edges {
		fmt.Fprintf(w, "  %s · %s%s\n", e.Kind, e.Confidence, statusSuffix(e.Status))
		for _, ev := range e.Evidence {
			fmt.Fprintf(w, "    %s\n      source: %s\n      %s\n", ev.Rule, ev.Source, ev.Detail)
		}
	}
	return nil
}

// suggest names the nodes whose id contains the unknown one, so a typo or a
// bare name ("orders-api") leads somewhere.
func suggest(g *Graph, id string) string {
	var hits []string
	for _, n := range g.Nodes {
		if strings.Contains(n.ID, id) || strings.Contains(id, n.Name) && n.Name != "" {
			hits = append(hits, n.ID)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 5 {
		hits = hits[:5]
	}
	return "; did you mean " + strings.Join(hits, ", ") + "?"
}

// WriteMermaid renders a flowchart GitHub and most doc tools display natively.
// Asynchronous edges are dashed, disabled ones crossed out, and nodes are
// coloured by flow.
func WriteMermaid(w io.Writer, g *Graph) {
	ids := map[string]string{}
	for i, n := range g.Nodes {
		ids[n.ID] = fmt.Sprintf("n%d", i)
	}
	fmt.Fprintln(w, "flowchart LR")
	for _, n := range g.Nodes {
		label := mermaidEscape(n.ID)
		if n.Name != "" && !strings.HasSuffix(n.ID, "/"+n.Name) {
			label += "<br/>" + mermaidEscape(n.Name)
		}
		open, close := "[\"", "\"]"
		switch {
		case n.External:
			open, close = "([\"", "\"])"
		case strings.HasPrefix(n.Type, "sqs."):
			open, close = "[/\"", "\"/]"
		case strings.HasPrefix(n.Type, "elbv2."):
			open, close = "{{\"", "\"}}"
		case strings.HasPrefix(n.Type, "dynamodb."), strings.HasPrefix(n.Type, "rds."), strings.HasPrefix(n.Type, "elasticache."):
			open, close = "[(\"", "\")]"
		}
		class := string(n.Flow)
		if n.External {
			class = "external"
		}
		fmt.Fprintf(w, "  %s%s%s%s:::%s\n", ids[n.ID], open, label, close, class)
	}
	for _, e := range g.Edges {
		label := string(e.Kind)
		if e.Confidence == Medium || e.Confidence == Low {
			label += ", " + string(e.Confidence)
		}
		arrow := "-->"
		if !g.Synchronous(e) {
			arrow = "-.->"
		}
		switch e.Status {
		case Disabled:
			arrow, label = "-.-x", label+", disabled"
		case Unsettled:
			label += ", unsettled"
		}
		fmt.Fprintf(w, "  %s %s|%s| %s\n", ids[e.From], arrow, label, ids[e.To])
	}
	classes := map[string]string{
		"entrypoint": "fill:#e3f2fd,stroke:#1565c0",
		"sync":       "fill:#e8f5e9,stroke:#2e7d32",
		"async":      "fill:#fff8e1,stroke:#f9a825",
		"scheduled":  "fill:#f3e5f5,stroke:#6a1b9a",
		"unreached":  "fill:#f5f5f5,stroke:#9e9e9e,color:#616161",
		"external":   "fill:#fce4ec,stroke:#ad1457",
	}
	names := make([]string, 0, len(classes))
	for k := range classes {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(w, "  classDef %s %s\n", k, classes[k])
	}
}

func mermaidEscape(s string) string {
	return strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;").Replace(s)
}
