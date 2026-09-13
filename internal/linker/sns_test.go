package linker

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named assertions on the hand-written SNS account.

func casesSNS(t *testing.T) *Graph {
	t.Helper()
	return linkFixture(t, filepath.Join("testdata", "sns-cases"))
}

// TestATopicDeliversToItsSubscriptions: each confirmed subscription is a
// publish edge, corroborated by the receiver's own policy — the newer form
// (Service sns.amazonaws.com) and the older (Principal "*" + SourceArn) alike —
// and a dead-letter queue is one more.
func TestATopicDeliversToItsSubscriptions(t *testing.T) {
	g := casesSNS(t)
	for to, policy := range map[string]string{"sqs/allowed-q": "sqs.resource-policy", "sqs/legacy-q": "sqs.resource-policy",
		"lambda/sub-fn": "lambda.resource-policy", "sqs/dlq": "sqs.resource-policy"} {
		e := mustEdge(t, g, "sns/events", to, KindPublish)
		if e.Confidence != Certain || !contains(rulesOf(e), "sns.subscription") || !contains(rulesOf(e), policy) {
			t.Errorf("sns/events → %s: %s %v", to, e.Confidence, rulesOf(e))
		}
	}
	if e := mustEdge(t, g, "sns/events", "sqs/allowed-q", KindPublish); !strings.Contains(e.Evidence[0].Detail, "filter policy") {
		t.Errorf("a filtered subscription says so: %+v", e.Evidence)
	}
}

// TestASubscriptionIsNotADelivery: SNS sends to a queue or invokes a function
// only if the receiver's policy lets it. Without that every message is dropped
// — no edge, a blocked finding — while a policy that could not be read is not
// a refusal.
func TestASubscriptionIsNotADelivery(t *testing.T) {
	g := casesSNS(t)
	for _, to := range []string{"sqs/no-policy-q", "sqs/other-topic-q", "lambda/no-perm-fn"} {
		if len(g.Between("sns/events", to)) > 0 || !finding(g, "blocked", "sns/events", to) {
			t.Errorf("%s: a refused delivery must be blocked, with no edge", to)
		}
		if f := flowOf(t, g, to); f != Unreached {
			t.Errorf("%s is reached through a delivery that fails: %s", to, f)
		}
	}
	mustEdge(t, g, "sns/events", "lambda/unread-fn", KindPublish)
	mustEdge(t, g, "sns/events", "lambda/sub2-fn", KindPublish)
	if len(g.Between("sns/events", "sqs/no-policy-q")) > 0 || !finding(g, "blocked", "sns/events", "sqs/no-policy-q") {
		t.Error("a dead-letter queue that refuses the topic must be blocked too")
	}
}

// TestATopicIsAnAsyncBoundary: a function on the request path publishes to a
// topic; the topic and everything it delivers to run beside the request, even
// a load balancer reached over HTTP. A topic CloudWatch publishes to is an
// entrypoint whose deliveries are still async.
func TestATopicIsAnAsyncBoundary(t *testing.T) {
	g := casesSNS(t)
	if f := flowOf(t, g, "lambda/caller-fn"); f != Sync {
		t.Fatalf("setup: caller-fn is %s, want sync", f)
	}
	for id, want := range map[string]Flow{"sns/events": Async, "sqs/allowed-q": Async, "elb/web": Async,
		"sns/alarms": Entrypoint, "sqs/alarms-q": Async, "sns/notify": Async} {
		if f := flowOf(t, g, id); f != want {
			t.Errorf("%s: %s, want %s", id, f, want)
		}
	}
	mustEdge(t, g, "sns/events", "elb/web", KindPublish)
}

// TestWhoMayPublishIsReadFromThePolicy: every topic carries AWS's default
// statement — Principal "*", conditioned on AWS:SourceOwner — which is the
// account's own services, not anyone. Services, other accounts and an
// unconditioned "*" make a topic an entrypoint.
func TestWhoMayPublishIsReadFromThePolicy(t *testing.T) {
	g := casesSNS(t)
	for _, id := range []string{"sns/alarms", "sns/partner-in", "sns/open"} {
		if n, _ := g.Node(id); len(n.Triggers) == 0 {
			t.Errorf("%s: no trigger", id)
		}
	}
	if n, _ := g.Node("sns/quiet"); len(n.Triggers) > 0 {
		t.Errorf("the default statement made a topic an entrypoint: %v", n.Triggers)
	}
}

// TestTheReceiverSideSaysWhatItExpects: a function or queue policy naming a
// topic that delivers nothing there is a stale permission; naming a topic
// outside the inventory, or none, makes the receiver an entrypoint — and so
// does S3 on a queue.
func TestTheReceiverSideSaysWhatItExpects(t *testing.T) {
	g := casesSNS(t)
	if !finding(g, "stale-permission", "lambda/stale-fn", "sns/quiet") || !finding(g, "stale-permission", "sqs/other-topic-q", "sns/alarms") {
		t.Error("a permission for a topic that does not deliver here was not reported")
	}
	// twin-fn's permission names another account's topic called events: a
	// namesake, not this scan's events.
	if finding(g, "stale-permission", "lambda/twin-fn", "sns/events") {
		t.Error("another account's topic was taken for this scan's namesake")
	}
	for _, id := range []string{"lambda/far-fn", "lambda/any-fn", "lambda/twin-fn", "sqs/far-q", "sqs/bucket-q"} {
		if f := flowOf(t, g, id); f != Entrypoint {
			t.Errorf("%s: %s, want entrypoint", id, f)
		}
	}
}

// TestPendingAndForeignSubscriptions: a pending subscription delivers
// nothing — a disabled edge, so the dependency stays visible — even through a
// redacted credential; another account's queue is unresolved; a person's
// email is no node; unread subscriptions are said.
func TestPendingAndForeignSubscriptions(t *testing.T) {
	g := casesSNS(t)
	if e := mustEdge(t, g, "sns/events", "ext/partner.example.com", KindPublish); e.Status != Disabled {
		t.Errorf("pending: %s", e.Status)
	}
	if !finding(g, "unresolved", "sns/events", "999999999999:partner-q") {
		t.Error("another account's subscribed queue was not reported")
	}
	for _, n := range g.Nodes {
		if strings.Contains(n.ID, "redacted") || strings.Contains(n.ID, "@") {
			t.Errorf("a withheld endpoint became a node: %s", n.ID)
		}
	}
	if !finding(g, "unscanned", "sns/unlisted", "sns/unlisted") {
		t.Error("unread subscriptions were not said")
	}
}

// TestConfigurationAndRolesNameTopics: an ARN and a role's sns:Publish agree
// on publish, high; a bare name with a TOPIC key is the topic, a queue of the
// same name only a candidate; and a topic's default policy does not stand in
// for a grant, so a name the role cannot publish to is unpermitted.
func TestConfigurationAndRolesNameTopics(t *testing.T) {
	g := casesSNS(t)
	if e := mustEdge(t, g, "lambda/caller-fn", "sns/events", KindPublish); e.Confidence != High {
		t.Errorf("config + grant: %s", e.Confidence)
	}
	if e := mustEdge(t, g, "lambda/caller-fn", "sns/notify", KindReferences); e.Confidence != Medium {
		t.Errorf("NOTIFY_TOPIC: %s", e.Confidence)
	}
	if !finding(g, "ambiguous", "lambda/caller-fn", "notify") {
		t.Error("a topic and a queue of one name: not reported")
	}
	if !finding(g, "unpermitted", "lambda/caller-fn", "sns/alarms") {
		t.Error("the default topic policy held back unpermitted")
	}
}

// TestAnUnreadPolicyMayGrant: a function whose GetPolicy was refused may have
// a policy naming the caller's role. Unread is not none: naming it is never
// called unpermitted — a latent Tier-3 gap the SNS round's collector change
// (policyUnread) closed.
func TestAnUnreadPolicyMayGrant(t *testing.T) {
	g := casesSNS(t)
	mustEdge(t, g, "lambda/caller-fn", "lambda/unread-fn", KindReferences)
	if finding(g, "unpermitted", "lambda/caller-fn", "lambda/unread-fn") {
		t.Error("an unread policy was taken for one that grants nothing")
	}
}

// TestALoadBalancerIsItsDNSNameWhateverTheShape: an emulator's load balancer
// has a DNS name AWS would never mint (…elb.localhost.floci.io). Matched
// exactly it is still that load balancer — from a subscription and from
// configuration — never a third party the gateway would mock.
func TestALoadBalancerIsItsDNSNameWhateverTheShape(t *testing.T) {
	g := casesSNS(t)
	mustEdge(t, g, "sns/events", "elb/local", KindPublish)
	mustEdge(t, g, "lambda/caller-fn", "elb/local", KindHTTP)
	if _, ok := g.Node("ext/local-9.elb.localhost.floci.io"); ok {
		t.Error("an exact load balancer DNS name became a third party")
	}
}
