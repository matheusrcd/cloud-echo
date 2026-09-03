package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
)

// Session is the read-only handle on an AWS account. It is the only way this
// package hands out an aws.Config, and every config it hands out carries the
// guard — there is no constructor, option, or environment variable that produces
// an unguarded one.
//
// The materializer talks to Floci through a different type in a different
// package, so the read path and the write path cannot be mixed up by a refactor.
// See docs/adr/0006-read-only-by-construction.md.
type Session struct {
	cfg       aws.Config
	accountID string
	alias     string
	region    string
}

// Options configures how the session authenticates. Zero value is valid and
// means "use the default SDK credential chain in the profile's region".
type Options struct {
	Profile string
	Region  string

	// AssumeRoleARN, when set, is assumed after the base chain resolves. Used
	// for multi-account orgs (docs/02-discovery.md).
	AssumeRoleARN string

	// MaxRetries bounds SDK retries. Discovery is read-only and idempotent, so
	// retrying is always safe; the limit exists to keep a throttled scan from
	// running for an unbounded time.
	MaxRetries int
}

// NewSession resolves credentials and verifies them with sts:GetCallerIdentity.
//
// Resolving the account id up front is not just a health check: the account id
// is what makes local ARNs byte-identical to production ones, because Floci
// adopts a 12-digit access key as its account id (docs/01-architecture.md).
func NewSession(ctx context.Context, opts Options) (*Session, error) {
	loadOpts := []func(*config.LoadOptions) error{
		config.WithAPIOptions([]func(*middleware.Stack) error{readOnlyGuard}),
		config.WithRetryer(func() aws.Retryer {
			maxAttempts := opts.MaxRetries
			if maxAttempts <= 0 {
				maxAttempts = 5
			}
			// Adaptive mode backs off on throttling rather than hammering. A
			// scanner that trips API limits on a production account gets the
			// tool banned from the org.
			return retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
				o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
					so.MaxAttempts = maxAttempts
				})
			})
		}),
	}
	if opts.Profile != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(opts.Profile))
	}
	if opts.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opts.Region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no region configured: pass --region or set one in your profile")
	}

	if opts.AssumeRoleARN != "" {
		return nil, fmt.Errorf("--assume-role is not implemented yet")
	}

	s := &Session{cfg: cfg, region: cfg.Region}

	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("verifying credentials with sts:GetCallerIdentity: %w", err)
	}
	s.accountID = aws.ToString(ident.Account)

	return s, nil
}

// Config returns a guarded config. Every client built from it is read-only.
func (s *Session) Config() aws.Config { return s.cfg }

// AccountID is the real account id, used verbatim as the local Floci account id.
func (s *Session) AccountID() string { return s.accountID }

// Region is the single region this scan covers. Multi-region is a v2 concern.
func (s *Session) Region() string { return s.region }

// Alias is the account alias if one was resolved, otherwise empty.
func (s *Session) Alias() string { return s.alias }

// NewTestSession wraps a caller-supplied config so collector contract tests can
// run against recorded fixtures instead of a live account.
//
// It attaches the guard to the supplied config rather than trusting the caller to
// have done so. That matters: this is an exported constructor, and if it accepted
// an unguarded config it would be a documented way around the one guarantee this
// package exists to provide. Every Session, however constructed, is guarded.
func NewTestSession(cfg aws.Config, accountID, region string) *Session {
	cfg = cfg.Copy()
	cfg.APIOptions = append(cfg.APIOptions, readOnlyGuard)
	return &Session{cfg: cfg, accountID: accountID, region: region}
}
