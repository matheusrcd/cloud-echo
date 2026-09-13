package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwv2types "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// API Gateway is two services with two models — REST APIs (v1) are a tree of
// resources with methods, HTTP and WebSocket APIs (v2) are routes pointing at
// shared integrations — and one normalized spec. The linker should see the same
// thing either way: route → integration → target.
//
// Scope decisions, each checked against a real account:
//
//   - IDs are the API id, not the name. API Gateway does not enforce unique names
//     (create-rest-api with an existing name makes a second API). Ids are unique
//     across v1 and v2, which share the {id}.execute-api hostname namespace.
//   - The routes are the API's current definition. A REST API stage serves a
//     deployment snapshot, which can lag behind undeployed edits; that drift is
//     not detected yet.
//   - Mapping templates are not kept. They are free text that can hold literal
//     credentials; only their content types are recorded, and their bodies are
//     replaced in Raw.

const (
	restPageSize = 500   // GetRestApis / GetResources / GetAuthorizers maximum
	v2PageSize   = "500" // v2 MaxResults is a string
)

// ---------------------------------------------------------------- v1

// APIGateway collects REST APIs.
type APIGateway struct{}

func (*APIGateway) Service() string { return "API Gateway" }

func (c *APIGateway) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := apigateway.NewFromConfig(s.Config())

	p := apigateway.NewGetRestApisPaginator(api, &apigateway.GetRestApisInput{Limit: aws.Int32(restPageSize)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return warnOrFail(out, "API Gateway", "GetRestApis", err)
		}
		for i := range page.Items {
			if err := c.collectAPI(ctx, api, s, out, &page.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *APIGateway) collectAPI(ctx context.Context, api *apigateway.Client, s *awsx.Session, out Emitter, ra *apigwtypes.RestApi) error {
	id := aws.ToString(ra.Id)
	spec := apiSpec{
		APIID:    id,
		Name:     aws.ToString(ra.Name),
		Protocol: "REST",
	}
	if ec := ra.EndpointConfiguration; ec != nil && len(ec.Types) > 0 {
		spec.EndpointType = string(ec.Types[0])
	}
	spec.DefaultEndpointDisabled = ra.DisableExecuteApiEndpoint
	if !spec.DefaultEndpointDisabled {
		spec.Endpoint = fmt.Sprintf("https://%s.execute-api.%s.amazonaws.com", id, s.Region())
	}
	if pol, err := decodeRestAPIPolicy(aws.ToString(ra.Policy)); err != nil {
		out.Warn(inventory.Warning{Service: "API Gateway", Op: "GetRestApis", Kind: "unparseable",
			Message: id + " resource policy: " + err.Error()})
	} else {
		spec.ResourcePolicy = pol
	}

	var resources []apigwtypes.Resource
	rp := apigateway.NewGetResourcesPaginator(api, &apigateway.GetResourcesInput{
		RestApiId: aws.String(id),
		// embed=methods returns every method with its integration, so there is
		// no GetMethod/GetIntegration per method. Confirmed against the real
		// API; on a throttled control plane that is the difference between one
		// call per page and two per method.
		Embed: []string{"methods"},
		Limit: aws.Int32(restPageSize),
	})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "API Gateway", "GetResources", err); err != nil {
				return err
			}
			break
		}
		resources = append(resources, page.Items...)
	}
	sort.Slice(resources, func(i, j int) bool { return aws.ToString(resources[i].Path) < aws.ToString(resources[j].Path) })

	for ri := range resources {
		r := &resources[ri]
		methods := make([]string, 0, len(r.ResourceMethods))
		for m := range r.ResourceMethods {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		for _, m := range methods {
			meth := r.ResourceMethods[m]
			path := aws.ToString(r.Path)
			route := routeSpec{
				RouteKey:       m + " " + path,
				Method:         m,
				Path:           path,
				Authorization:  aws.ToString(meth.AuthorizationType),
				AuthorizerID:   aws.ToString(meth.AuthorizerId),
				APIKeyRequired: aws.ToBool(meth.ApiKeyRequired),
			}
			if in := meth.MethodIntegration; in != nil {
				where := "route:" + route.RouteKey
				it := &integrationSpec{
					Type:           string(in.Type),
					URI:            aws.ToString(in.Uri),
					HTTPMethod:     aws.ToString(in.HttpMethod),
					ConnectionType: string(in.ConnectionType),
					ConnectionID:   aws.ToString(in.ConnectionId),
					Credentials:    aws.ToString(in.Credentials),
					TimeoutMs:      in.TimeoutInMillis,
				}
				redactMappings(where, in.RequestParameters, &spec.Redacted)
				it.RequestParameters = copyMap(in.RequestParameters)
				it.TemplateContentTypes = omitTemplates(in.RequestTemplates)
				for code, ir := range in.IntegrationResponses {
					redactMappings(where+":response:"+code, ir.ResponseParameters, &spec.Redacted)
					omitTemplates(ir.ResponseTemplates)
				}
				resolveIntegration(it)
				route.Integration = it
				// Write the redacted/omitted maps back: map values in the SDK
				// struct were already mutated in place, so Raw matches the spec.
				r.ResourceMethods[m] = meth
			}
			spec.Routes = append(spec.Routes, route)
		}
	}

	stages, err := api.GetStages(ctx, &apigateway.GetStagesInput{RestApiId: aws.String(id)})
	if err != nil {
		if err := warnOrFail(out, "API Gateway", "GetStages", err); err != nil {
			return err
		}
		stages = &apigateway.GetStagesOutput{}
	}
	for i := range stages.Item {
		st := &stages.Item[i]
		name := aws.ToString(st.StageName)
		redactStageVariables(name, st.Variables, &spec.Redacted)
		spec.Stages = append(spec.Stages, stageSpec{
			Name:         name,
			DeploymentID: aws.ToString(st.DeploymentId),
			Variables:    copyMap(st.Variables),
		})
	}

	var auths []apigwtypes.Authorizer
	var pos *string
	for {
		page, err := api.GetAuthorizers(ctx, &apigateway.GetAuthorizersInput{
			RestApiId: aws.String(id), Limit: aws.Int32(restPageSize), Position: pos,
		})
		if err != nil {
			if err := warnOrFail(out, "API Gateway", "GetAuthorizers", err); err != nil {
				return err
			}
			break
		}
		auths = append(auths, page.Items...)
		if aws.ToString(page.Position) == "" {
			break
		}
		pos = page.Position
	}
	for _, a := range auths {
		as := authorizerSpec{
			ID:          aws.ToString(a.Id),
			Name:        aws.ToString(a.Name),
			Type:        string(a.Type),
			Credentials: aws.ToString(a.AuthorizerCredentials),
			Providers:   a.ProviderARNs,
		}
		if src := aws.ToString(a.IdentitySource); src != "" {
			as.IdentitySource = strings.Split(src, ",")
		}
		as.Function, as.Qualifier = lambdaFromInvokeURI(aws.ToString(a.AuthorizerUri))
		spec.Authorizers = append(spec.Authorizers, as)
	}

	finishAPISpec(&spec)
	out.Emit(newResource(s, resourceArgs{
		ID:   "apigw/" + id,
		Type: "apigateway.rest",
		ARN:  fmt.Sprintf("arn:aws:apigateway:%s::/restapis/%s", s.Region(), id),
		Name: spec.Name,
		Tags: ra.Tags,
		API:  "apigateway:GetRestApis",
		Spec: spec,
		Raw: map[string]any{
			"api":         ra,
			"resources":   resources,
			"stages":      stages.Item,
			"authorizers": auths,
		},
	}))
	return nil
}

// decodeRestAPIPolicy undoes the double encoding of a REST API resource policy.
//
// Against a real account the field arrives as {\"Version\":…\/*…}: escaped
// quotes *and* escaped slashes, i.e. the body of a JSON string literal rather
// than JSON. strconv.Unquote cannot read it — \/ is not a Go escape — so it is
// decoded as a JSON string first. Already-plain JSON passes through.
func decodeRestAPIPolicy(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if json.Valid([]byte(v)) {
		return json.RawMessage(v), nil
	}
	var unescaped string
	if err := json.Unmarshal([]byte(`"`+v+`"`), &unescaped); err != nil {
		return nil, fmt.Errorf("resource policy is neither JSON nor an escaped JSON string: %w", err)
	}
	if !json.Valid([]byte(unescaped)) {
		return nil, fmt.Errorf("resource policy is not valid JSON after unescaping")
	}
	return json.RawMessage(unescaped), nil
}

// ---------------------------------------------------------------- v2

// APIGatewayV2 collects HTTP and WebSocket APIs.
type APIGatewayV2 struct{}

func (*APIGatewayV2) Service() string { return "ApiGatewayV2" }

// Every v2 list call is driven by hand — the SDK ships no paginators for this
// service — and always sends MaxResults. Like SQS ListQueues, v2 returns a
// NextToken only when MaxResults is set; confirmed against the real API, where
// the same GetRoutes call paged at MaxResults=2 and returned no token without it.
func (c *APIGatewayV2) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := apigatewayv2.NewFromConfig(s.Config())

	var next *string
	for {
		page, err := api.GetApis(ctx, &apigatewayv2.GetApisInput{MaxResults: aws.String(v2PageSize), NextToken: next})
		if err != nil {
			return warnOrFail(out, "ApiGatewayV2", "GetApis", err)
		}
		for i := range page.Items {
			if err := c.collectAPI(ctx, api, s, out, &page.Items[i]); err != nil {
				return err
			}
		}
		if aws.ToString(page.NextToken) == "" {
			return nil
		}
		next = page.NextToken
	}
}

func (c *APIGatewayV2) collectAPI(ctx context.Context, api *apigatewayv2.Client, s *awsx.Session, out Emitter, a *apigwv2types.Api) error {
	id := aws.ToString(a.ApiId)
	spec := apiSpec{
		APIID:                   id,
		Name:                    aws.ToString(a.Name),
		Protocol:                string(a.ProtocolType),
		EndpointType:            "REGIONAL",
		DefaultEndpointDisabled: aws.ToBool(a.DisableExecuteApiEndpoint),
	}
	if !spec.DefaultEndpointDisabled {
		spec.Endpoint = aws.ToString(a.ApiEndpoint)
	}

	integrations, err := v2List(ctx, func(next *string) ([]apigwv2types.Integration, *string, error) {
		o, err := api.GetIntegrations(ctx, &apigatewayv2.GetIntegrationsInput{ApiId: aws.String(id), MaxResults: aws.String(v2PageSize), NextToken: next})
		if err != nil {
			return nil, nil, err
		}
		return o.Items, o.NextToken, nil
	})
	if err := warnOrFail(out, "ApiGatewayV2", "GetIntegrations", err); err != nil {
		return err
	}
	byID := map[string]*integrationSpec{}
	for i := range integrations {
		in := &integrations[i]
		iid := aws.ToString(in.IntegrationId)
		where := "integration:" + iid
		it := &integrationSpec{
			ID:             iid,
			Type:           string(in.IntegrationType),
			Subtype:        aws.ToString(in.IntegrationSubtype),
			URI:            aws.ToString(in.IntegrationUri),
			HTTPMethod:     aws.ToString(in.IntegrationMethod),
			ConnectionType: string(in.ConnectionType),
			ConnectionID:   aws.ToString(in.ConnectionId),
			Credentials:    aws.ToString(in.CredentialsArn),
			TimeoutMs:      aws.ToInt32(in.TimeoutInMillis),
			PayloadFormat:  aws.ToString(in.PayloadFormatVersion),
		}
		redactMappings(where, in.RequestParameters, &spec.Redacted)
		it.RequestParameters = copyMap(in.RequestParameters)
		it.TemplateContentTypes = omitTemplates(in.RequestTemplates)
		for code, rp := range in.ResponseParameters {
			redactMappings(where+":response:"+code, rp, &spec.Redacted)
		}
		resolveIntegration(it)
		byID[iid] = it
	}

	routes, err := v2List(ctx, func(next *string) ([]apigwv2types.Route, *string, error) {
		o, err := api.GetRoutes(ctx, &apigatewayv2.GetRoutesInput{ApiId: aws.String(id), MaxResults: aws.String(v2PageSize), NextToken: next})
		if err != nil {
			return nil, nil, err
		}
		return o.Items, o.NextToken, nil
	})
	if err := warnOrFail(out, "ApiGatewayV2", "GetRoutes", err); err != nil {
		return err
	}
	sort.Slice(routes, func(i, j int) bool { return aws.ToString(routes[i].RouteKey) < aws.ToString(routes[j].RouteKey) })
	for _, r := range routes {
		key := aws.ToString(r.RouteKey)
		route := routeSpec{
			RouteKey:       key,
			Authorization:  string(r.AuthorizationType),
			AuthorizerID:   aws.ToString(r.AuthorizerId),
			APIKeyRequired: aws.ToBool(r.ApiKeyRequired),
		}
		// "POST /orders" splits into method and path; "$default", "$connect"
		// and other WebSocket keys have neither and keep only the key.
		if m, p, ok := strings.Cut(key, " "); ok {
			route.Method, route.Path = m, p
		}
		// Several routes can share one integration; each route carries its own
		// copy so the linker never has to join, and the id records the sharing.
		if iid, ok := strings.CutPrefix(aws.ToString(r.Target), "integrations/"); ok && byID[iid] != nil {
			cp := *byID[iid]
			route.Integration = &cp
		}
		spec.Routes = append(spec.Routes, route)
	}

	stages, err := v2List(ctx, func(next *string) ([]apigwv2types.Stage, *string, error) {
		o, err := api.GetStages(ctx, &apigatewayv2.GetStagesInput{ApiId: aws.String(id), MaxResults: aws.String(v2PageSize), NextToken: next})
		if err != nil {
			return nil, nil, err
		}
		return o.Items, o.NextToken, nil
	})
	if err := warnOrFail(out, "ApiGatewayV2", "GetStages", err); err != nil {
		return err
	}
	for i := range stages {
		st := &stages[i]
		name := aws.ToString(st.StageName)
		redactStageVariables(name, st.StageVariables, &spec.Redacted)
		spec.Stages = append(spec.Stages, stageSpec{
			Name:         name,
			DeploymentID: aws.ToString(st.DeploymentId),
			AutoDeploy:   aws.ToBool(st.AutoDeploy),
			Variables:    copyMap(st.StageVariables),
		})
	}

	auths, err := v2List(ctx, func(next *string) ([]apigwv2types.Authorizer, *string, error) {
		o, err := api.GetAuthorizers(ctx, &apigatewayv2.GetAuthorizersInput{ApiId: aws.String(id), MaxResults: aws.String(v2PageSize), NextToken: next})
		if err != nil {
			return nil, nil, err
		}
		return o.Items, o.NextToken, nil
	})
	if err := warnOrFail(out, "ApiGatewayV2", "GetAuthorizers", err); err != nil {
		return err
	}
	for _, au := range auths {
		as := authorizerSpec{
			ID:             aws.ToString(au.AuthorizerId),
			Name:           aws.ToString(au.Name),
			Type:           string(au.AuthorizerType),
			Credentials:    aws.ToString(au.AuthorizerCredentialsArn),
			IdentitySource: au.IdentitySource,
		}
		if j := au.JwtConfiguration; j != nil {
			as.JWTIssuer = aws.ToString(j.Issuer)
			as.JWTAudience = j.Audience
		}
		as.Function, as.Qualifier = lambdaFromInvokeURI(aws.ToString(au.AuthorizerUri))
		spec.Authorizers = append(spec.Authorizers, as)
	}

	typ := "apigateway.http"
	if spec.Protocol == "WEBSOCKET" {
		typ = "apigateway.websocket"
	}
	finishAPISpec(&spec)
	out.Emit(newResource(s, resourceArgs{
		ID:   "apigw/" + id,
		Type: typ,
		ARN:  fmt.Sprintf("arn:aws:apigateway:%s::/apis/%s", s.Region(), id),
		Name: spec.Name,
		Tags: a.Tags,
		API:  "apigatewayv2:GetApis",
		Spec: spec,
		Raw: map[string]any{
			"api":          a,
			"integrations": integrations,
			"routes":       routes,
			"stages":       stages,
			"authorizers":  auths,
		},
	}))
	return nil
}

// finishAPISpec puts every list in a canonical order. AWS does not promise one:
// scanning the same HTTP API from AWS and from Floci returned its stages in
// opposite orders, and an order-dependent spec turns every scan --diff into
// noise. Routes are always a list, never null, so consumers see one shape.
func finishAPISpec(s *apiSpec) {
	if s.Routes == nil {
		s.Routes = []routeSpec{}
	}
	sort.SliceStable(s.Routes, func(i, j int) bool { return s.Routes[i].RouteKey < s.Routes[j].RouteKey })
	sort.SliceStable(s.Stages, func(i, j int) bool { return s.Stages[i].Name < s.Stages[j].Name })
	sort.SliceStable(s.Authorizers, func(i, j int) bool {
		if s.Authorizers[i].Name != s.Authorizers[j].Name {
			return s.Authorizers[i].Name < s.Authorizers[j].Name
		}
		return s.Authorizers[i].ID < s.Authorizers[j].ID
	})
	sort.Strings(s.Redacted)
}

// v2List drains a NextToken-paged v2 list call.
func v2List[T any](ctx context.Context, call func(next *string) ([]T, *string, error)) ([]T, error) {
	var all []T
	var next *string
	for {
		items, tok, err := call(next)
		if err != nil {
			return all, err
		}
		all = append(all, items...)
		if aws.ToString(tok) == "" {
			return all, nil
		}
		if err := ctx.Err(); err != nil {
			return all, err
		}
		next = tok
	}
}

// ---------------------------------------------------------------- normalized spec

// ---------------------------------------------------------------- resolution

// resolveIntegration names what an integration calls, from its definition alone.
func resolveIntegration(it *integrationSpec) {
	if strings.Contains(it.URI, "${") {
		it.Templated = true
		return
	}

	// v2 service integrations carry no URI: the subtype names the call and the
	// target sits in the request parameters (confirmed for SQS-SendMessage).
	if it.Subtype != "" {
		svc, action, _ := strings.Cut(it.Subtype, "-")
		it.Service, it.Action = strings.ToLower(svc), action
		if it.Service == "sqs" {
			if q := it.RequestParameters["QueueUrl"]; strings.HasPrefix(q, "https://") {
				it.Target = queueTargetFromURL(q)
			}
		}
		return
	}

	switch {
	case it.Type == "MOCK":
		it.Service = "mock"

	case strings.HasPrefix(it.URI, "arn:aws:apigateway:"):
		// arn:aws:apigateway:<region>:<service>:<path|action>/<rest>
		parts := strings.SplitN(it.URI, ":", 6)
		if len(parts) < 6 {
			return
		}
		it.Service = parts[4]
		kind, rest, _ := strings.Cut(parts[5], "/")
		switch {
		case it.Service == "lambda":
			it.Target, it.Qualifier = lambdaFromInvokeURI(it.URI)
		case it.Service == "sqs" && kind == "path":
			// path/<account>/<queue>
			if acct, queue, ok := strings.Cut(rest, "/"); ok && queue != "" {
				arn := fmt.Sprintf("arn:aws:sqs:%s:%s:%s", parts[3], acct, queue)
				it.Target = &targetRef{ARN: arn, ID: resourceIDFromARN(arn)}
			}
		case kind == "action":
			it.Action = rest
		}

	case strings.HasPrefix(it.URI, "arn:aws:lambda:"):
		it.Service = "lambda"
		name, q := functionNameFromARN(it.URI)
		if name != "" {
			it.Target = &targetRef{ARN: it.URI, ID: "lambda/" + name}
			it.Qualifier = q
		}

	case strings.HasPrefix(it.URI, "http://") || strings.HasPrefix(it.URI, "https://"):
		// An external or VPC-linked HTTP backend. No inventory target: the
		// linker turns the URL into an ext/ node, or — through a VPC link —
		// finds the Network Load Balancer its host names.
		it.Service = "http"

	case strings.HasPrefix(it.URI, "arn:aws:elasticloadbalancing:"):
		it.Service = "elasticloadbalancing"
		it.Target = &targetRef{ARN: it.URI}

	case strings.HasPrefix(it.URI, "arn:aws:servicediscovery:"):
		it.Service = "servicediscovery"
		it.Target = &targetRef{ARN: it.URI}
	}
}

// ---------------------------------------------------------------- redaction

// isMappingExpression reports whether a parameter value refers to the request
// or context rather than holding a literal. References are never secrets:
// "method.request.header.Authorization" names where a token comes from, it does
// not contain one.
func isMappingExpression(v string) bool {
	for _, p := range []string{"method.", "context.", "stageVariables.", "integration.",
		"$request.", "$context.", "$stageVariables.", "$util."} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// redactMappings withholds literal values in a parameter mapping, in place, so
// Raw and the spec carry the same redactions. REST APIs quote literals
// ('value'); v2 does not.
func redactMappings(where string, m map[string]string, redacted *[]string) {
	for k, v := range m {
		if isMappingExpression(v) {
			continue
		}
		lit := strings.Trim(v, "'")
		if r, red := redactValue(k, lit); red {
			m[k] = r
			*redacted = append(*redacted, where+":"+k)
		}
	}
}

func redactStageVariables(stage string, vars map[string]string, redacted *[]string) {
	for k, v := range vars {
		if r, red := redactValue(k, v); red {
			vars[k] = r
			*redacted = append(*redacted, "stage:"+stage+":"+k)
		}
	}
}

// omitTemplates replaces mapping-template bodies with a marker, in place, and
// returns their content types. Templates are free text — VTL that can embed a
// literal credential anywhere — so they are withheld wholesale rather than
// pattern-matched.
func omitTemplates(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	types := make([]string, 0, len(m))
	for k := range m {
		m[k] = marker("template")
		types = append(types, k)
	}
	sort.Strings(types)
	return types
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
