package awsx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/smithy-go/middleware"
)

// countingTransport records whether anything actually went out on the wire. A
// guard that returns an error *after* the request has been sent would be
// worthless, so every block test asserts this counter stayed at zero.
type countingTransport struct {
	n    int32
	body string
}

func (t *countingTransport) Do(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.n, 1)
	body := t.body
	if body == "" {
		body = "{}"
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (t *countingTransport) calls() int32 { return atomic.LoadInt32(&t.n) }

func testSession(t *testing.T, tr *countingTransport) *Session {
	t.Helper()
	return NewTestSession(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIATEST", "secret", ""),
		HTTPClient:  tr,
	}, "123456789012", "us-east-1")
}

// TestGuardBlocksMutationOnARealClient is the load-bearing test in this package.
//
// It does not test a helper function — it builds a real ECS client from a real
// Session and calls a real mutating operation, the way a bug would. The call must
// fail, and nothing may reach the network.
func TestGuardBlocksMutationOnARealClient(t *testing.T) {
	tr := &countingTransport{}
	client := ecs.NewFromConfig(testSession(t, tr).Config())

	_, err := client.CreateService(context.Background(), &ecs.CreateServiceInput{
		ServiceName: aws.String("should-never-happen"),
	})

	var blocked *ErrBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("CreateService: want ErrBlocked, got %v", err)
	}
	if blocked.Operation != "CreateService" || blocked.ServiceID != "ECS" {
		t.Errorf("blocked the wrong thing: %+v", blocked)
	}
	if n := tr.calls(); n != 0 {
		t.Fatalf("guard let %d request(s) reach the network — the block happened too late", n)
	}
}

// TestGuardBlocksDestructiveOperations covers the operations whose names a
// reviewer would most want to see explicitly named in a test.
func TestGuardBlocksDestructiveOperations(t *testing.T) {
	tr := &countingTransport{}
	client := ecs.NewFromConfig(testSession(t, tr).Config())
	ctx := context.Background()

	calls := map[string]func() error{
		"DeleteService": func() error {
			_, err := client.DeleteService(ctx, &ecs.DeleteServiceInput{Service: aws.String("x")})
			return err
		},
		"UpdateService": func() error {
			_, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{Service: aws.String("x")})
			return err
		},
		"StopTask": func() error {
			_, err := client.StopTask(ctx, &ecs.StopTaskInput{Task: aws.String("x")})
			return err
		},
		"RunTask": func() error {
			_, err := client.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: aws.String("x")})
			return err
		},
		"DeleteCluster": func() error {
			_, err := client.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: aws.String("x")})
			return err
		},
		"RegisterTaskDefinition": func() error {
			_, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{})
			return err
		},
		"DeregisterTaskDefinition": func() error {
			_, err := client.DeregisterTaskDefinition(ctx, &ecs.DeregisterTaskDefinitionInput{})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var blocked *ErrBlocked
			if err := call(); !errors.As(err, &blocked) {
				t.Fatalf("want ErrBlocked, got %v", err)
			}
		})
	}
	if n := tr.calls(); n != 0 {
		t.Fatalf("%d request(s) reached the network", n)
	}
}

// TestGuardRunsBeforeInputValidation pins the guard's position at the very front
// of the stack.
//
// RegisterTaskDefinition here is missing two required fields, so the SDK's own
// OperationInputValidation middleware would reject it. If the guard ran after
// validation, the user would be told "missing required field, Family" for a call
// cloud-echo was never going to make — technically safe, but it buries the real
// answer. The refusal must come first.
func TestGuardRunsBeforeInputValidation(t *testing.T) {
	tr := &countingTransport{}
	client := ecs.NewFromConfig(testSession(t, tr).Config())

	_, err := client.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{})

	var blocked *ErrBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("want ErrBlocked to win over input validation, got %v", err)
	}
	if strings.Contains(err.Error(), "missing required field") {
		t.Errorf("input validation ran first; the guard is registered too late: %v", err)
	}
}

// TestGuardAllowsListedOperations proves the guard is not simply blocking
// everything — a test suite where the guard is broken-closed would otherwise pass
// every other test in this file.
func TestGuardAllowsListedOperations(t *testing.T) {
	tr := &countingTransport{body: `{"clusterArns":[]}`}
	client := ecs.NewFromConfig(testSession(t, tr).Config())

	if _, err := client.ListClusters(context.Background(), &ecs.ListClustersInput{}); err != nil {
		t.Fatalf("ListClusters was blocked but is on the allow-list: %v", err)
	}
	if n := tr.calls(); n != 1 {
		t.Fatalf("want exactly 1 request, got %d", n)
	}
}

// TestGuardBlocksReadLookingNamesNotOnTheAllowList is the test that justifies
// having an allow-list at all.
//
// Every operation here starts with Get and would sail through a prefix-only
// check, which is what docs/07-security.md originally sketched. GetSecretValue in
// particular is forbidden by that same document's Guarantee 2 — so a prefix-only
// guard would have contradicted the security model it was written to enforce.
func TestGuardBlocksReadLookingNamesNotOnTheAllowList(t *testing.T) {
	for _, tc := range []struct{ service, op string }{
		{"Secrets Manager", "GetSecretValue"},
		{"STS", "GetFederationToken"},
		{"STS", "GetSessionToken"},
		{"EC2", "GetPasswordData"},
		{"ECS", "GetTotallyMadeUpThing"},
	} {
		t.Run(tc.service+":"+tc.op, func(t *testing.T) {
			err := runGuard(t, tc.service, tc.op)
			var blocked *ErrBlocked
			if !errors.As(err, &blocked) {
				t.Fatalf("want ErrBlocked, got %v", err)
			}
			if !strings.Contains(blocked.Reason, "allow-list") {
				t.Errorf("reason should point at the allow-list, got %q", blocked.Reason)
			}
		})
	}
}

// TestGuardFailsClosedWithoutMetadata pins the middleware ordering.
//
// The guard reads the operation name from the context, which the SDK's own
// RegisterServiceMetadata middleware puts there at Initialize/Before. If a future
// SDK version stops doing that, or the guard is moved to run first, the operation
// name goes empty. This asserts the resulting behaviour is "block everything",
// never "allow everything".
func TestGuardFailsClosedWithoutMetadata(t *testing.T) {
	err := runGuard(t, "", "")
	var blocked *ErrBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("want ErrBlocked when metadata is missing, got %v", err)
	}
	if !strings.Contains(blocked.Reason, "did not report an operation name") {
		t.Errorf("unexpected reason: %q", blocked.Reason)
	}
}

// TestGuardSeesOperationMetadata is the other half of the ordering pin: it proves
// that in the real stack the metadata *is* populated, so the fail-closed path
// above is a safety net rather than the everyday behaviour.
func TestGuardSeesOperationMetadata(t *testing.T) {
	var sawService, sawOp string

	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIATEST", "secret", ""),
		HTTPClient:  &countingTransport{body: `{"clusterArns":[]}`},
	}
	cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("captureMetadata", func(
			ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
		) (middleware.InitializeOutput, middleware.Metadata, error) {
			sawService = awsmiddleware.GetServiceID(ctx)
			sawOp = awsmiddleware.GetOperationName(ctx)
			return next.HandleInitialize(ctx, in)
		}), middleware.After)
	})

	client := ecs.NewFromConfig(NewTestSession(cfg, "123456789012", "us-east-1").Config())
	if _, err := client.ListClusters(context.Background(), &ecs.ListClustersInput{}); err != nil {
		t.Fatalf("ListClusters: %v", err)
	}

	if sawService != "ECS" || sawOp != "ListClusters" {
		t.Fatalf("at Initialize/After the SDK reported service=%q op=%q; "+
			"the guard depends on both being populated there", sawService, sawOp)
	}
}

// TestGuardIsRegisteredOnEverySessionClient guards against the guard being
// dropped by a refactor of NewTestSession or NewSession.
func TestGuardIsRegisteredOnEverySessionClient(t *testing.T) {
	cfg := testSession(t, &countingTransport{}).Config()
	if len(cfg.APIOptions) == 0 {
		t.Fatal("session config carries no APIOptions — the guard is missing")
	}

	stack := middleware.NewStack("test", func() interface{} { return nil })
	for _, opt := range cfg.APIOptions {
		if err := opt(stack); err != nil {
			t.Fatalf("applying API option: %v", err)
		}
	}
	if _, ok := stack.Initialize.Get(GuardID); !ok {
		t.Fatalf("%s is not registered in the Initialize step", GuardID)
	}
}

// runGuard drives the middleware directly with a synthetic context, for cases
// where no real SDK client exposes the operation we want to test.
func runGuard(t *testing.T, service, op string) error {
	t.Helper()

	stack := middleware.NewStack("test", func() interface{} { return nil })
	if err := readOnlyGuard(stack); err != nil {
		t.Fatalf("registering guard: %v", err)
	}

	ctx := context.Background()
	if service != "" {
		ctx = awsmiddleware.SetServiceID(ctx, service)
	}
	if op != "" {
		ctx = awsmiddleware.SetOperationName(ctx, op)
	}

	reached := false
	h := middleware.DecorateHandler(
		middleware.HandlerFunc(func(context.Context, interface{}) (interface{}, middleware.Metadata, error) {
			reached = true
			return nil, middleware.Metadata{}, nil
		}),
		stack,
	)

	_, _, err := h.Handle(ctx, struct{}{})
	if err == nil && !reached {
		t.Fatal("handler was neither reached nor blocked")
	}
	return err
}
