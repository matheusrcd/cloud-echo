package linker

import (
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// loadBalancerRule marks an ECS service behind a load balancer as an entrypoint.
//
// There is no load balancer node yet — that needs the ELBv2 collector — so this
// produces a trigger, not an edge, and says what it cannot know: whether the
// balancer is internet-facing or internal.
type loadBalancerRule struct{}

func (loadBalancerRule) Name() string { return "ecs.load-balancer" }
func (loadBalancerRule) Tier() int    { return 1 }

func (loadBalancerRule) Apply(c *Context) {
	Each(c, spec.TypeECSService, func(r inventory.Resource, s *spec.ECSService) {
		for _, lb := range s.LoadBalancers {
			c.Trigger(r.ID, "behind a load balancer (target group "+targetGroupName(lb.TargetGroupARN)+
				"; internet-facing or internal needs the ELBv2 collector)")
		}
	})
}

// targetGroupName reads the name from arn:…:targetgroup/<name>/<id>; the last
// segment is a hash nobody recognises.
func targetGroupName(arn string) string {
	if a, ok := spec.ParseARN(arn); ok {
		if parts := strings.Split(a.Resource, "/"); len(parts) >= 2 && parts[0] == "targetgroup" {
			return parts[1]
		}
	}
	return lastSegment(arn)
}
