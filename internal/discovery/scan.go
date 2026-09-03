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
}

// NewRegistry returns the collectors enabled for v1.
func NewRegistry() *Registry {
	return &Registry{collectors: []Collector{
		&DynamoDB{},
		&ECS{},
		&SQS{},
	}}
}

// Collectors returns the registered collectors.
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

	var wg sync.WaitGroup
	for _, c := range r.collectors {
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			if err := c.Collect(ctx, s, sink); err != nil {
				// A collector that fails outright still leaves the rest of the
				// scan usable. Losing ECS should not cost you the DynamoDB
				// tables you already paid for.
				sink.Warn(inventory.Warning{
					Service: c.Service(),
					Kind:    "api-error",
					Message: err.Error(),
				})
			}
		}(c)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	inv.Normalize()
	if dup := inv.Duplicates(); len(dup) > 0 {
		return nil, fmt.Errorf("collector bug: duplicate resource ids %v", dup)
	}
	return inv, nil
}

// syncEmitter serializes concurrent collector output.
type syncEmitter struct {
	mu  sync.Mutex
	inv *inventory.Inventory
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
