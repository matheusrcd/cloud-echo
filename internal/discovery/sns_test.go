package discovery

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

func collectSNS(t *testing.T, prepend ...exchange) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "sns")
	tr.exchanges = append(prepend, tr.exchanges...)
	out := &captureEmitter{}
	if err := (&SNS{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(prepend) == 0 {
		tr.assertAllMatched(t)
	}
	return out, tr
}

func subscriptionOf(t *testing.T, s spec.SNSTopic, protocol string) spec.SNSSubscription {
	t.Helper()
	for _, sub := range s.Subscriptions {
		if sub.Protocol == protocol {
			return sub
		}
	}
	t.Fatalf("no %s subscription in %+v", protocol, s.Subscriptions)
	return spec.SNSSubscription{}
}

// TestSNSCollectsTopicsAndTheirSubscriptions: every topic across ListTopics'
// pages, every subscription across ListSubscriptionsByTopic's, and a tag
// denial that costs the tags, never the topic.
func TestSNSCollectsTopicsAndTheirSubscriptions(t *testing.T) {
	out, tr := collectSNS(t)
	if got := out.ids(); !reflect.DeepEqual(got, []string{"sns/alerts", "sns/billing.fifo", "sns/order-events"}) {
		t.Fatalf("ids: %q", got)
	}
	if n := tr.count(snsSvc, "ListTopics"); n != 2 {
		t.Errorf("ListTopics called %d times, want 2 (one per page)", n)
	}
	ev := rdsSpec[spec.SNSTopic](t, out, "sns/order-events")
	if len(ev.Subscriptions) != 5 {
		t.Errorf("order-events: %d subscriptions across two pages, want 5", len(ev.Subscriptions))
	}
	if r, _ := out.byID("sns/order-events"); r.Tags["team"] != "orders" {
		t.Errorf("tags: %v", r.Tags)
	}
	if len(out.warnings) != 1 || out.warnings[0].Op != "ListTagsForResource" || out.warnings[0].Kind != "access-denied" {
		t.Errorf("SNS's AuthorizationError must read as a denial, once: %+v", out.warnings)
	}
}

// TestSubscriptionAttributesAreRead: the filter policy, raw delivery and
// dead-letter queue exist only in GetSubscriptionAttributes, which a pending
// subscription — no ARN — is never asked for.
func TestSubscriptionAttributesAreRead(t *testing.T) {
	out, tr := collectSNS(t)
	ev := rdsSpec[spec.SNSTopic](t, out, "sns/order-events")
	q := subscriptionOf(t, ev, "sqs")
	if !q.RawDelivery || q.FilterPolicy != `{"type":["order.created"]}` || q.FilterScope != "MessageAttributes" ||
		q.Target == nil || q.Target.ID != "sqs/orders-events" {
		t.Errorf("sqs subscription: %+v", q)
	}
	fn := subscriptionOf(t, ev, "lambda")
	if fn.Target == nil || fn.Target.ID != "lambda/audit-writer" || fn.DeadLetter == nil || fn.DeadLetter.ID != "sqs/orders-events-dlq" {
		t.Errorf("lambda subscription and its dead-letter queue: %+v", fn)
	}
	if h := subscriptionOf(t, ev, "https"); !h.Pending || h.ARN != "" {
		t.Errorf("a pending subscription has no ARN: %+v", h)
	}
	if n := tr.count(snsSvc, "GetSubscriptionAttributes"); n != 7 {
		t.Errorf("GetSubscriptionAttributes called %d times, want 7 (every confirmed subscription)", n)
	}
	bi := rdsSpec[spec.SNSTopic](t, out, "sns/billing.fifo")
	if fh := subscriptionOf(t, bi, "firehose"); fh.RoleARN == "" {
		t.Errorf("firehose subscription keeps the role it delivers with: %+v", fh)
	}
	if x := subscriptionOf(t, bi, "sqs"); x.Owner != "999999999999" {
		t.Errorf("another account's subscription keeps its owner: %+v", x)
	}
}

// TestSubscriptionEndpointsNeverLeak: SNS masks a basic-auth password but
// returns a query token in full; an email address and a phone number are a
// person's. All three are planted, in both responses that carry an endpoint;
// none may reach the serialized resource.
func TestSubscriptionEndpointsNeverLeak(t *testing.T) {
	out, _ := collectSNS(t)
	r, _ := out.byID("sns/order-events")
	all := string(r.Spec) + string(r.Raw)
	for name, secret := range map[string]string{
		"query token":   "tk-" + "planted-5511",
		"email address": "ops-oncall" + "@example.com",
		"phone number":  "+1555" + "5550100",
	} {
		if strings.Contains(all, secret) {
			t.Errorf("%s reached the resource", name)
		}
	}
	ev := rdsSpec[spec.SNSTopic](t, out, "sns/order-events")
	if h := subscriptionOf(t, ev, "https"); !strings.Contains(h.Endpoint, "partner.example.com") {
		t.Errorf("the host is the link and must survive: %q", h.Endpoint)
	}
	if e := subscriptionOf(t, ev, "email"); e.Endpoint != "<redacted:personal-data>" {
		t.Errorf("email endpoint: %q", e.Endpoint)
	}
}

// TestTopicSettings: what the materializer needs to recreate a topic, and the
// policy kept raw for the linker.
func TestTopicSettings(t *testing.T) {
	out, _ := collectSNS(t)
	bi := rdsSpec[spec.SNSTopic](t, out, "sns/billing.fifo")
	if !bi.FIFO || !bi.ContentDedup || len(bi.DeliveryPolicy) == 0 {
		t.Errorf("fifo topic: %+v", bi)
	}
	if ev := rdsSpec[spec.SNSTopic](t, out, "sns/order-events"); ev.KMSKeyID != "alias/aws/sns" {
		t.Errorf("kms key: %q", ev.KMSKeyID)
	}
	if al := rdsSpec[spec.SNSTopic](t, out, "sns/alerts"); !strings.Contains(string(al.Policy), "cloudwatch.amazonaws.com") {
		t.Errorf("policy: %s", al.Policy)
	}
}

// TestSubscriptionAttributesDenialCostsOnlyThem: a role allowed to list
// subscriptions but not to read their attributes keeps every subscription,
// says what it could not read, and does not ask again for each one.
func TestSubscriptionAttributesDenialCostsOnlyThem(t *testing.T) {
	out, tr := collectSNS(t, exchange{Op: "GetSubscriptionAttributes", Status: 403, service: "sns",
		Body: `<ErrorResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><Error><Type>Sender</Type>` +
			`<Code>AuthorizationError</Code><Message>not authorized</Message></Error></ErrorResponse>`})
	ev := rdsSpec[spec.SNSTopic](t, out, "sns/order-events")
	if len(ev.Subscriptions) != 5 || !reflect.DeepEqual(ev.Unread, []string{"subscription attributes"}) {
		t.Errorf("subscriptions kept, gap said: %d %v", len(ev.Subscriptions), ev.Unread)
	}
	if n := tr.count(snsSvc, "GetSubscriptionAttributes"); n != 3 {
		t.Errorf("GetSubscriptionAttributes called %d times, want 3 (once per topic)", n)
	}
}

// TestADeletedTopicIsSkipped: a topic deleted between ListTopics and
// GetTopicAttributes answers NotFound; the scan goes on without it.
func TestADeletedTopicIsSkipped(t *testing.T) {
	out, _ := collectSNS(t, exchange{Op: "GetTopicAttributes", Match: "alerts", Status: 404, service: "sns",
		Body: `<ErrorResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><Error><Type>Sender</Type>` +
			`<Code>NotFound</Code><Message>Topic does not exist</Message></Error></ErrorResponse>`})
	if got := out.ids(); !reflect.DeepEqual(got, []string{"sns/billing.fifo", "sns/order-events"}) {
		t.Errorf("ids: %q", got)
	}
}

func TestSNSUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectSNS(t)
	assertOpsMatchAllowList(t, snsSvc, tr)
}

func TestSNSIDs(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:sns:us-east-1:1:orders":                  "sns/orders",
		"arn:aws:sns:us-east-1:1:billing.fifo":            "sns/billing.fifo",
		"arn:aws:sns:us-east-1:1:orders:3a69814f-c865-42": "sns/orders",
	} {
		if got := resourceIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}
