// Package awsx builds the AWS clients discovery uses, and enforces that they can
// only ever read.
//
// The allow-list in this file is the single source of truth for three things:
//
//  1. what the read-only middleware permits (guard.go),
//  2. what policies/cloud-echo-scanner.json grants (policy.go),
//  3. what collectors are allowed to call (checked by TestCollectorOpsAreAllowed).
//
// Adding an API call to a collector without adding it here fails the build's
// tests, in that order. That is deliberate: the list should be tedious to grow,
// because every entry is a permission a user has to grant us.
package awsx

import (
	"fmt"
	"sort"
	"strings"
)

// Service is one AWS service and the exact operations cloud-echo may call on it.
type Service struct {
	// SDKID is the smithy service id reported by middleware.GetServiceID, e.g.
	// "ECS". It is not the IAM prefix and not the endpoint prefix.
	SDKID string

	// IAMPrefix is the prefix used in policy actions, e.g. "ecs".
	IAMPrefix string

	// Ops are API operation names, exactly as the SDK reports them.
	Ops []string

	// IAMActions overrides the derived IAMPrefix:Op action for operations whose
	// IAM action name differs from the API operation name. Empty for most
	// services; see apigateway, whose IAM model is HTTP-verb based.
	//
	// A nil value (present key, no actions) means the operation requires no IAM
	// action at all.
	IAMActions map[string][]string
}

// services is the allow-list. Alphabetical by SDKID; keep it that way.
//
// Every operation here must be a genuine read. The Describe/List/Get naming
// convention is checked separately as defence in depth (see guard.go), but the
// convention is not the control — this list is.
var services = []Service{
	{
		SDKID:     "DynamoDB",
		IAMPrefix: "dynamodb",
		// DescribeContinuousBackups (PITR) is deliberately absent: point-in-time
		// recovery has no meaning for a local emulated table, so asking for the
		// permission would buy nothing.
		Ops: []string{
			"DescribeTable",
			"DescribeTimeToLive",
			"ListTables",
			"ListTagsOfResource",
		},
	},
	{
		SDKID:     "ECS",
		IAMPrefix: "ecs",
		// Tags come back from Describe* via the Include parameter, so no
		// ListTagsForResource. Running tasks are not read at all in v1: the
		// task definition attached to a service is what the local environment
		// reproduces, and ecs:ListTasks would be a permission we ask for and
		// never use.
		Ops: []string{
			"DescribeClusters",
			"DescribeServices",
			"DescribeTaskDefinition",
			"ListClusters",
			"ListServices",
		},
	},
	{
		SDKID:     "SQS",
		IAMPrefix: "sqs",
		// GetQueueAttributes with AttributeNames=["All"] returns the ARN, the
		// redrive policy, and the access policy in one call, so there is no
		// GetQueueUrl or separate ARN lookup here.
		//
		// ReceiveMessage is forbidden (see `forbidden` above): it is a read by
		// name and a mutation in effect, because it hides messages from the real
		// consumer for the visibility timeout.
		Ops: []string{
			"GetQueueAttributes",
			"ListQueueTags",
			"ListQueues",
		},
	},
	{
		SDKID:     "STS",
		IAMPrefix: "sts",
		Ops:       []string{"GetCallerIdentity"},
		// GetCallerIdentity is callable by any principal and needs no grant.
		IAMActions: map[string][]string{"GetCallerIdentity": nil},
	},
}

// Deliberately absent, and not to be added without an ADR:
//
//	secretsmanager:GetSecretValue  — see docs/07-security.md, Guarantee 2. Secret
//	                                 values never leave AWS. DescribeSecret gives
//	                                 the metadata linking needs.
//	sts:GetFederationToken         — mints credentials. Reads as a Get*, is not a
//	sts:GetSessionToken              read. The prefix heuristic would allow both,
//	                                 which is exactly why the allow-list is the
//	                                 real control.
//	ec2:GetPasswordData            — returns an encrypted admin password.
//	sqs:ReceiveMessage             — mutating in effect: it makes messages
//	                                 invisible to the real consumer.
var forbidden = map[string]string{
	"secretsmanager:GetSecretValue": "secret values never leave AWS (docs/07-security.md)",
	"sts:GetFederationToken":        "mints credentials",
	"sts:GetSessionToken":           "mints credentials",
	"ec2:GetPasswordData":           "returns an encrypted administrator password",
	"sqs:ReceiveMessage":            "hides messages from the real consumer",
}

// allowed is the flattened "SDKID:Operation" set, built once at init.
var allowed = func() map[string]struct{} {
	m := make(map[string]struct{})
	for _, s := range services {
		for _, op := range s.Ops {
			m[s.SDKID+":"+op] = struct{}{}
		}
	}
	return m
}()

// IsAllowed reports whether the given SDK service id and operation name are on
// the allow-list.
func IsAllowed(serviceID, operation string) bool {
	_, ok := allowed[serviceID+":"+operation]
	return ok
}

// Services returns the allow-list. The returned slice is a copy; the Service
// values inside share backing arrays and must not be mutated.
func Services() []Service {
	out := make([]Service, len(services))
	copy(out, services)
	return out
}

// IAMActionsFor returns the sorted, de-duplicated IAM actions implied by the
// allow-list for one service.
func (s Service) IAMActionsFor() []string {
	seen := make(map[string]struct{})
	for _, op := range s.Ops {
		if override, ok := s.IAMActions[op]; ok {
			for _, a := range override {
				seen[a] = struct{}{}
			}
			continue
		}
		seen[s.IAMPrefix+":"+op] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// AllIAMActions returns every action the scanner policy must grant, sorted.
func AllIAMActions() []string {
	var out []string
	for _, s := range services {
		out = append(out, s.IAMActionsFor()...)
	}
	sort.Strings(out)
	return out
}

// validateAllowList is called from init so a bad edit fails at process start
// rather than mid-scan against a production account.
func validateAllowList() error {
	seenSDK := make(map[string]struct{})
	seenIAM := make(map[string]struct{})
	for _, s := range services {
		if _, dup := seenSDK[s.SDKID]; dup {
			return fmt.Errorf("duplicate SDKID %q", s.SDKID)
		}
		seenSDK[s.SDKID] = struct{}{}

		if s.IAMPrefix != strings.ToLower(s.IAMPrefix) {
			return fmt.Errorf("%s: IAMPrefix %q must be lowercase", s.SDKID, s.IAMPrefix)
		}
		seenOps := make(map[string]struct{})
		for _, op := range s.Ops {
			if _, dup := seenOps[op]; dup {
				return fmt.Errorf("%s: duplicate operation %q", s.SDKID, op)
			}
			seenOps[op] = struct{}{}

			if !hasReadPrefix(op) {
				return fmt.Errorf("%s: operation %q does not look like a read; "+
					"if it genuinely is one, it needs an ADR, not an exception", s.SDKID, op)
			}
		}
		for op := range s.IAMActions {
			if _, ok := seenOps[op]; !ok {
				return fmt.Errorf("%s: IAMActions override for %q, which is not in Ops", s.SDKID, op)
			}
		}
		for _, a := range s.IAMActionsFor() {
			if _, dup := seenIAM[a]; dup {
				return fmt.Errorf("IAM action %q granted by two services", a)
			}
			seenIAM[a] = struct{}{}
			if reason, bad := forbidden[a]; bad {
				return fmt.Errorf("forbidden action %q on the allow-list: %s", a, reason)
			}
		}
	}
	return nil
}

func init() {
	if err := validateAllowList(); err != nil {
		panic("awsx: invalid allow-list: " + err.Error())
	}
}
