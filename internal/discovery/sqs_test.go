package discovery

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func collectSQS(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "sqs")
	out := &captureEmitter{}
	if err := (&SQS{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func TestSQSCollectsExpectedQueues(t *testing.T) {
	out, _ := collectSQS(t)

	want := []string{"sqs/notifications", "sqs/orders", "sqs/orders-events", "sqs/orders-events-dlq"}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Errorf("queue ids:\n got %q\nwant %q", got, want)
	}
	if len(out.warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", out.warnings)
	}
}

// TestSQSParsesRedrivePolicy covers the collector's one piece of real parsing.
//
// AWS returns the redrive policy as a JSON document embedded in a string inside
// an attribute map. Leaving it as an opaque string would push that parsing into
// the linker, where a failure would silently cost a Tier-1 edge — the highest
// confidence edge kind there is, and one that needs no inference at all.
func TestSQSParsesRedrivePolicy(t *testing.T) {
	out, _ := collectSQS(t)

	q, ok := out.byID("sqs/orders-events")
	if !ok {
		t.Fatal("missing sqs/orders-events")
	}
	var spec queueSpec
	specOf(t, q, &spec)

	if spec.Redrive == nil {
		t.Fatal("redrive policy was not parsed")
	}
	if spec.Redrive.MaxReceive != 5 {
		t.Errorf("maxReceiveCount: got %d want 5", spec.Redrive.MaxReceive)
	}
	if spec.Redrive.TargetID != "sqs/orders-events-dlq" {
		t.Fatalf("DLQ inventory id: got %q", spec.Redrive.TargetID)
	}
	if _, ok := out.byID(spec.Redrive.TargetID); !ok {
		t.Errorf("redrive points at %q, which was never emitted", spec.Redrive.TargetID)
	}
}

func TestSQSQueueWithoutRedriveHasNone(t *testing.T) {
	out, _ := collectSQS(t)

	q, _ := out.byID("sqs/orders-events-dlq")
	var spec queueSpec
	specOf(t, q, &spec)

	if spec.Redrive != nil {
		t.Errorf("a DLQ with no redrive policy of its own reported one: %+v", spec.Redrive)
	}
}

func TestSQSRecordsFIFOAttributes(t *testing.T) {
	out, _ := collectSQS(t)

	q, _ := out.byID("sqs/notifications")
	var spec queueSpec
	specOf(t, q, &spec)

	if !spec.FIFO || !spec.ContentDedup {
		t.Errorf("FIFO attributes lost: fifo=%v dedup=%v", spec.FIFO, spec.ContentDedup)
	}
	if spec.VisibilityTimeout != 60 {
		t.Errorf("visibilityTimeout: got %d want 60", spec.VisibilityTimeout)
	}
}

// TestSQSKeepsAccessPolicy matters because the policy's Principal entries name
// who may send to a queue, which is a linking signal. Interpreting it belongs to
// the linker; preserving it belongs here.
func TestSQSKeepsAccessPolicy(t *testing.T) {
	out, _ := collectSQS(t)

	q, _ := out.byID("sqs/orders-events")
	var spec queueSpec
	specOf(t, q, &spec)

	if len(spec.Policy) == 0 {
		t.Fatal("access policy was dropped")
	}
	if !bytes.Contains(spec.Policy, []byte("orders-api-task")) {
		t.Errorf("policy lost its principal: %s", spec.Policy)
	}
}

func TestSQSMalformedRedriveDoesNotLoseTheQueue(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":       `{not json`,
		"empty target":   `{"deadLetterTargetArn":"","maxReceiveCount":5}`,
		"missing target": `{"maxReceiveCount":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseRedrive(raw); got != nil {
				t.Errorf("want nil for %q, got %+v", raw, got)
			}
		})
	}
}

// TestSQSRedriveAcceptsStringMaxReceiveCount pins a real AWS inconsistency:
// maxReceiveCount has been returned both as a number and as a quoted string
// depending on how the queue was created.
func TestSQSRedriveAcceptsStringMaxReceiveCount(t *testing.T) {
	got := parseRedrive(`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:123456789012:dlq","maxReceiveCount":"3"}`)
	if got == nil {
		t.Fatal("string maxReceiveCount was rejected")
	}
	if got.MaxReceive != 3 {
		t.Errorf("maxReceiveCount: got %d want 3", got.MaxReceive)
	}
}

func TestSQSUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectSQS(t)
	assertOpsMatchAllowList(t, "SQS", tr)
}

// TestSQSRequestsPagination pins a silent-truncation bug found while validating
// against a real account: ListQueues returns NextToken only when MaxResults is
// set, so without it an account with more than 1000 queues is cut off with no
// sign anything is missing. The fixture's first page only matches a request that
// carries MaxResults, and the queues are split across two pages.
func TestSQSRequestsPagination(t *testing.T) {
	out, tr := collectSQS(t)
	if n := tr.count("SQS", "ListQueues"); n != 2 {
		t.Errorf("ListQueues called %d times, want 2 (one per page)", n)
	}
	if len(out.resources) != 4 {
		t.Errorf("want 4 queues across two pages, got %d", len(out.resources))
	}
}
