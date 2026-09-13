package linker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// sqsResourcePolicyRule reads who may send to a queue, as
// lambda.resource-policy reads who may invoke a function: a topic in the
// inventory is corroborated (or its permission is stale), and a sender outside
// the graph — a topic elsewhere, S3, EventBridge, another account — makes the
// queue an entrypoint. Roles of this account are Tier 3's to read.
type sqsResourcePolicyRule struct{}

func (sqsResourcePolicyRule) Name() string { return "sqs.resource-policy" }
func (sqsResourcePolicyRule) Tier() int    { return 1 }

func (sqsResourcePolicyRule) Apply(c *Context) {
	Each(c, spec.TypeSQSQueue, func(r inventory.Resource, q *spec.SQSQueue) {
		if len(q.Policy) == 0 || !c.HasNode(r.ID) {
			return
		}
		var doc policyDoc
		if err := json.Unmarshal(q.Policy, &doc); err != nil {
			c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: %s queue policy: %v", c.rule, r.ID, err))
			return
		}
		for _, st := range doc.Statement {
			if st.Effect != "Allow" || !actionAllows(st.Action, "sqs:SendMessage") {
				continue
			}
			source := fmt.Sprintf("sqs:GetQueueAttributes %s Policy %s", q.QueueName, st.Sid)
			services, principals := principalsOf(st.Principal)
			for _, svc := range services {
				if svc == "sns.amazonaws.com" {
					snsSource(c, r.ID, "send to it", source, st.Condition)
					continue
				}
				c.Trigger(r.ID, svc+" may send to it (queue policy)")
			}
			for _, p := range principals {
				a, ok := spec.ParseARN(p)
				switch src := sourceArn(st.Condition); {
				case p == "*" && strings.HasPrefix(src, "arn:aws:sns:"):
					// The older form of the same grant: anyone, from this topic.
					snsSource(c, r.ID, "send to it", source, st.Condition)
				case p == "*" && ownAccountOnly(st.Condition, c.inv.AccountID):
				case p == "*" && src != "":
					c.Trigger(r.ID, "a sender from "+src+" may send to it (queue policy)")
				case p == "*":
					c.Trigger(r.ID, "anyone may send to it (queue policy principal \"*\")")
				case !ok || a.Account != c.inv.AccountID:
					c.Trigger(r.ID, "principal "+p+" may send to it (queue policy)")
				}
			}
		}
	})
}

// redriveRule: after maxReceiveCount failed receives, SQS moves a message to the
// dead-letter queue.
type redriveRule struct{}

func (redriveRule) Name() string { return "sqs.redrive" }
func (redriveRule) Tier() int    { return 1 }

func (redriveRule) Apply(c *Context) {
	Each(c, spec.TypeSQSQueue, func(r inventory.Resource, s *spec.SQSQueue) {
		if s.Redrive == nil {
			return
		}
		ref := &spec.TargetRef{ARN: s.Redrive.TargetARN, ID: s.Redrive.TargetID}
		if dst, ok := c.Local(r.ID, ref, "dead-letter queue"); ok {
			c.Edge(r.ID, dst, KindPublish, Certain, Active, "sqs:GetQueueAttributes "+s.QueueName,
				fmt.Sprintf("RedrivePolicy: after %d receives a message moves to %s", s.Redrive.MaxReceive, lastSegment(s.Redrive.TargetARN)))
		}
	})
}
