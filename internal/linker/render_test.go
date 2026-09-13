package linker

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestTextGroupsByFlowAndExplainsUnreached(t *testing.T) {
	var buf bytes.Buffer
	WriteText(&buf, cases(t))
	out := buf.String()
	for _, want := range []string{
		"ENTRYPOINT (4)", "SYNC (5)", "ASYNC (5)", "UNREACHED (8)",
		"↳ s3.amazonaws.com may invoke it",
		"─consume→ lambda/disabled-consumer  certain [disabled]",
		// The unreached section must say it is not a verdict of disuse.
		"not a verdict that the resource is unused",
		"[stale-permission] lambda/stale-fn",
		"target group web, not in the inventory",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q", want)
		}
	}
}

// TestExplainShowsEveryPieceOfEvidence: the integration and the resource policy
// are independent sources for one edge; explain must show both.
func TestExplainShowsEveryPieceOfEvidence(t *testing.T) {
	var buf bytes.Buffer
	if err := Explain(&buf, cases(t), "apigw/api0000001", "lambda/items-fn"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"invoke · certain", "apigw.integration", "apigateway:GetResources api0000001 GET /items",
		"lambda.resource-policy", "lambda:GetPolicy items-fn"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
}

func TestExplainHelpsWithUnknownAndMissing(t *testing.T) {
	g := cases(t)
	err := Explain(&bytes.Buffer{}, g, "items-fn", "sqs/events")
	if err == nil || !strings.Contains(err.Error(), "did you mean lambda/items-fn") {
		t.Errorf("unknown node: %v", err)
	}
	var buf bytes.Buffer
	if err := Explain(&buf, g, "lambda/shared-fn", "sqs/events"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "there is one the other way: sqs/events ─consume→ lambda/shared-fn") {
		t.Errorf("reversed edge not pointed out:\n%s", buf.String())
	}
}

// TestMermaidIsWellFormed checks the structure rather than bytes: one node
// line per node, one edge line per edge, and only ids that were declared.
func TestMermaidIsWellFormed(t *testing.T) {
	g := cases(t)
	var buf bytes.Buffer
	WriteMermaid(&buf, g)
	out := buf.String()
	if !strings.HasPrefix(out, "flowchart LR\n") {
		t.Fatal("not a flowchart")
	}
	nodeLine := regexp.MustCompile(`(?m)^  (n\d+)[\[\(].*:::\w+$`)
	edgeLine := regexp.MustCompile(`(?m)^  (n\d+) (-->|-\.->|-\.-x)\|[^|]+\| (n\d+)$`)
	declared := map[string]bool{}
	for _, m := range nodeLine.FindAllStringSubmatch(out, -1) {
		declared[m[1]] = true
	}
	if len(declared) != len(g.Nodes) {
		t.Errorf("want %d node lines, got %d", len(g.Nodes), len(declared))
	}
	edges := edgeLine.FindAllStringSubmatch(out, -1)
	if len(edges) != len(g.Edges) {
		t.Errorf("want %d edge lines, got %d", len(g.Edges), len(edges))
	}
	for _, e := range edges {
		if !declared[e[1]] || !declared[e[3]] {
			t.Errorf("edge between undeclared nodes: %s", e[0])
		}
	}
	if !strings.Contains(out, "-.-x|consume, disabled|") || !strings.Contains(out, "|consume, unsettled|") {
		t.Error("disabled and unsettled edges are not distinguishable")
	}
}
