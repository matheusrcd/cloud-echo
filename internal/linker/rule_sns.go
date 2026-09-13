package linker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// snsSubscriptionRule: a topic hands every message it accepts to each of its
// confirmed subscriptions. The edge is publish whatever the endpoint — a queue,
// a function, a URL: SNS delivers asynchronously, so a topic is an async
// boundary even when an entrypoint publishes to it.
//
// A subscription is a declaration, but not a delivery. SNS may send to a queue
// only if the queue's policy lets it, and invoke a function only if the
// function's policy does; without that, every message is dropped, silently —
// the M1 account's alerts topic reported NumberOfNotificationsFailed and its
// queue stayed empty. That is a blocked finding and no edge, as for a Deny.
//
// Who may publish comes from the topic policy. Every topic carries AWS's
// default statement, Principal "*" conditioned on AWS:SourceOwner: that is
// the account's own services, not anyone. Services (CloudWatch alarms,
// EventBridge, S3) and other accounts publishing make the topic an entrypoint.
type snsSubscriptionRule struct{}

func (snsSubscriptionRule) Name() string { return "sns.subscription" }
func (snsSubscriptionRule) Tier() int    { return 1 }

func (snsSubscriptionRule) Apply(c *Context) {
	Each(c, spec.TypeSNSTopic, func(r inventory.Resource, t *spec.SNSTopic) {
		if !c.HasNode(r.ID) {
			return
		}
		topicPublishers(c, r, t)
		if contains(t.Unread, "subscriptions") {
			c.Finding("unscanned", r.ID, r.ID, "its subscriptions could not be read: what it delivers to is unknown")
		}
		for _, sub := range t.Subscriptions {
			deliver(c, r, t, sub)
		}
	})
}

func deliver(c *Context, r inventory.Resource, t *spec.SNSTopic, sub spec.SNSSubscription) {
	source := fmt.Sprintf("sns:ListSubscriptionsByTopic %s %s", t.TopicName, sub.Protocol)
	status, note := Active, ""
	if sub.Pending {
		status, note = Disabled, " — pending confirmation: nothing is delivered until the endpoint confirms"
	}
	if sub.FilterPolicy != "" {
		scope := sub.FilterScope
		if scope == "" {
			scope = "MessageAttributes"
		}
		note = " (filter policy on " + scope + ")" + note
	}
	switch sub.Protocol {
	case "sqs", "lambda":
		to, ok := c.Local(r.ID, sub.Target, "subscription")
		if !ok {
			return
		}
		if why := refusesTopic(c, to, r.ARN); why != "" && !sub.Pending {
			c.Finding("blocked", r.ID, to, fmt.Sprintf("subscribed, but %s: every delivery fails, no edge", why))
		} else {
			c.Edge(r.ID, to, KindPublish, Certain, status, source, "delivers to "+to+note)
		}
		if sub.DeadLetter != nil {
			dlq, ok := c.Local(r.ID, sub.DeadLetter, "dead-letter queue of the subscription to "+to)
			if !ok {
				return
			}
			if why := refusesTopic(c, dlq, r.ARN); why != "" {
				c.Finding("blocked", r.ID, dlq, fmt.Sprintf("dead-letter queue of the subscription to %s, but %s: undeliverable messages are lost", to, why))
				return
			}
			c.Edge(r.ID, dlq, KindPublish, Certain, status, source, fmt.Sprintf("messages it cannot deliver to %s go to %s", to, dlq))
		}
	case "http", "https":
		host := strings.ToLower(hostOf(sub.Endpoint))
		// A load balancer's exact DNS name is that load balancer, whatever its
		// shape: an emulator's is not AWS's (…elb.localhost.floci.io), and it
		// became a third party in the SNS round trip.
		lb := c.elbs().byDNS[strings.TrimPrefix(host, "dualstack.")]
		switch {
		case host == "":
			c.Unresolved(r.ID, sub.Endpoint, "an HTTP subscription whose endpoint is not a URL")
		case lb != "":
			c.Edge(r.ID, lb, KindPublish, Certain, status, source, "delivers over HTTP to "+lb+note)
		case isELBHost(host):
			c.Unresolved(r.ID, host, "an HTTP subscription to a load balancer that matches none in the inventory")
		case isAWSHost(host):
			c.Unresolved(r.ID, host, "an HTTP subscription to an AWS endpoint cloud-echo does not resolve")
		default:
			// From scheme, host and port: a redaction marker in the
			// credentials is not a URL net/url parses, and losing the
			// endpoint to it would lose the dependency silently.
			scheme, _, port := splitURL(sub.Endpoint)
			base := scheme + "://" + host
			if port != "" {
				base += ":" + port
			}
			if to, ok := c.External(base); ok {
				c.Edge(r.ID, to, KindPublish, Certain, status, source, "delivers over HTTP to "+strings.TrimPrefix(to, "ext/")+note)
			} else {
				c.Unresolved(r.ID, host, "an HTTP subscription whose endpoint is not a URL")
			}
		}
	case "firehose":
		c.Unresolved(r.ID, sub.Endpoint, "delivers to a Firehose delivery stream; cloud-echo has no Firehose collector")
	case "application":
		c.Unresolved(r.ID, sub.Endpoint, "delivers to a mobile push endpoint, which is not collected")
	default:
		// email, email-json, sms: a person, not a node — and their address is
		// withheld at collection.
	}
}

// topicPublishers marks a topic an entrypoint when its policy lets something
// outside the graph publish.
func topicPublishers(c *Context, r inventory.Resource, t *spec.SNSTopic) {
	if len(t.Policy) == 0 {
		return
	}
	var doc policyDoc
	if err := json.Unmarshal(t.Policy, &doc); err != nil {
		c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: %s topic policy: %v", c.rule, r.ID, err))
		return
	}
	for _, st := range doc.Statement {
		if st.Effect != "Allow" || !actionAllows(st.Action, "sns:Publish") {
			continue
		}
		services, principals := principalsOf(st.Principal)
		for _, svc := range services {
			c.Trigger(r.ID, svc+" may publish to it (topic policy)")
		}
		for _, p := range principals {
			switch a, ok := spec.ParseARN(p); {
			case p == "*" && ownAccountOnly(st.Condition, c.inv.AccountID):
				// AWS's default statement: the account's own services.
			case p == "*":
				c.Trigger(r.ID, "anyone may publish to it (topic policy principal \"*\")")
			case !ok || a.Account != c.inv.AccountID:
				c.Trigger(r.ID, "principal "+p+" may publish to it (topic policy)")
			}
		}
	}
}

// refusesTopic says why a subscribed queue or function would refuse the
// topic, or "" when its policy lets it — or cannot be read, which is not a
// refusal.
func refusesTopic(c *Context, id, topicARN string) string {
	if q, ok := lookup[spec.SQSQueue](c, id, spec.TypeSQSQueue); ok {
		switch {
		case len(q.Policy) == 0:
			return "the queue has no policy, and SNS may send to a queue only if its policy says so"
		case !policyLetsService(q.Policy, "sns.amazonaws.com", "sqs:SendMessage", topicARN):
			return "the queue's policy does not let this topic send"
		}
		return ""
	}
	if fn, ok := lookup[spec.LambdaFunction](c, id, spec.TypeLambdaFunction); ok && !fn.PolicyUnread {
		switch {
		case len(fn.ResourcePolicy) == 0:
			return "the function has no resource policy, and SNS may invoke a function only if its policy says so"
		case !policyLetsService(fn.ResourcePolicy, "sns.amazonaws.com", "lambda:InvokeFunction", topicARN):
			return "the function's resource policy does not let this topic invoke it"
		}
	}
	return ""
}

// policyLetsService reports whether a resource policy may allow a service to
// take an action on behalf of a source. Generous on purpose: it is used to
// claim a refusal, so anything it cannot rule out — a NotAction, a wildcard
// principal, a condition it does not read — counts as allowing.
func policyLetsService(raw json.RawMessage, service, action, source string) bool {
	var doc policyDoc
	if json.Unmarshal(raw, &doc) != nil {
		return true
	}
	for _, st := range doc.Statement {
		if st.Effect != "Allow" {
			continue
		}
		if len(st.NotAction) == 0 && !actionAllows(st.Action, action) {
			continue
		}
		services, principals := principalsOf(st.Principal)
		if len(st.NotPrincipal) == 0 && !contains(services, service) && !contains(principals, "*") {
			continue
		}
		if src := sourceArn(st.Condition); src != "" && !wildMatch(src, source, false) {
			continue
		}
		return true
	}
	return false
}

// actionAllows reports whether a statement's Action covers an action; IAM
// action names are case-insensitive.
func actionAllows(raw json.RawMessage, action string) bool {
	for _, a := range strOrList(raw) {
		if wildMatch(a, action, true) {
			return true
		}
	}
	return false
}

// sourceContextKeys constrain the AWS service acting for a resource owner, not
// the principal: a statement conditioned only on them grants no IAM role.
var sourceContextKeys = []string{"aws:sourceowner", "aws:sourceaccount", "aws:sourcearn"}

// ownAccountOnly reports whether a statement's conditions limit it to the
// account's own sources: AWS:SourceOwner or aws:SourceAccount equal to it.
func ownAccountOnly(cond map[string]map[string]json.RawMessage, account string) bool {
	for _, kv := range cond {
		for k, v := range kv {
			k = strings.ToLower(k)
			if (k == "aws:sourceowner" || k == "aws:sourceaccount") && contains(strOrList(v), account) {
				return true
			}
		}
	}
	return false
}

// onlySourceConditions reports whether every condition key is a
// source-context key.
func onlySourceConditions(cond map[string]map[string]json.RawMessage) bool {
	if len(cond) == 0 {
		return false
	}
	for _, kv := range cond {
		for k := range kv {
			if !contains(sourceContextKeys, strings.ToLower(k)) {
				return false
			}
		}
	}
	return true
}

// snsSource corroborates, from a queue's or a function's own policy, the
// delivery of a topic in the inventory, or makes the resource an entrypoint
// for a topic outside it. Shared by lambda.resource-policy and
// sqs.resource-policy.
func snsSource(c *Context, id, what, source string, cond map[string]map[string]json.RawMessage) {
	src := sourceArn(cond)
	topic := spec.IDFromARN(src)
	a, _ := spec.ParseARN(src)
	switch {
	case src == "":
		c.Trigger(id, "any SNS topic may "+what+" (the policy names none)")
	case strings.ContainsAny(src, "*?") || a.Account != c.inv.AccountID || a.Region != c.inv.Region || !c.HasNode(topic):
		c.Trigger(id, "SNS topic "+src+", outside the inventory, may "+what)
	default:
		c.Corroborate(topic, id, KindPublish, source, fmt.Sprintf("its policy lets %s %s", topic, what),
			&Finding{Kind: "stale-permission", Node: id, Target: topic,
				Detail: fmt.Sprintf("its policy lets %s %s, but no subscription of that topic delivers here", topic, what)})
	}
}
