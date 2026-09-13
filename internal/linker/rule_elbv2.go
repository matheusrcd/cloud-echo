package linker

import (
	"fmt"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// elbIndex is what every rule touching a load balancer resolves through. It
// matches by full ARN and by DNS name, never by name alone: a load balancer's
// id drops the ARN's trailing hex, and a deleted-and-recreated balancer of the
// same name — or another account's — must not be taken for this one.
type elbIndex struct {
	byARN  map[string]string // load balancer ARN → node
	byDNS  map[string]string // DNS name → node
	byName map[string]string // name → node, only to explain a near miss
	lsnr   map[string]string // listener ARN → its load balancer's node
	typeOf map[string]string // node → application | network
	tgs    map[string]*spec.TargetGroup
	tgID   map[string]string   // target group ARN → its resource id
	svcs   map[string][]string // target group ARN → ECS services registering in it
}

func (c *Context) elbs() *elbIndex {
	if c.elb != nil {
		return c.elb
	}
	x := &elbIndex{byARN: map[string]string{}, byDNS: map[string]string{}, byName: map[string]string{}, lsnr: map[string]string{}, typeOf: map[string]string{},
		tgs: map[string]*spec.TargetGroup{}, tgID: map[string]string{}, svcs: map[string][]string{}}
	Each(c, spec.TypeLoadBalancer, func(r inventory.Resource, lb *spec.ELB) {
		if !c.HasNode(r.ID) {
			return
		}
		x.byARN[r.ARN] = r.ID
		x.byDNS[strings.ToLower(lb.DNSName)] = r.ID
		x.byName[lb.Name] = r.ID
		x.typeOf[r.ID] = lb.Type
		for _, l := range lb.Listeners {
			if l.ARN != "" {
				x.lsnr[l.ARN] = r.ID
			}
		}
	})
	Each(c, spec.TypeTargetGroup, func(r inventory.Resource, tg *spec.TargetGroup) {
		x.tgs[r.ARN], x.tgID[r.ARN] = tg, r.ID
	})
	Each(c, spec.TypeECSService, func(r inventory.Resource, s *spec.ECSService) {
		for _, lb := range s.LoadBalancers {
			x.svcs[lb.TargetGroupARN] = append(x.svcs[lb.TargetGroupARN], r.ID)
		}
	})
	c.elb = x
	return x
}

// kind is how a load balancer's caller reaches it, and how it reaches its
// targets: an Application Load Balancer speaks HTTP; a Network Load Balancer
// passes connections.
func (x *elbIndex) kind(lb string) Kind {
	if x.typeOf[lb] == "network" {
		return KindConnect
	}
	return KindHTTP
}

// byListener finds the load balancer a listener ARN belongs to, and whether
// that load balancer has the listener: listener/app/<name>/<lb-id>/<id> is
// loadbalancer/app/<name>/<lb-id>'s, but a deleted listener's ARN still
// names it.
func (x *elbIndex) byListener(arn string) (lb string, listed bool) {
	if id, ok := x.lsnr[arn]; ok {
		return id, true
	}
	i := strings.Index(arn, ":listener/")
	if i < 0 {
		return "", false
	}
	p := strings.Split(arn[i+len(":listener/"):], "/")
	if len(p) < 4 {
		return "", false
	}
	return x.byARN[arn[:i]+":loadbalancer/"+strings.Join(p[:3], "/")], false
}

// elbListenerRule: a load balancer sends each request its rules match to a
// target group, and the target group to what is registered in it — ECS
// services (which name their target groups), Lambda functions and ALBs (which
// the target group lists). An internet-facing load balancer is an entrypoint.
type elbListenerRule struct{}

func (elbListenerRule) Name() string { return "elbv2.listener" }
func (elbListenerRule) Tier() int    { return 1 }

func (elbListenerRule) Apply(c *Context) {
	x := c.elbs()
	Each(c, spec.TypeLoadBalancer, func(r inventory.Resource, lb *spec.ELB) {
		if !c.HasNode(r.ID) {
			return
		}
		if lb.Scheme == "internet-facing" {
			c.Trigger(r.ID, fmt.Sprintf("internet-facing %s load balancer", lb.Type))
		}
		reported := map[string]bool{}
		for _, l := range lb.Listeners {
			for _, rule := range l.Rules {
				where := fmt.Sprintf("%s:%d %s", l.Protocol, l.Port, ruleLabel(rule))
				source := fmt.Sprintf("elasticloadbalancing:DescribeRules %s %s:%d %s", lb.Name, l.Protocol, l.Port, rule.Priority)
				if lb.Type == "network" {
					source = fmt.Sprintf("elasticloadbalancing:DescribeListeners %s %s:%d", lb.Name, l.Protocol, l.Port)
				}
				for _, a := range rule.Actions {
					for _, f := range a.TargetGroups {
						forward(c, x, r.ID, lb, f, where, source, reported)
					}
				}
			}
		}
	})
}

func forward(c *Context, x *elbIndex, lbID string, lb *spec.ELB, f spec.ELBForward, where, source string, reported map[string]bool) {
	tg := x.tgs[f.ARN]
	name := targetGroupName(f.ARN)
	if tg == nil {
		if !reported[f.ARN] {
			reported[f.ARN] = true
			c.Unresolved(lbID, f.ARN, fmt.Sprintf("%s forwards to target group %s, which is not in the inventory", where, name))
		}
		return
	}
	detail := func(to string) string {
		d := fmt.Sprintf("%s → target group %s → %s", where, name, to)
		if f.Weight > 0 && f.Weight != 1 {
			d += fmt.Sprintf(" (weight %d)", f.Weight)
		}
		return d
	}
	linked := false
	for _, svc := range x.svcs[f.ARN] {
		c.Edge(lbID, svc, x.kind(lbID), Certain, Active, source, detail(svc))
		linked = true
	}
	for i := range tg.Targets {
		t := &tg.Targets[i]
		kind := KindInvoke
		if tg.TargetType == "alb" {
			kind = KindHTTP
			if id, ok := x.byARN[t.ARN]; ok {
				c.Edge(lbID, id, kind, Certain, Active, source, detail(id))
				linked = true
				continue
			}
		}
		if to, ok := c.Local(lbID, t, "target of "+name); ok {
			c.Edge(lbID, to, kind, Certain, Active, source, detail(to))
			linked = true
		}
	}
	if !linked && !reported[f.ARN] && (tg.TargetType == "ip" || tg.TargetType == "instance") {
		reported[f.ARN] = true
		c.Unresolved(lbID, f.ARN, fmt.Sprintf("%s forwards to target group %s, whose %s targets no ECS service registers: hosts outside the inventory",
			where, name, tg.TargetType))
	}
}

func ruleLabel(r spec.ELBRule) string {
	if r.Priority == "default" {
		return "default"
	}
	var cs []string
	for _, c := range r.Conditions {
		field := c.Field
		if c.Header != "" {
			field += " " + c.Header
		}
		cs = append(cs, field+" "+strings.Join(c.Values, ","))
	}
	return "rule " + r.Priority + " (" + strings.Join(cs, "; ") + ")"
}

// elbHost links a load balancer's DNS name, exactly. A Route 53 alias adds
// "dualstack."; a name that is this scan's balancer with another hash is a
// namesake — recreated, or another account's.
//
// Two shapes: an Application Load Balancer is [internal-]<name>-<hash>.<region>.elb.amazonaws.com,
// a Network Load Balancer <name>-<hash>.elb.<region>.amazonaws.com.
func (s *scanner) elbHost(holder string, v configValue, host string) {
	x := s.c.elbs()
	host = strings.TrimPrefix(host, "dualstack.")
	if id, ok := x.byDNS[host]; ok {
		if id != holder {
			s.c.Edge(holder, id, x.kind(id), High, Active, v.Source, fmt.Sprintf("%s reaches %s (its DNS name)", v.Label, id))
		}
		return
	}
	first, _, _ := strings.Cut(host, ".")
	name := strings.TrimPrefix(first, "internal-")
	if i := strings.LastIndex(name, "-"); i > 0 {
		name = name[:i]
	}
	if id := x.byName[name]; id != "" {
		s.c.Unresolved(holder, host, fmt.Sprintf("%s names a load balancer called %s, like %s in this scan, with another DNS name: a namesake (recreated, or another account's), not this one",
			v.Label, name, id))
		return
	}
	s.c.Unresolved(holder, host, fmt.Sprintf("%s names a load balancer that matches none in the inventory (deleted, or in another account)", v.Label))
}

func isELBHost(h string) bool {
	return strings.HasSuffix(h, ".elb.amazonaws.com") || (strings.Contains(h, ".elb.") && strings.HasSuffix(h, ".amazonaws.com"))
}

// hostOf is a URL's host, or "" for anything that is not one.
func hostOf(u string) string {
	if !strings.Contains(u, "://") {
		return ""
	}
	_, h, _ := splitURL(u)
	return h
}
