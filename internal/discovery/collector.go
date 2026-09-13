// Package discovery scans an AWS account and produces a normalized inventory.
//
// It is the only package in cloud-echo that talks to a real AWS account, and it
// can only read — see internal/awsx and docs/adr/0006-read-only-by-construction.md.
package discovery

import (
	"context"
	"errors"

	"github.com/aws/smithy-go"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// Collector reads one AWS service.
//
// Every implementation obeys four rules, and the rules are the interesting part:
//
//  1. Read-only. Enforced by the SDK guard, not by convention — a collector
//     cannot mutate even if its author tries.
//  2. Fully paginated. Never trust a first page. An account large enough to
//     paginate is exactly the account where a partial graph is most misleading.
//  3. Degrades, never fails. An AccessDenied on one call produces a warning and
//     a partial inventory. Real accounts have partial permissions.
//  4. Records provenance on every resource, because the linker's evidence chains
//     bottom out in it.
type Collector interface {
	// Service returns the SDK service id, e.g. "ECS". It must match the SDKID
	// in the awsx allow-list.
	Service() string

	// Collect emits resources on out. It must be side-effect free.
	//
	// Returning an error aborts only this collector; the scan continues with the
	// rest. Recoverable problems should be reported through Emitter.Warn instead,
	// so that partial results survive.
	Collect(ctx context.Context, s *awsx.Session, out Emitter) error
}

// Emitter is how a collector reports what it found and what it could not see.
type Emitter interface {
	Emit(inventory.Resource)
	Warn(inventory.Warning)
}

// isAccessDenied reports whether an error is AWS refusing us, as opposed to a
// real failure.
//
// This distinction is the whole of rule 3: a denial means "this account did not
// grant that permission", which is a normal state of affairs and should degrade
// the scan. Anything else — a throttle we exhausted retries on, a malformed
// response, a network partition — is a genuine failure the user needs to know
// about as more than a footnote.
func isAccessDenied(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation",
		"AuthorizationError", "MissingAuthenticationToken":
		return true
	}
	return false
}

// isNotFound reports whether AWS said the thing does not exist.
//
// That is a different fact from "you may not see it", and often not a failure at
// all: lambda:GetPolicy answers ResourceNotFoundException for every function that
// simply has no resource policy. Where it *is* a finding — a task definition
// naming an IAM role that was deleted — the caller decides how to report it.
func isNotFound(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "ResourceNotFoundException", "NoSuchEntity", "NotFoundException":
		return true
	}
	return false
}

// warnOrFail converts an error into either a warning (returning nil, so the
// collector continues) or a hard error the caller should propagate.
func warnOrFail(out Emitter, service, op string, err error) error {
	if err == nil {
		return nil
	}
	// A cancelled context is the user pressing Ctrl-C or a deadline expiring.
	// Recording that as an access warning would be actively misleading.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isAccessDenied(err) {
		out.Warn(inventory.Warning{
			Service: service,
			Op:      op,
			Kind:    "access-denied",
			Message: err.Error(),
		})
		return nil
	}
	return err
}
