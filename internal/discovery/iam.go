package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// IAM collects the roles collected workloads assume, and every policy that shapes
// what those roles may do.
//
// It is the Tier-3 input, and the only one that yields direction and intent:
// Tier 2 can tell that a service knows a table's name, only IAM can tell that it
// writes to it. See docs/03-linker.md.
//
// Scope is deliberately narrow. The roles read are the ECS task roles and Lambda
// execution roles found in phase 1 — the identities the application code runs
// as. ECS *execution* roles are skipped: they belong to the ECS agent, which uses
// them to pull images and inject secrets[], and reading them as the application's
// permissions would make every service appear to read every secret the agent
// fetches on its behalf. That relationship is already a Tier-1 declaration on the
// task definition.
type IAM struct{}

func (*IAM) Service() string { return "IAM" }

func (c *IAM) CollectFrom(ctx context.Context, s *awsx.Session, prior []inventory.Resource, out Emitter) error {
	api := iam.NewFromConfig(s.Config())

	refs := assumedRoles(prior, s.AccountID(), out)
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	// Managed policies are shared — AWSLambdaBasicExecutionRole is attached to
	// most Lambda roles in most accounts — so they are gathered across roles and
	// read once each.
	policies := map[string]struct{}{}

	for _, name := range names {
		if err := c.collectRole(ctx, api, s, out, name, refs[name], policies); err != nil {
			return err
		}
	}

	arns := make([]string, 0, len(policies))
	for a := range policies {
		arns = append(arns, a)
	}
	sort.Strings(arns)
	for _, arn := range arns {
		if err := c.collectPolicy(ctx, api, s, out, arn); err != nil {
			return err
		}
	}
	return nil
}

// assumedRoles maps role name → the inventory ids of the workloads assuming it.
//
// A role in another account cannot be read by name from this one — GetRole with
// that name would silently return a *different* role, if one existed locally
// with the same name. So cross-account references are reported and skipped,
// never looked up.
func assumedRoles(prior []inventory.Resource, account string, out Emitter) map[string][]string {
	refs := map[string][]string{}
	add := func(arn, referrer string) {
		if arn == "" {
			return
		}
		parts := strings.SplitN(arn, ":", 6)
		// Only roles are identities to read. API Gateway integrations also
		// accept arn:aws:iam::*:user/* — "use the caller's credentials" — which
		// is neither a role nor another account, and must not be reported as one.
		if len(parts) < 6 || parts[2] != "iam" || !strings.HasPrefix(parts[5], "role/") {
			return
		}
		if parts[4] != account {
			out.Warn(inventory.Warning{
				Service: "IAM",
				Kind:    "out-of-scope",
				Message: fmt.Sprintf("%s assumes %s, in account %s; it cannot be read from this scan", referrer, arn, parts[4]),
			})
			return
		}
		name := strings.TrimPrefix(roleIDFromARN(arn), "iam/role/")
		if name == "" {
			return
		}
		refs[name] = append(refs[name], referrer)
	}

	for _, r := range prior {
		switch r.Type {
		case "ecs.taskdefinition":
			var spec taskDefinitionSpec
			if json.Unmarshal(r.Spec, &spec) == nil {
				add(spec.TaskRoleARN, r.ID)
			}
		case "lambda.function":
			var spec functionSpec
			if json.Unmarshal(r.Spec, &spec) == nil {
				add(spec.RoleARN, r.ID)
			}
		case "apigateway.rest", "apigateway.http", "apigateway.websocket":
			// The roles API Gateway assumes to call a backend directly (an SQS
			// SendMessage integration) or to invoke an authorizer. They are what
			// Tier 3 needs to tell that an API writes to a queue.
			var spec apiSpec
			if json.Unmarshal(r.Spec, &spec) == nil {
				for _, rt := range spec.Routes {
					if rt.Integration != nil {
						add(rt.Integration.Credentials, r.ID)
					}
				}
				for _, a := range spec.Authorizers {
					add(a.Credentials, r.ID)
				}
			}
		}
	}
	// One API routes many methods through the same role; list it once.
	for n, rs := range refs {
		sort.Strings(rs)
		uniq := rs[:0]
		for i, v := range rs {
			if i == 0 || v != rs[i-1] {
				uniq = append(uniq, v)
			}
		}
		refs[n] = uniq
	}
	return refs
}

func (c *IAM) collectRole(
	ctx context.Context,
	api *iam.Client,
	s *awsx.Session,
	out Emitter,
	name string,
	assumedBy []string,
	policies map[string]struct{},
) error {
	resp, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		// A workload pointing at a role that no longer exists is a real
		// finding about the account — that workload cannot start — not a
		// reason to stop reading the others.
		if isNotFound(err) {
			out.Warn(inventory.Warning{
				Service: "IAM",
				Op:      "GetRole",
				Kind:    "dangling-reference",
				Message: fmt.Sprintf("role %s is assumed by %s but does not exist", name, strings.Join(assumedBy, ", ")),
			})
			return nil
		}
		return warnOrFail(out, "IAM", "GetRole", err)
	}
	role := resp.Role

	spec := roleSpec{
		RoleName:  name,
		Path:      aws.ToString(role.Path),
		AssumedBy: assumedBy,
	}
	spec.TrustPolicy = c.document(out, "GetRole", name+" trust policy", role.AssumeRolePolicyDocument)

	if pb := role.PermissionsBoundary; pb != nil && aws.ToString(pb.PermissionsBoundaryArn) != "" {
		arn := aws.ToString(pb.PermissionsBoundaryArn)
		spec.PermissionsBoundary = &targetRef{ARN: arn, ID: policyIDFromARN(arn)}
		policies[arn] = struct{}{}
	}

	inline := iam.NewListRolePoliciesPaginator(api, &iam.ListRolePoliciesInput{RoleName: aws.String(name)})
	var inlineNames []string
	for inline.HasMorePages() {
		page, err := inline.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "IAM", "ListRolePolicies", err); err != nil {
				return err
			}
			spec.Unread = append(spec.Unread, "inline policies")
			break
		}
		inlineNames = append(inlineNames, page.PolicyNames...)
	}
	sort.Strings(inlineNames)
	for _, pn := range inlineNames {
		doc, err := api.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(pn)})
		if err != nil {
			if err := warnOrFail(out, "IAM", "GetRolePolicy", err); err != nil {
				return err
			}
			spec.Unread = append(spec.Unread, "inline policy "+pn)
			continue
		}
		d := c.document(out, "GetRolePolicy", name+"/"+pn, doc.PolicyDocument)
		if d == nil {
			spec.Unread = append(spec.Unread, "inline policy "+pn)
			continue
		}
		spec.InlinePolicies = append(spec.InlinePolicies, inlinePolicy{Name: pn, Document: d})
	}

	attached := iam.NewListAttachedRolePoliciesPaginator(api, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(name)})
	for attached.HasMorePages() {
		page, err := attached.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "IAM", "ListAttachedRolePolicies", err); err != nil {
				return err
			}
			spec.Unread = append(spec.Unread, "attached policies")
			break
		}
		for _, ap := range page.AttachedPolicies {
			arn := aws.ToString(ap.PolicyArn)
			spec.AttachedPolicies = append(spec.AttachedPolicies, targetRef{ARN: arn, ID: policyIDFromARN(arn)})
			policies[arn] = struct{}{}
		}
	}
	sort.Slice(spec.AttachedPolicies, func(i, j int) bool { return spec.AttachedPolicies[i].ARN < spec.AttachedPolicies[j].ARN })

	out.Emit(newResource(s, resourceArgs{
		// Role names are unique per account regardless of path.
		ID:     "iam/role/" + name,
		Type:   "iam.role",
		ARN:    aws.ToString(role.Arn),
		Name:   name,
		Region: "global",
		Tags:   iamTags(role.Tags),
		API:    "iam:GetRole",
		Spec:   spec,
		Raw:    role,
	}))
	return nil
}

func (c *IAM) collectPolicy(ctx context.Context, api *iam.Client, s *awsx.Session, out Emitter, arn string) error {
	pol, err := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	if err != nil {
		if isNotFound(err) {
			out.Warn(inventory.Warning{
				Service: "IAM", Op: "GetPolicy", Kind: "dangling-reference",
				Message: fmt.Sprintf("policy %s is attached but does not exist", arn),
			})
			return nil
		}
		return warnOrFail(out, "IAM", "GetPolicy", err)
	}
	p := pol.Policy

	ver, err := api.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{
		PolicyArn: aws.String(arn),
		VersionId: p.DefaultVersionId,
	})
	if err != nil {
		return warnOrFail(out, "IAM", "GetPolicyVersion", err)
	}

	name := aws.ToString(p.PolicyName)
	out.Emit(newResource(s, resourceArgs{
		ID:     policyIDFromARN(arn),
		Type:   "iam.policy",
		ARN:    arn,
		Name:   name,
		Region: "global",
		Tags:   iamTags(p.Tags),
		API:    "iam:GetPolicyVersion",
		Spec: policySpec{
			PolicyName:     name,
			Path:           aws.ToString(p.Path),
			AWSManaged:     isAWSManaged(arn),
			DefaultVersion: aws.ToString(p.DefaultVersionId),
			Document:       c.document(out, "GetPolicyVersion", arn, ver.PolicyVersion.Document),
		},
		Raw: map[string]any{"policy": p, "version": ver.PolicyVersion},
	}))
	return nil
}

// document decodes an IAM policy document, warning rather than failing on one it
// cannot read: a single malformed document must not cost the rest of the role.
func (c *IAM) document(out Emitter, op, what string, raw *string) json.RawMessage {
	doc, err := decodePolicyDocument(aws.ToString(raw))
	if err != nil {
		out.Warn(inventory.Warning{Service: "IAM", Op: op, Kind: "unparseable", Message: what + ": " + err.Error()})
		return nil
	}
	return doc
}

// decodePolicyDocument undoes the URL encoding IAM applies to every policy
// document it returns (trust policies, inline policies, managed policy versions).
// The SDK does not decode it.
//
// It uses PathUnescape, not QueryUnescape. IAM encodes per RFC 3986, where a space
// is %20 and a plus is %2B. For such input the two agree; they differ only on a
// literal '+', which RFC 3986 reads as '+' and form decoding reads as a space.
// If a literal '+' ever arrives, PathUnescape is the one that keeps the document
// saying what it said. A value
// that already looks like JSON is passed through, so a decoded document containing
// a literal '%' is not mangled by a second decode.
func decodePolicyDocument(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if !strings.HasPrefix(v, "{") {
		dec, err := url.PathUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("url-decoding policy document: %w", err)
		}
		v = dec
	}
	if !json.Valid([]byte(v)) {
		return nil, fmt.Errorf("policy document is not valid JSON after decoding")
	}
	return json.RawMessage(v), nil
}

// policyIDFromARN keeps AWS-managed and customer-managed policies apart. A
// customer may create a policy named AmazonSQSFullAccess in their own account,
// and it would be a different document from AWS's; they must not share an id.
func policyIDFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[2] != "iam" {
		return ""
	}
	res, ok := strings.CutPrefix(parts[5], "policy/")
	if !ok || res == "" {
		return ""
	}
	if i := strings.LastIndex(res, "/"); i >= 0 {
		res = res[i+1:]
	}
	if parts[4] == "aws" {
		return "iam/aws-policy/" + res
	}
	return "iam/policy/" + res
}

func isAWSManaged(arn string) bool {
	parts := strings.SplitN(arn, ":", 6)
	return len(parts) >= 6 && parts[4] == "aws"
}

func iamTags(tags []iamtypes.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
