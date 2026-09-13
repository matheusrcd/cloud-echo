package spec

import (
	"fmt"
	"strings"
)

// The inventory id scheme is part of the contract. Discovery uses these to name
// the targets it records; the linker uses them to resolve targets it derives
// itself (a templated integration resolved per stage). Defining them once keeps
// both sides naming a resource the same way.

// ARN is a parsed ARN.
type ARN struct {
	Partition, Service, Region, Account, Resource string
}

// ParseARN splits an ARN into its fields; ok is false for anything that is not
// one.
func ParseARN(s string) (ARN, bool) {
	p := strings.SplitN(s, ":", 6)
	if len(p) < 6 || p[0] != "arn" {
		return ARN{}, false
	}
	return ARN{Partition: p[1], Service: p[2], Region: p[3], Account: p[4], Resource: p[5]}, true
}

// LambdaFromInvokeURI extracts the function from an API Gateway Lambda invoke
// URI (arn:aws:apigateway:<r>:lambda:path/2015-03-31/functions/<fn-arn>/invocations),
// used by v1 integrations and by authorizers of both versions.
func LambdaFromInvokeURI(uri string) (*TargetRef, string) {
	if uri == "" || strings.Contains(uri, "${") {
		return nil, ""
	}
	_, after, ok := strings.Cut(uri, "/functions/")
	if !ok {
		return nil, ""
	}
	fnARN, _, _ := strings.Cut(after, "/invocations")
	name, q := FunctionFromARN(fnARN)
	if name == "" {
		return nil, ""
	}
	return &TargetRef{ARN: fnARN, ID: "lambda/" + name}, q
}

// QueueFromURL maps https://sqs.<region>.amazonaws.com/<acct>/<name> to a
// target. The URL is only trusted in that exact shape.
func QueueFromURL(u string) *TargetRef {
	rest, ok := strings.CutPrefix(u, "https://sqs.")
	if !ok {
		return nil
	}
	host, path, ok := strings.Cut(rest, "/")
	region, _, ok2 := strings.Cut(host, ".amazonaws.com")
	acct, name, ok3 := strings.Cut(path, "/")
	if !ok || !ok2 || !ok3 || name == "" || strings.Contains(name, "/") {
		return nil
	}
	region = strings.TrimSuffix(region, ".")
	arn := fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, acct, name)
	return &TargetRef{ARN: arn, ID: IDFromARN(arn)}
}

// IDFromARN maps an ARN to an inventory id for the services cloud-echo
// collects, and returns "" for everything else. It never invents an id for a
// service without a collector: a made-up id would look like a real node to the
// linker and dangle silently.
func IDFromARN(arn string) string {
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
		if name, _ := FunctionFromARN(arn); name != "" {
			return "lambda/" + name
		}
	}
	return ""
}

// FunctionFromARN splits arn:aws:lambda:<region>:<acct>:function:<name>[:<qualifier>].
func FunctionFromARN(arn string) (name, qualifier string) {
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
