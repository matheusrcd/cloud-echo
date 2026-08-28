package awsx

import (
	"context"
	"fmt"
	"strings"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// GuardID is the middleware's id in the stack. Tests assert it is present.
const GuardID = "cloudEchoReadOnlyGuard"

// ErrBlocked is returned when the guard refuses an operation. It is deliberately
// not wrapped in a retryable error type: a blocked call is a programming error,
// and retrying it is never right.
type ErrBlocked struct {
	ServiceID string
	Operation string
	Reason    string
}

func (e *ErrBlocked) Error() string {
	return fmt.Sprintf("cloud-echo refuses to call %s:%s — %s. "+
		"This is enforced in code and cannot be disabled; see docs/adr/0006-read-only-by-construction.md",
		e.ServiceID, e.Operation, e.Reason)
}

// readPrefixes is the naming-convention check. It is defence in depth, not the
// control — the control is the allow-list in operations.go. Its job is to catch
// an operation added to the allow-list by someone who did not read the comment.
var readPrefixes = []string{"Describe", "List", "Get", "BatchGet"}

func hasReadPrefix(op string) bool {
	for _, p := range readPrefixes {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

// readOnlyGuard rejects any operation that is not on the allow-list.
//
// It registers at Initialize/Before, which makes it the first middleware in the
// stack — ahead of the SDK's own input validation. That ordering is not just
// about being early enough to be safe (anything in Initialize is), it is about
// which error the user sees: run it after validation and a malformed mutating
// call reports "missing required field" instead of "cloud-echo refuses to call
// this". Same protection, far worse explanation.
//
// Running first is safe because the service id and operation name are not set by
// a middleware — the generated client puts them on the context in invokeOperation
// before the stack is built, so they are readable from every step.
//
// If a future SDK version stops doing that, the operation name goes empty, which
// is not on the allow-list, so the guard blocks everything and the breakage is
// loud. TestGuardFailsClosedWithoutMetadata and TestGuardRunsBeforeInputValidation
// pin both halves of this.
func readOnlyGuard(stack *middleware.Stack) error {
	guard := middleware.InitializeMiddlewareFunc(GuardID, func(
		ctx context.Context,
		in middleware.InitializeInput,
		next middleware.InitializeHandler,
	) (middleware.InitializeOutput, middleware.Metadata, error) {
		service := awsmiddleware.GetServiceID(ctx)
		op := awsmiddleware.GetOperationName(ctx)

		switch {
		case op == "":
			return middleware.InitializeOutput{}, middleware.Metadata{}, &ErrBlocked{
				ServiceID: service,
				Operation: "<unknown>",
				Reason: "the SDK did not report an operation name, so the guard cannot " +
					"verify this call is read-only",
			}
		case !hasReadPrefix(op):
			return middleware.InitializeOutput{}, middleware.Metadata{}, &ErrBlocked{
				ServiceID: service,
				Operation: op,
				Reason:    "it is not a Describe/List/Get/BatchGet operation",
			}
		case !IsAllowed(service, op):
			return middleware.InitializeOutput{}, middleware.Metadata{}, &ErrBlocked{
				ServiceID: service,
				Operation: op,
				Reason: "it is not on the discovery allow-list in internal/awsx/operations.go " +
					"(a read-looking name is not sufficient)",
			}
		}

		return next.HandleInitialize(ctx, in)
	})

	return stack.Initialize.Add(guard, middleware.Before)
}
