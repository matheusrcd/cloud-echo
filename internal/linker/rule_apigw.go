package linker

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

var apiTypes = []string{spec.TypeRESTAPI, spec.TypeHTTPAPI, spec.TypeWebSocketAPI}

// apiEntrypointRule: every API is reachable from outside the graph.
type apiEntrypointRule struct{}

func (apiEntrypointRule) Name() string { return "apigw.entrypoint" }
func (apiEntrypointRule) Tier() int    { return 1 }

func (apiEntrypointRule) Apply(c *Context) {
	for _, typ := range apiTypes {
		Each(c, typ, func(r inventory.Resource, s *spec.API) {
			reason := fmt.Sprintf("%s API", s.Protocol)
			if s.EndpointType != "" {
				reason += fmt.Sprintf(" (%s)", s.EndpointType)
			}
			if s.DefaultEndpointDisabled {
				reason += ", default endpoint disabled: reachable through a custom domain or VPC endpoint only"
			}
			c.Trigger(r.ID, reason)
		})
	}
}

// apiIntegrationRule: a route hands the request to its integration's target.
type apiIntegrationRule struct{}

func (apiIntegrationRule) Name() string { return "apigw.integration" }
func (apiIntegrationRule) Tier() int    { return 1 }

func (apiIntegrationRule) Apply(c *Context) {
	for _, typ := range apiTypes {
		Each(c, typ, func(r inventory.Resource, s *spec.API) {
			for _, rt := range s.Routes {
				it := rt.Integration
				if it == nil {
					continue
				}
				source := integrationSource(r.Type, s.APIID, rt, it)
				if it.Templated {
					linkTemplated(c, r.ID, s, rt, it, source)
					continue
				}
				linkIntegration(c, r.ID, rt, it, it.Target, it.Qualifier, source, "")
			}
		})
	}
}

// linkIntegration draws the edge for one resolved integration. note carries
// context such as the stage that resolved a templated URI.
func linkIntegration(c *Context, api string, rt spec.Route, it *spec.Integration, target *spec.TargetRef, qualifier, source, note string) {
	detail := func(to string) string {
		d := fmt.Sprintf("%s %s → %s", it.Type, rt.RouteKey, to)
		if qualifier != "" {
			d += ":" + qualifier
		}
		return d + note
	}
	switch it.Service {
	case "mock":
		// Answered by API Gateway itself: nothing downstream.
	case "lambda", "sqs":
		kind := KindInvoke
		if it.Service == "sqs" {
			kind = KindPublish
		}
		if to, ok := c.Local(api, target, "integration "+rt.RouteKey); ok {
			c.Edge(api, to, kind, Certain, Active, source, detail(strings.TrimPrefix(to, "lambda/")))
		}
	case "http":
		// Through a VPC link the URL is an internal load balancer, not a third
		// party: it must not become an ext/ node the gateway would mock.
		if it.ConnectionType == "VPC_LINK" {
			// A REST API's VPC link targets a Network Load Balancer, named by
			// its DNS name in the URI.
			x := c.elbs()
			if id, ok := x.byDNS[strings.ToLower(hostOf(it.URI))]; ok {
				c.Edge(api, id, KindHTTP, Certain, Active, source, detail(id)+" (through a VPC link)")
				return
			}
			c.Unresolved(api, it.URI, fmt.Sprintf("integration %s: VPC link target is not a load balancer in the inventory", rt.RouteKey))
			return
		}
		if to, ok := c.External(it.URI); ok {
			c.Edge(api, to, KindHTTP, Certain, Active, source, detail(strings.TrimPrefix(to, "ext/")))
		}
	case "elasticloadbalancing":
		// An HTTP API's VPC link names a listener; the listener belongs to
		// its load balancer — if the load balancer still has it.
		switch id, listed := c.elbs().byListener(it.URI); {
		case listed:
			c.Edge(api, id, KindHTTP, Certain, Active, source, detail(id)+" (through a VPC link, listener "+lastSegment(it.URI)+")")
		case id != "":
			c.Unresolved(api, it.URI, fmt.Sprintf("integration %s: VPC link to listener %s, which %s does not have (deleted): requests fail", rt.RouteKey, lastSegment(it.URI), id))
		default:
			c.Unresolved(api, it.URI, fmt.Sprintf("integration %s: VPC link to a listener of a load balancer not in the inventory", rt.RouteKey))
		}
	case "servicediscovery":
		c.Unresolved(api, it.URI, fmt.Sprintf("integration %s: VPC link target needs the Cloud Map collector", rt.RouteKey))
	default:
		target := it.Service
		if it.Action != "" {
			target += ":" + it.Action
		}
		c.Unresolved(api, target, fmt.Sprintf("integration %s: AWS service integration whose target is not in the definition (mapping templates are withheld) or not collected", rt.RouteKey))
	}
}

var stageVarRef = regexp.MustCompile(`\$\{stageVariables\.([A-Za-z0-9_]+)\}`)

// linkTemplated resolves an integration URI built from stage variables, one
// stage at a time. A stage that does not define every variable contributes
// nothing; if no stage does, the integration is reported, never guessed.
func linkTemplated(c *Context, api string, s *spec.API, rt spec.Route, it *spec.Integration, source string) {
	var resolved bool
	for _, st := range s.Stages {
		uri, ok := substitute(it.URI, st.Variables)
		if !ok {
			continue
		}
		cp := *it
		cp.URI, cp.Templated = uri, false
		var target *spec.TargetRef
		var q string
		switch {
		case strings.HasPrefix(uri, "arn:aws:apigateway:"):
			target, q = spec.LambdaFromInvokeURI(uri)
			cp.Service = "lambda"
		case strings.HasPrefix(uri, "arn:aws:lambda:"):
			if name, qq := spec.FunctionFromARN(uri); name != "" {
				target, q = &spec.TargetRef{ARN: uri, ID: "lambda/" + name}, qq
			}
			cp.Service = "lambda"
		case strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://"):
			cp.Service = "http"
		}
		if cp.Service == "" {
			continue
		}
		resolved = true
		linkIntegration(c, api, rt, &cp, target, q, source, fmt.Sprintf(" (stage %s resolves %s)", st.Name, it.URI))
	}
	if !resolved {
		vars := stageVarRef.FindAllStringSubmatch(it.URI, -1)
		var names []string
		for _, v := range vars {
			names = append(names, "stageVariables."+v[1])
		}
		sort.Strings(names)
		c.Unresolved(api, it.URI, fmt.Sprintf("integration %s: no stage defines %s", rt.RouteKey, strings.Join(names, ", ")))
	}
}

func substitute(uri string, vars map[string]string) (string, bool) {
	ok := true
	out := stageVarRef.ReplaceAllStringFunc(uri, func(m string) string {
		name := stageVarRef.FindStringSubmatch(m)[1]
		v, has := vars[name]
		if !has || strings.HasPrefix(v, "<redacted:") {
			ok = false
			return m
		}
		return v
	})
	return out, ok
}

func integrationSource(typ, apiID string, rt spec.Route, it *spec.Integration) string {
	if typ == spec.TypeRESTAPI {
		return fmt.Sprintf("apigateway:GetResources %s %s", apiID, rt.RouteKey)
	}
	return fmt.Sprintf("apigatewayv2:GetIntegrations %s %s (route %s)", apiID, it.ID, rt.RouteKey)
}

// apiAuthorizerRule: an authorizer function runs in the request path of every
// route it guards, so it is a synchronous edge. An authorizer no route uses runs
// for nothing and produces no edge.
type apiAuthorizerRule struct{}

func (apiAuthorizerRule) Name() string { return "apigw.authorizer" }
func (apiAuthorizerRule) Tier() int    { return 1 }

func (apiAuthorizerRule) Apply(c *Context) {
	for _, typ := range apiTypes {
		Each(c, typ, func(r inventory.Resource, s *spec.API) {
			for _, a := range s.Authorizers {
				if a.Function == nil {
					continue // JWT and Cognito authorizers call no function of ours
				}
				var routes []string
				for _, rt := range s.Routes {
					if rt.AuthorizerID == a.ID {
						routes = append(routes, rt.RouteKey)
					}
				}
				if len(routes) == 0 {
					continue
				}
				if to, ok := c.Local(r.ID, a.Function, "authorizer "+a.Name); ok {
					c.Edge(r.ID, to, KindInvoke, Certain, Active,
						fmt.Sprintf("apigateway:GetAuthorizers %s %s", s.APIID, a.ID),
						fmt.Sprintf("%s authorizer %s guards %s", a.Type, a.Name, strings.Join(routes, ", ")))
				}
			}
		})
	}
}
