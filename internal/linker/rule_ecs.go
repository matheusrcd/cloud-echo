package linker

import (
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// loadBalancerRule reads the target groups an ECS service registers in.
//
// When the target group and its load balancer are in the inventory, the
// load balancer's edge to the service says it all (elbv2.listener), and whether
// the service is reachable follows from whether the balancer is. A target group
// no load balancer forwards to reaches the service from nowhere, and is said
// so. Only when the target group is not in the inventory at all — ELBv2 not
// collected, or denied — is the service marked an entrypoint on the strength
// of the declaration alone, as before the collector existed.
type loadBalancerRule struct{}

func (loadBalancerRule) Name() string { return "ecs.load-balancer" }
func (loadBalancerRule) Tier() int    { return 1 }

func (loadBalancerRule) Apply(c *Context) {
	x := c.elbs()
	Each(c, spec.TypeECSService, func(r inventory.Resource, s *spec.ECSService) {
		for _, lb := range s.LoadBalancers {
			name := targetGroupName(lb.TargetGroupARN)
			tg := x.tgs[lb.TargetGroupARN]
			switch {
			case tg == nil:
				c.Trigger(r.ID, "behind a load balancer (target group "+name+
					", not in the inventory: internet-facing or internal cannot be told)")
			case len(tg.LoadBalancerARNs) == 0:
				c.Unresolved(r.ID, lb.TargetGroupARN, "registers in target group "+name+
					", which no load balancer forwards to: nothing reaches the service through it")
			}
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
