package discovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// ELBv2 collects Application and Network Load Balancers, their listeners and
// rules, and their target groups.
//
// The rules are the load balancer's definition — which path or host reaches
// which target group — as routes are an API's, and the local environment has
// to reproduce them. They are also where secrets hide: a header condition
// holding the shared secret a CDN sends, an OIDC action's client secret, a
// fixed response's body. Conditions are redacted like env vars, the client
// secret and the body are dropped, in Spec and in Raw alike.
//
// Gateway Load Balancers route packets to appliances, not requests to
// workloads, and are reported and skipped. Classic Load Balancers are another
// API and are not collected.
type ELBv2 struct{}

func (*ELBv2) Service() string { return "Elastic Load Balancing v2" }

const elbSvc = "Elastic Load Balancing v2"

func (c *ELBv2) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := elb.NewFromConfig(s.Config())

	var lbs []elbtypes.LoadBalancer
	lp := elb.NewDescribeLoadBalancersPaginator(api, &elb.DescribeLoadBalancersInput{})
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, elbSvc, "DescribeLoadBalancers", err); err != nil {
				return err
			}
			break
		}
		lbs = append(lbs, page.LoadBalancers...)
	}

	var tgs []elbtypes.TargetGroup
	tp := elb.NewDescribeTargetGroupsPaginator(api, &elb.DescribeTargetGroupsInput{})
	for tp.HasMorePages() {
		page, err := tp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, elbSvc, "DescribeTargetGroups", err); err != nil {
				return err
			}
			break
		}
		tgs = append(tgs, page.TargetGroups...)
	}

	var kept []elbtypes.LoadBalancer
	for _, lb := range lbs {
		if lb.Type == elbtypes.LoadBalancerTypeEnumGateway {
			out.Warn(inventory.Warning{Service: elbSvc, Kind: "out-of-scope", Message: fmt.Sprintf(
				"load balancer %s is a Gateway Load Balancer: it routes packets to appliances, not requests to workloads, and is not collected",
				aws.ToString(lb.LoadBalancerName))})
			continue
		}
		kept = append(kept, lb)
	}

	arns := make([]string, 0, len(kept)+len(tgs))
	for _, lb := range kept {
		arns = append(arns, aws.ToString(lb.LoadBalancerArn))
	}
	for _, tg := range tgs {
		arns = append(arns, aws.ToString(tg.TargetGroupArn))
	}
	tags, err := c.tags(ctx, api, out, arns)
	if err != nil {
		return err
	}

	for _, lb := range kept {
		r, err := c.loadBalancer(ctx, api, s, out, lb, tags[aws.ToString(lb.LoadBalancerArn)])
		if err != nil {
			return err
		}
		out.Emit(r)
	}
	for _, tg := range tgs {
		r, err := c.targetGroup(ctx, api, s, out, tg, tags[aws.ToString(tg.TargetGroupArn)])
		if err != nil {
			return err
		}
		out.Emit(r)
	}
	return nil
}

// tags reads tags twenty ARNs at a time, the most DescribeTags accepts. A
// denial costs the tags, never the resources.
func (c *ELBv2) tags(ctx context.Context, api *elb.Client, out Emitter, arns []string) (map[string]map[string]string, error) {
	all := map[string]map[string]string{}
	for i := 0; i < len(arns); i += 20 {
		batch := arns[i:min(i+20, len(arns))]
		resp, err := api.DescribeTags(ctx, &elb.DescribeTagsInput{ResourceArns: batch})
		if err != nil {
			return all, warnOrFail(out, elbSvc, "DescribeTags", err)
		}
		for _, d := range resp.TagDescriptions {
			if len(d.Tags) == 0 {
				continue
			}
			m := map[string]string{}
			for _, t := range d.Tags {
				m[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			all[aws.ToString(d.ResourceArn)] = m
		}
	}
	return all, nil
}

func (c *ELBv2) loadBalancer(ctx context.Context, api *elb.Client, s *awsx.Session, out Emitter,
	lb elbtypes.LoadBalancer, tags map[string]string) (inventory.Resource, error) {
	name := aws.ToString(lb.LoadBalancerName)
	sp := spec.ELB{
		Name:           name,
		Type:           string(lb.Type),
		Scheme:         string(lb.Scheme),
		DNSName:        aws.ToString(lb.DNSName),
		VpcID:          aws.ToString(lb.VpcId),
		SecurityGroups: append([]string(nil), lb.SecurityGroups...),
		Listeners:      []spec.ELBListener{},
	}
	if lb.State != nil {
		sp.State = string(lb.State.Code)
	}
	for _, az := range lb.AvailabilityZones {
		if id := aws.ToString(az.SubnetId); id != "" {
			sp.Subnets = append(sp.Subnets, id)
		}
	}
	sort.Strings(sp.Subnets)
	sort.Strings(sp.SecurityGroups)

	type rawListener struct {
		Listener elbtypes.Listener
		Rules    []elbtypes.Rule `json:",omitempty"`
	}
	var raw []rawListener
	lp := elb.NewDescribeListenersPaginator(api, &elb.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn})
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, elbSvc, "DescribeListeners", err); err != nil {
				return inventory.Resource{}, err
			}
			break
		}
		for _, l := range page.Listeners {
			l.DefaultActions = sanitizeActions(l.DefaultActions)
			rl := rawListener{Listener: l}
			li := spec.ELBListener{ARN: aws.ToString(l.ListenerArn), Port: aws.ToInt32(l.Port), Protocol: string(l.Protocol)}
			for _, cert := range l.Certificates {
				li.Certificates = append(li.Certificates, aws.ToString(cert.CertificateArn))
			}
			// An Application Load Balancer's rules include its default; a
			// Network Load Balancer has no rules, only default actions.
			if lb.Type == elbtypes.LoadBalancerTypeEnumApplication {
				rules, err := c.rules(ctx, api, out, aws.ToString(l.ListenerArn))
				if err != nil {
					return inventory.Resource{}, err
				}
				rl.Rules = rules
				for _, r := range rules {
					li.Rules = append(li.Rules, specRule(r))
				}
			}
			if len(li.Rules) == 0 {
				li.Rules = []spec.ELBRule{{Priority: "default", Actions: specActions(l.DefaultActions)}}
			}
			sp.Listeners = append(sp.Listeners, li)
			raw = append(raw, rl)
		}
	}
	sort.Slice(sp.Listeners, func(i, j int) bool { return sp.Listeners[i].Port < sp.Listeners[j].Port })

	return newResource(s, resourceArgs{
		ID: "elb/" + name, Type: spec.TypeLoadBalancer, ARN: aws.ToString(lb.LoadBalancerArn), Name: name,
		Tags: tags, API: "elasticloadbalancing:DescribeLoadBalancers", Spec: sp,
		Raw: map[string]any{"loadBalancer": lb, "listeners": raw},
	}), nil
}

func (c *ELBv2) rules(ctx context.Context, api *elb.Client, out Emitter, listener string) ([]elbtypes.Rule, error) {
	var rules []elbtypes.Rule
	rp := elb.NewDescribeRulesPaginator(api, &elb.DescribeRulesInput{ListenerArn: aws.String(listener)})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			return rules, warnOrFail(out, elbSvc, "DescribeRules", err)
		}
		for _, r := range page.Rules {
			r.Conditions = sanitizeConditions(r.Conditions)
			r.Actions = sanitizeActions(r.Actions)
			rules = append(rules, r)
		}
	}
	// Priority order, the default last: the order the load balancer applies.
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if aws.ToBool(a.IsDefault) != aws.ToBool(b.IsDefault) {
			return !aws.ToBool(a.IsDefault)
		}
		return atoiOr(aws.ToString(a.Priority)) < atoiOr(aws.ToString(b.Priority))
	})
	return rules, nil
}

func (c *ELBv2) targetGroup(ctx context.Context, api *elb.Client, s *awsx.Session, out Emitter,
	tg elbtypes.TargetGroup, tags map[string]string) (inventory.Resource, error) {
	name := aws.ToString(tg.TargetGroupName)
	sp := spec.TargetGroup{
		Name:             name,
		TargetType:       string(tg.TargetType),
		Protocol:         string(tg.Protocol),
		Port:             aws.ToInt32(tg.Port),
		VpcID:            aws.ToString(tg.VpcId),
		HealthCheckPath:  aws.ToString(tg.HealthCheckPath),
		LoadBalancerARNs: append([]string(nil), tg.LoadBalancerArns...),
	}
	sort.Strings(sp.LoadBalancerARNs)
	// Only Lambda and ALB targets are read: they are resources with ids. An
	// ip or instance target is a task or a host whose address changes with
	// every deployment, and ECS services name their own target groups.
	if tg.TargetType == elbtypes.TargetTypeEnumLambda || tg.TargetType == elbtypes.TargetTypeEnumAlb {
		h, err := api.DescribeTargetHealth(ctx, &elb.DescribeTargetHealthInput{TargetGroupArn: tg.TargetGroupArn})
		if err != nil {
			if err := warnOrFail(out, elbSvc, "DescribeTargetHealth", err); err != nil {
				return inventory.Resource{}, err
			}
		} else {
			for _, d := range h.TargetHealthDescriptions {
				if d.Target == nil {
					continue
				}
				id := aws.ToString(d.Target.Id)
				sp.Targets = append(sp.Targets, spec.TargetRef{ARN: id, ID: resourceIDFromARN(id)})
			}
			sort.Slice(sp.Targets, func(i, j int) bool { return sp.Targets[i].ARN < sp.Targets[j].ARN })
		}
	}
	return newResource(s, resourceArgs{
		ID: "elb/tg/" + name, Type: spec.TypeTargetGroup, ARN: aws.ToString(tg.TargetGroupArn), Name: name,
		Tags: tags, API: "elasticloadbalancing:DescribeTargetGroups", Spec: sp, Raw: tg,
	}), nil
}

// sanitizeConditions redacts the values a rule matches on where a secret can
// sit — header and query-string values — before Spec or Raw is built.
func sanitizeConditions(cs []elbtypes.RuleCondition) []elbtypes.RuleCondition {
	out := make([]elbtypes.RuleCondition, len(cs))
	for i, c := range cs {
		if h := c.HttpHeaderConfig; h != nil {
			cp := *h
			name := aws.ToString(cp.HttpHeaderName)
			cp.Values = redactAll(name, cp.Values)
			c.HttpHeaderConfig = &cp
			if aws.ToString(c.Field) == "http-header" {
				c.Values = redactAll(name, c.Values)
			}
		}
		if q := c.QueryStringConfig; q != nil {
			cp := *q
			cp.Values = nil
			for _, kv := range q.Values {
				v, _ := redactValue(aws.ToString(kv.Key), aws.ToString(kv.Value))
				cp.Values = append(cp.Values, elbtypes.QueryStringKeyValuePair{Key: kv.Key, Value: aws.String(v)})
			}
			c.QueryStringConfig = &cp
			if aws.ToString(c.Field) == "query-string" {
				c.Values = redactAll("", c.Values)
			}
		}
		out[i] = c
	}
	return out
}

func redactAll(key string, vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i], _ = redactValue(key, v)
	}
	return out
}

// sanitizeActions drops what an action carries that must not leave AWS: an
// OIDC client secret and a fixed response's free-text body; a redirect's
// query is redacted like a URL's.
func sanitizeActions(as []elbtypes.Action) []elbtypes.Action {
	out := make([]elbtypes.Action, len(as))
	for i, a := range as {
		if o := a.AuthenticateOidcConfig; o != nil {
			cp := *o
			cp.ClientSecret = nil
			a.AuthenticateOidcConfig = &cp
		}
		if f := a.FixedResponseConfig; f != nil {
			cp := *f
			cp.MessageBody = nil
			a.FixedResponseConfig = &cp
		}
		if r := a.RedirectConfig; r != nil && aws.ToString(r.Query) != "" {
			cp := *r
			if u, changed := sanitizeURL("https://h/?" + aws.ToString(r.Query)); changed {
				cp.Query = aws.String(u[len("https://h/?"):])
			}
			a.RedirectConfig = &cp
		}
		out[i] = a
	}
	return out
}

func specRule(r elbtypes.Rule) spec.ELBRule {
	sr := spec.ELBRule{Priority: aws.ToString(r.Priority), Actions: specActions(r.Actions)}
	for _, c := range r.Conditions {
		sc := spec.ELBCondition{Field: aws.ToString(c.Field), Values: append([]string(nil), c.Values...)}
		if h := c.HttpHeaderConfig; h != nil {
			sc.Header, sc.Values = aws.ToString(h.HttpHeaderName), append([]string(nil), h.Values...)
		}
		if p := c.PathPatternConfig; p != nil && len(sc.Values) == 0 {
			sc.Values = append([]string(nil), p.Values...)
		}
		if h := c.HostHeaderConfig; h != nil && len(sc.Values) == 0 {
			sc.Values = append([]string(nil), h.Values...)
		}
		if q := c.QueryStringConfig; q != nil && len(sc.Values) == 0 {
			for _, kv := range q.Values {
				sc.Values = append(sc.Values, aws.ToString(kv.Key)+"="+aws.ToString(kv.Value))
			}
		}
		sr.Conditions = append(sr.Conditions, sc)
	}
	return sr
}

func specActions(as []elbtypes.Action) []spec.ELBAction {
	var out []spec.ELBAction
	for _, a := range as {
		sa := spec.ELBAction{Type: string(a.Type)}
		if f := a.ForwardConfig; f != nil && len(f.TargetGroups) > 0 {
			for _, t := range f.TargetGroups {
				sa.TargetGroups = append(sa.TargetGroups, spec.ELBForward{ARN: aws.ToString(t.TargetGroupArn), Weight: aws.ToInt32(t.Weight)})
			}
		} else if a.TargetGroupArn != nil {
			sa.TargetGroups = []spec.ELBForward{{ARN: aws.ToString(a.TargetGroupArn)}}
		}
		if r := a.RedirectConfig; r != nil {
			sa.Redirect = fmt.Sprintf("%s://%s:%s%s?%s %s", aws.ToString(r.Protocol), aws.ToString(r.Host),
				aws.ToString(r.Port), aws.ToString(r.Path), aws.ToString(r.Query), r.StatusCode)
		}
		if f := a.FixedResponseConfig; f != nil {
			sa.FixedStatus = aws.ToString(f.StatusCode)
		}
		if o := a.AuthenticateOidcConfig; o != nil {
			sa.Issuer = aws.ToString(o.Issuer)
		}
		if cg := a.AuthenticateCognitoConfig; cg != nil {
			sa.Issuer = aws.ToString(cg.UserPoolArn)
		}
		out = append(out, sa)
	}
	return out
}

func atoiOr(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 1 << 30
		}
		n = n*10 + int(r-'0')
	}
	return n
}
