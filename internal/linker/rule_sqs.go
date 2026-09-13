package linker

import (
	"fmt"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

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
