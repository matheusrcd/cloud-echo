package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCollectorDegradesOnAccessDenied is rule 3 of the Collector contract.
//
// Real accounts hand out partial permissions, and a scanner that aborts on the
// first denial is useless in exactly the organisations this tool is aimed at. The
// scan must keep everything it was allowed to see, and it must say out loud what
// it could not.
func TestCollectorDegradesOnAccessDenied(t *testing.T) {
	tr := loadFixture(t, "partial-denied", "ecs")
	out := &captureEmitter{}

	if err := (&ECS{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("a denied DescribeTaskDefinition aborted the whole collector: %v", err)
	}

	// Everything readable survived.
	for _, id := range []string{"ecs/cluster/main", "ecs/main/orders-api"} {
		if _, ok := out.byID(id); !ok {
			t.Errorf("lost %s to an unrelated denial", id)
		}
	}
	// The unreadable part did not turn into a silently empty resource.
	if _, ok := out.byID("ecs/taskdef/orders-api:41"); ok {
		t.Error("emitted a task definition the API refused to return")
	}

	if len(out.warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %+v", len(out.warnings), out.warnings)
	}
	w := out.warnings[0]
	if w.Kind != "access-denied" {
		t.Errorf("warning kind: got %q want %q", w.Kind, "access-denied")
	}
	if w.Op != "DescribeTaskDefinition" || w.Service != "ECS" {
		t.Errorf("warning does not identify the failing call: %+v", w)
	}
}

// TestCancellationIsNotAWarning separates "you may not read this" from "stop
// reading". Recording a Ctrl-C as an access denial would tell users their
// permissions are wrong when they are fine.
func TestCancellationIsNotAWarning(t *testing.T) {
	tr := loadFixture(t, "orders", "ecs")
	out := &captureEmitter{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&ECS{}).Collect(ctx, fixtureSession(tr), out)
	if err == nil {
		t.Fatal("a cancelled context should surface as an error, not a partial result")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	for _, w := range out.warnings {
		if w.Kind == "access-denied" {
			t.Errorf("cancellation was misreported as a permissions problem: %+v", w)
		}
	}
}

func TestWarnOrFailPropagatesRealErrors(t *testing.T) {
	out := &captureEmitter{}

	err := warnOrFail(out, "ECS", "ListClusters", errors.New("connection reset by peer"))
	if err == nil {
		t.Fatal("a network failure was swallowed as a warning")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("unexpected error: %v", err)
	}
	if len(out.warnings) != 0 {
		t.Errorf("a hard failure should not also produce a warning: %+v", out.warnings)
	}
}
