package discovery

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// Registry is the set of collectors a scan will run.
type Registry struct {
	collectors []Collector
	dependents []DependentCollector
}

// NewRegistry returns the collectors enabled for v1.
func NewRegistry() *Registry {
	return &Registry{
		collectors: []Collector{
			&APIGateway{},
			&APIGatewayV2{},
			&DynamoDB{},
			&ECS{},
			&Lambda{},
			&RDS{},
			&SQS{},
		},
		dependents: []DependentCollector{
			&IAM{},
		},
	}
}

// Collectors returns the registered first-phase collectors.
func (r *Registry) Collectors() []Collector { return r.collectors }

// Options tunes a scan.
type Options struct {
	// Concurrency bounds how many collectors run at once. The default is
	// deliberately conservative: a scanner that trips API throttling on a
	// production account gets the whole tool banned from the organisation.
	Concurrency int

	// GeneratedBy is stamped into the inventory for provenance.
	GeneratedBy string
}

// Scan runs every registered collector and assembles the inventory.
//
// Collectors run concurrently and emit into a serialized collector-safe sink, so
// the resulting slice is in arrival order — Normalize fixes that before anything
// is written or compared.
func (r *Registry) Scan(ctx context.Context, s *awsx.Session, opts Options) (*inventory.Inventory, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}

	inv := &inventory.Inventory{
		ScanID:      newScanID(),
		GeneratedBy: opts.GeneratedBy,
		AccountID:   s.AccountID(),
		Region:      s.Region(),
		ScannedAt:   time.Now().UTC(),
	}

	sink := &syncEmitter{inv: inv}
	sem := make(chan struct{}, opts.Concurrency)

	// Phase 1: independent collectors, concurrently.
	runPhase(ctx, sem, len(r.collectors), func(i int) (string, error) {
		c := r.collectors[i]
		return c.Service(), c.Collect(ctx, s, sink)
	}, sink)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Phase 2: collectors that read what phase 1 found. They get a snapshot,
	// not the live slice, so nothing they emit can change what another
	// dependent sees — the order dependents run in must not matter.
	if len(r.dependents) > 0 {
		prior := sink.snapshot()
		runPhase(ctx, sem, len(r.dependents), func(i int) (string, error) {
			d := r.dependents[i]
			return d.Service(), d.CollectFrom(ctx, s, prior, sink)
		}, sink)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	inv.Normalize()
	if dup := inv.Duplicates(); len(dup) > 0 {
		return nil, fmt.Errorf("collector bug: duplicate resource ids %v", dup)
	}
	return inv, nil
}

// runPhase runs n collectors under the shared concurrency bound and waits for all
// of them. A collector that fails outright becomes a warning: losing ECS should
// not cost the user the DynamoDB tables that were already read.
func runPhase(ctx context.Context, sem chan struct{}, n int, run func(i int) (string, error), sink *syncEmitter) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if service, err := run(i); err != nil {
				sink.Warn(inventory.Warning{Service: service, Kind: "api-error", Message: err.Error()})
			}
		}(i)
	}
	wg.Wait()
}

// syncEmitter serializes concurrent collector output.
type syncEmitter struct {
	mu  sync.Mutex
	inv *inventory.Inventory
}

// snapshot copies the resources emitted so far.
func (e *syncEmitter) snapshot() []inventory.Resource {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]inventory.Resource(nil), e.inv.Resources...)
}

func (e *syncEmitter) Emit(r inventory.Resource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inv.Add(r)
}

func (e *syncEmitter) Warn(w inventory.Warning) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inv.Warn(w)
}

func newScanID() string {
	return time.Now().UTC().Format("20060102T150405Z")
}
