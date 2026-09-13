package discovery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// Lambda collects functions and their event source mappings.
//
// Event source mappings are emitted as resources of their own rather than folded
// into the function. They are independent AWS resources with their own identity,
// listed account-wide; one function can have several; and a mapping frequently
// targets an alias (…:function:order-processor:prod) rather than the unqualified
// function ListFunctions returns. Keeping them separate also keeps their
// provenance honest: the linker's evidence for a queue → function edge must cite
// lambda:ListEventSourceMappings, not the call that listed the function.
//
// Known gap: ListFunctions returns $LATEST. If production traffic goes through
// an alias pinned to an older published version, that version's environment can
// differ from what is recorded here. The mapping's qualifier is recorded so the
// planner can at least say so.
type Lambda struct{}

func (*Lambda) Service() string { return "Lambda" }

func (c *Lambda) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := lambda.NewFromConfig(s.Config())

	p := lambda.NewListFunctionsPaginator(api, &lambda.ListFunctionsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "Lambda", "ListFunctions", err); err != nil {
				return err
			}
			break
		}
		for i := range page.Functions {
			if err := c.emitFunction(ctx, api, s, out, &page.Functions[i]); err != nil {
				return err
			}
		}
	}

	return c.collectMappings(ctx, api, s, out)
}

func (c *Lambda) emitFunction(
	ctx context.Context,
	api *lambda.Client,
	s *awsx.Session,
	out Emitter,
	fn *lambdatypes.FunctionConfiguration,
) error {
	name := aws.ToString(fn.FunctionName)

	spec := functionSpec{
		FunctionName: name,
		Runtime:      string(fn.Runtime),
		Handler:      aws.ToString(fn.Handler),
		PackageType:  string(fn.PackageType),
		MemoryMB:     aws.ToInt32(fn.MemorySize),
		TimeoutSec:   aws.ToInt32(fn.Timeout),
		RoleARN:      aws.ToString(fn.Role),
		RoleID:       roleIDFromARN(aws.ToString(fn.Role)),
	}
	for _, a := range fn.Architectures {
		spec.Architectures = append(spec.Architectures, string(a))
	}
	for _, l := range fn.Layers {
		spec.Layers = append(spec.Layers, aws.ToString(l.Arn))
	}
	if v := fn.VpcConfig; v != nil && aws.ToString(v.VpcId) != "" {
		spec.VPC = &vpcSpec{VpcID: aws.ToString(v.VpcId), Subnets: v.SubnetIds, SecurityGroups: v.SecurityGroupIds}
	}
	if d := fn.DeadLetterConfig; d != nil && aws.ToString(d.TargetArn) != "" {
		spec.DeadLetter = &targetRef{ARN: aws.ToString(d.TargetArn), ID: resourceIDFromARN(aws.ToString(d.TargetArn))}
	}
	if l := fn.LoggingConfig; l != nil {
		spec.LogGroup = aws.ToString(l.LogGroup)
	}

	if env := fn.Environment; env != nil {
		// With a customer-managed KMS key and no kms:Decrypt — which the
		// scanner policy deliberately does not grant — AWS returns an error in
		// place of the variables. That is the policy working, but it also means
		// this function's Tier-2 signal is invisible, and the user should know.
		if e := env.Error; e != nil {
			spec.EnvUnreadable = aws.ToString(e.ErrorCode)
			out.Warn(inventory.Warning{
				Service: "Lambda",
				Op:      "ListFunctions",
				Kind:    "unreadable",
				Message: name + ": environment not readable (" + aws.ToString(e.ErrorCode) + "): " + aws.ToString(e.Message),
			})
		}
		// Redact in place so the Raw copy below carries the same redactions.
		for k, v := range env.Variables {
			if r, red := redactValue(k, v); red {
				env.Variables[k] = r
				spec.Redacted = append(spec.Redacted, "env:"+k)
			}
		}
		sort.Strings(spec.Redacted)
		if len(env.Variables) > 0 {
			spec.Env = env.Variables
		}
	}

	policy, err := api.GetPolicy(ctx, &lambda.GetPolicyInput{FunctionName: aws.String(name)})
	switch {
	case err == nil:
		spec.ResourcePolicy = rawIfPresent(aws.ToString(policy.Policy))
	case isNotFound(err):
		// Most functions have no resource policy. Not a warning.
	default:
		if err := warnOrFail(out, "Lambda", "GetPolicy", err); err != nil {
			return err
		}
	}

	out.Emit(newResource(s, resourceArgs{
		// Function names are unique per account and region.
		ID:   "lambda/" + name,
		Type: "lambda.function",
		ARN:  aws.ToString(fn.FunctionArn),
		Name: name,
		API:  "lambda:ListFunctions",
		Spec: spec,
		Raw:  fn,
	}))
	return nil
}

func (c *Lambda) collectMappings(ctx context.Context, api *lambda.Client, s *awsx.Session, out Emitter) error {
	p := lambda.NewListEventSourceMappingsPaginator(api, &lambda.ListEventSourceMappingsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return warnOrFail(out, "Lambda", "ListEventSourceMappings", err)
		}
		for _, m := range page.EventSourceMappings {
			uuid := aws.ToString(m.UUID)
			fnARN := aws.ToString(m.FunctionArn)
			fnName, qualifier := functionNameFromARN(fnARN)
			srcARN := aws.ToString(m.EventSourceArn)

			spec := mappingSpec{
				UUID:        uuid,
				State:       aws.ToString(m.State),
				BatchSize:   aws.ToInt32(m.BatchSize),
				BatchWindow: aws.ToInt32(m.MaximumBatchingWindowInSeconds),
				Source:      targetRef{ARN: srcARN, ID: resourceIDFromARN(srcARN)},
				SourceType:  sourceType(srcARN),
				FunctionARN: fnARN,
				Qualifier:   qualifier,
				StartingPos: string(m.StartingPosition),
			}
			spec.Enabled, spec.Transitional = mappingEnabled(spec.State)
			if fnName != "" {
				spec.FunctionID = "lambda/" + fnName
			}
			if f := m.FilterCriteria; f != nil {
				for _, flt := range f.Filters {
					spec.Filters = append(spec.Filters, aws.ToString(flt.Pattern))
				}
			}
			if d := m.DestinationConfig; d != nil && d.OnFailure != nil && aws.ToString(d.OnFailure.Destination) != "" {
				dst := aws.ToString(d.OnFailure.Destination)
				spec.OnFailure = &targetRef{ARN: dst, ID: resourceIDFromARN(dst)}
			}

			out.Emit(newResource(s, resourceArgs{
				ID:   "lambda/esm/" + uuid,
				Type: "lambda.event-source-mapping",
				ARN:  aws.ToString(m.EventSourceMappingArn),
				Name: uuid,
				API:  "lambda:ListEventSourceMappings",
				Spec: spec,
				Raw:  m,
			}))
		}
	}
	return nil
}

type functionSpec struct {
	FunctionName  string   `json:"functionName"`
	Runtime       string   `json:"runtime,omitempty"`
	Handler       string   `json:"handler,omitempty"`
	PackageType   string   `json:"packageType,omitempty"` // Zip | Image
	Architectures []string `json:"architectures,omitempty"`
	MemoryMB      int32    `json:"memoryMb"`
	TimeoutSec    int32    `json:"timeoutSec"`

	// RoleARN is the execution role, and for Lambda it *is* the application's
	// identity — the Tier-3 input for this function.
	RoleARN string `json:"roleArn"`
	RoleID  string `json:"roleId,omitempty"`

	Env           map[string]string `json:"env,omitempty"`
	Redacted      []string          `json:"redacted,omitempty"`
	EnvUnreadable string            `json:"envUnreadable,omitempty"`

	Layers     []string   `json:"layers,omitempty"`
	VPC        *vpcSpec   `json:"vpc,omitempty"`
	DeadLetter *targetRef `json:"deadLetter,omitempty"`
	LogGroup   string     `json:"logGroup,omitempty"`

	// ResourcePolicy names who may invoke the function — API Gateway, SNS, S3,
	// EventBridge. It is how a Lambda's non-queue triggers are found, and so how
	// entrypoints are classified. Kept raw; interpreting it is the linker's job.
	ResourcePolicy json.RawMessage `json:"resourcePolicy,omitempty"`
}

type mappingSpec struct {
	UUID  string `json:"uuid"`
	State string `json:"state"`

	// Enabled is whether the mapping delivers events, and is null when the
	// state cannot say: a mapping caught mid-update or mid-creation reports
	// "Updating" or "Creating" whether it is enabled or not. Scanned against a
	// real account, a live mapping whose batch size was being changed read as
	// disabled under a two-state rule — and the linker would have dropped a real
	// edge. Null tells it to keep the edge and say it is unsettled.
	Enabled      *bool `json:"enabled"`
	Transitional bool  `json:"transitional,omitempty"`

	BatchSize   int32 `json:"batchSize,omitempty"`
	BatchWindow int32 `json:"maxBatchingWindowSec,omitempty"`

	Source     targetRef `json:"source"`
	SourceType string    `json:"sourceType"` // sqs | dynamodb-stream | kinesis | kafka | mq | unknown

	FunctionARN string `json:"functionArn"`
	FunctionID  string `json:"functionId,omitempty"`
	// Qualifier is the alias or version the mapping invokes, when it is not
	// $LATEST. See the known gap on the Lambda type.
	Qualifier string `json:"qualifier,omitempty"`

	StartingPos string     `json:"startingPosition,omitempty"`
	Filters     []string   `json:"filters,omitempty"`
	OnFailure   *targetRef `json:"onFailure,omitempty"`
}

// mappingEnabled maps an event source mapping state to whether it delivers
// events, and whether that state is still settling. Creating and Updating say
// nothing about the outcome, so they return nil rather than a guess.
func mappingEnabled(state string) (*bool, bool) {
	yes, no := true, false
	switch state {
	case "Enabled":
		return &yes, false
	case "Disabled":
		return &no, false
	case "Enabling":
		return &yes, true
	case "Disabling", "Deleting":
		return &no, true
	case "Creating", "Updating":
		return nil, true
	}
	return nil, false
}

// targetRef is an ARN plus the inventory id it maps to, when cloud-echo has a
// collector for that service. The id is derived from the ARN alone, so it is set
// even if the target was not in this scan — a dangling id is itself information
// (the target is out of scope, or was deleted).
type targetRef struct {
	ARN string `json:"arn"`
	ID  string `json:"id,omitempty"`
}

type vpcSpec struct {
	VpcID          string   `json:"vpcId"`
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
}

// resourceIDFromARN maps an ARN to an inventory id for the services cloud-echo
// collects, and returns "" for everything else. It never invents an id for a
// service without a collector: a made-up id would look like a real node to the
// linker and dangle silently.
func resourceIDFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	service, res := parts[2], parts[5]
	switch service {
	case "sqs":
		return "sqs/" + res
	case "dynamodb":
		// table/orders or table/orders/stream/2026-...: the stream belongs to
		// the table and is not modelled as a separate node.
		if rest, ok := strings.CutPrefix(res, "table/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			return "ddb/" + name
		}
	case "lambda":
		if name, _ := functionNameFromARN(arn); name != "" {
			return "lambda/" + name
		}
	}
	return ""
}

func sourceType(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return "unknown"
	}
	switch parts[2] {
	case "sqs":
		return "sqs"
	case "dynamodb":
		return "dynamodb-stream"
	case "kinesis":
		return "kinesis"
	case "kafka":
		return "kafka"
	case "mq":
		return "mq"
	}
	return "unknown"
}

// functionNameFromARN splits arn:aws:lambda:<region>:<acct>:function:<name>[:<qualifier>].
func functionNameFromARN(arn string) (name, qualifier string) {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[2] != "lambda" || parts[5] != "function" {
		return "", ""
	}
	name = parts[6]
	if len(parts) >= 8 && parts[7] != "$LATEST" {
		qualifier = parts[7]
	}
	return name, qualifier
}

// roleIDFromARN maps arn:aws:iam::<acct>:role[/path]/<name> to "iam/role/<name>".
// IAM role names are unique per account regardless of path, so the path is not
// part of the id.
func roleIDFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[2] != "iam" {
		return ""
	}
	res, ok := strings.CutPrefix(parts[5], "role/")
	if !ok || res == "" {
		return ""
	}
	if i := strings.LastIndex(res, "/"); i >= 0 {
		res = res[i+1:]
	}
	return "iam/role/" + res
}
