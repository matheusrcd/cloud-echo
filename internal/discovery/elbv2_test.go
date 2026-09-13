package discovery

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

func collectELBv2(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "elasticloadbalancingv2")
	out := &captureEmitter{}
	if err := (&ELBv2{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

// TestELBv2CollectsLoadBalancersAndTargetGroups: both, and not a Gateway Load
// Balancer, which routes packets to appliances rather than requests to
// workloads — reported, not collected.
func TestELBv2CollectsLoadBalancersAndTargetGroups(t *testing.T) {
	out, tr := collectELBv2(t)
	want := []string{"elb/internal-jobs", "elb/orders-public", "elb/tg/fn-tg", "elb/tg/idle-tg", "elb/tg/jobs-tg", "elb/tg/orders-api"}
	if got := out.ids(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids:\n got %q\nwant %q", got, want)
	}
	if len(out.warnings) != 1 || out.warnings[0].Kind != "out-of-scope" || !strings.Contains(out.warnings[0].Message, "Gateway") {
		t.Errorf("the Gateway Load Balancer was not reported once: %+v", out.warnings)
	}
	if n := tr.count(elbSvc, "DescribeLoadBalancers"); n != 2 {
		t.Errorf("DescribeLoadBalancers called %d times, want 2 (one per page)", n)
	}
	if r, _ := out.byID("elb/orders-public"); r.Tags["team"] != "orders" {
		t.Errorf("tags: %v", r.Tags)
	}
}

// TestRulesAreTheDefinition: which path reaches which target group, in the
// order the load balancer applies them — the default last, however the API
// returns them.
func TestRulesAreTheDefinition(t *testing.T) {
	out, _ := collectELBv2(t)
	s := rdsSpec[spec.ELB](t, out, "elb/orders-public")
	if s.Type != "application" || s.Scheme != "internet-facing" || !strings.HasPrefix(s.DNSName, "orders-public-") || len(s.Subnets) != 2 {
		t.Errorf("load balancer: %+v", s)
	}
	if len(s.Listeners) != 2 || s.Listeners[0].Port != 80 || s.Listeners[1].Port != 443 || len(s.Listeners[1].Certificates) != 1 {
		t.Fatalf("listeners: %+v", s.Listeners)
	}
	// The listener's own ARN: what a VPC link integration names.
	if !strings.Contains(s.Listeners[1].ARN, ":listener/app/orders-public/") || s.Listeners[0].ARN == s.Listeners[1].ARN {
		t.Errorf("listener ARNs: %q %q", s.Listeners[0].ARN, s.Listeners[1].ARN)
	}
	var prios []string
	for _, r := range s.Listeners[1].Rules {
		prios = append(prios, r.Priority)
	}
	// Numerically: as text, "100" would come before "20".
	if strings.Join(prios, ",") != "10,20,30,100,default" {
		t.Errorf("rule order: %v", prios)
	}
	api := s.Listeners[1].Rules[0]
	if api.Conditions[0].Field != "path-pattern" || api.Conditions[0].Values[0] != "/api/*" ||
		!strings.HasSuffix(api.Actions[0].TargetGroups[0].ARN, "targetgroup/orders-api/8f2b1c") {
		t.Errorf("rule 10: %+v", api)
	}
	if d := s.Listeners[1].Rules[4]; d.Actions[0].Type != "fixed-response" || d.Actions[0].FixedStatus != "404" {
		t.Errorf("default: %+v", d)
	}
	if auth := s.Listeners[1].Rules[1].Actions[0]; auth.Type != "authenticate-oidc" || auth.Issuer != "https://auth.example.com" {
		t.Errorf("an authenticate action names its issuer: %+v", auth)
	}
	n := rdsSpec[spec.ELB](t, out, "elb/internal-jobs")
	if n.Type != "network" || len(n.Listeners) != 1 || len(n.Listeners[0].Rules) != 1 || n.Listeners[0].Rules[0].Priority != "default" ||
		!strings.Contains(n.Listeners[0].Rules[0].Actions[0].TargetGroups[0].ARN, "jobs-tg") {
		t.Errorf("a Network Load Balancer has only its default: %+v", n.Listeners)
	}
}

// TestRuleSecretsNeverLeave: a header condition holding a CDN's shared secret,
// an OIDC client secret, a fixed response's body and a signed redirect query
// are planted in the fixture. None may reach the serialized resource — Raw is
// written to disk too.
func TestRuleSecretsNeverLeave(t *testing.T) {
	out, _ := collectELBv2(t)
	r, _ := out.byID("elb/orders-public")
	// Spec and Raw as they are written — json.Marshal would escape the
	// marker's "<" and hide it from the search (M1 defect 3).
	all := string(r.Spec) + string(r.Raw)
	for name, secret := range map[string]string{
		"header value":   "cdn-" + "shared-" + "value-9f8e7d",
		"client secret":  "not-a-real-" + "client-secret",
		"fixed body":     "internal note: ask ops",
		"redirect query": "sig-" + "planted-0011",
	} {
		if strings.Contains(all, secret) {
			t.Errorf("%s reached the resource", name)
		}
	}
	if !strings.Contains(all, "<redacted:") {
		t.Error("nothing was marked as redacted")
	}
}

// TestLambdaTargetsAreRead: only the target group knows its Lambda targets;
// ip targets are ephemeral task addresses and are not read.
func TestLambdaTargetsAreRead(t *testing.T) {
	out, tr := collectELBv2(t)
	fn := rdsSpec[spec.TargetGroup](t, out, "elb/tg/fn-tg")
	if fn.TargetType != "lambda" || len(fn.Targets) != 1 || fn.Targets[0].ID != "lambda/webhook-receiver" || len(fn.LoadBalancerARNs) != 1 {
		t.Errorf("lambda target group: %+v", fn)
	}
	if n := tr.count(elbSvc, "DescribeTargetHealth"); n != 1 {
		t.Errorf("DescribeTargetHealth called %d times, want 1 (the Lambda target group only)", n)
	}
	if idle := rdsSpec[spec.TargetGroup](t, out, "elb/tg/idle-tg"); len(idle.LoadBalancerARNs) != 0 {
		t.Errorf("idle: %+v", idle)
	}
}

func TestELBv2UsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectELBv2(t)
	assertOpsMatchAllowList(t, elbSvc, tr)
}

func TestELBv2IDs(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/app/web/50dc6c495c0c9188":          "elb/web",
		"arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/net/tcp/73e2d6bc24d8a067":          "elb/tcp",
		"arn:aws:elasticloadbalancing:us-east-1:1:listener/app/web/50dc6c495c0c9188/f2f7dc8efc52": "elb/web",
		"arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/api/8f2b1c":                         "elb/tg/api",
		"arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/gwy/fw/1f2e3d4c5b6a7980":           "",
	} {
		if got := resourceIDFromARN(arn); got != want {
			t.Errorf("%s: got %q want %q", arn, got, want)
		}
	}
}
