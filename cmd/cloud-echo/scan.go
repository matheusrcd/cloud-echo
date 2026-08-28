package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/discovery"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/version"
)

func runScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	var (
		profile     = fs.String("profile", "", "AWS profile to use")
		region      = fs.String("region", "", "region to scan (default: the profile's)")
		out         = fs.String("out", filepath.Join(".cloud-echo", "inventory.json"), "where to write the inventory")
		concurrency = fs.Int("concurrency", 8, "how many collectors run at once")
		dryRun      = fs.Bool("dry-run", false, "print every API call this would make, then exit without touching AWS")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cloud-echo scan [flags]\n\n"+
			"Reads an AWS account and writes a normalized inventory. This is the only\n"+
			"command that talks to AWS, and it is structurally incapable of writing.\n\n"+
			"Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dryRun {
		printDryRun()
		return nil
	}

	session, err := awsx.NewSession(ctx, awsx.Options{Profile: *profile, Region: *region})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "scanning account %s in %s\n", session.AccountID(), session.Region())

	registry := discovery.NewRegistry()
	inv, err := registry.Scan(ctx, session, discovery.Options{
		Concurrency: *concurrency,
		GeneratedBy: version.UserAgent(),
	})
	if err != nil {
		return err
	}

	if err := writeInventory(*out, inv); err != nil {
		return err
	}

	printSummary(inv, *out)
	return nil
}

// writeInventory writes atomically: a scan interrupted mid-write must not leave
// a truncated inventory that the next command reads as authoritative.
func writeInventory(path string, inv *inventory.Inventory) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".inventory-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := inv.Write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func printSummary(inv *inventory.Inventory, path string) {
	byType := map[string]int{}
	for _, r := range inv.Resources {
		byType[r.Type]++
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	fmt.Fprintf(os.Stderr, "\nfound %d resources:\n", len(inv.Resources))
	for _, t := range types {
		fmt.Fprintf(os.Stderr, "  %-24s %d\n", t, byType[t])
	}

	if len(inv.Warnings) > 0 {
		// Warnings are printed in full rather than counted. A partial inventory
		// produces a partial graph, and a user who does not know which parts are
		// missing will read the gaps as facts about their architecture.
		fmt.Fprintf(os.Stderr, "\n%d warning(s) — this inventory is incomplete:\n", len(inv.Warnings))
		for _, w := range inv.Warnings {
			fmt.Fprintf(os.Stderr, "  [%s] %s", w.Kind, w.Service)
			if w.Op != "" {
				fmt.Fprintf(os.Stderr, ":%s", w.Op)
			}
			fmt.Fprintf(os.Stderr, " — %s\n", w.Message)
		}
		fmt.Fprintf(os.Stderr, "\nsee %s for the permissions cloud-echo needs\n", awsx.PolicyPath)
	}

	fmt.Fprintf(os.Stderr, "\nwrote %s\n", path)
}

// printDryRun lists the exact API surface a scan would touch, so a security team
// can review it before granting access. It is derived from the same allow-list
// that the middleware enforces and the shipped policy is generated from, so it
// cannot understate what the tool does.
func printDryRun() {
	fmt.Println("cloud-echo scan would call exactly these AWS APIs, and no others:")
	fmt.Println()

	for _, s := range awsx.Services() {
		fmt.Printf("  %s\n", s.SDKID)
		ops := append([]string(nil), s.Ops...)
		sort.Strings(ops)
		for _, op := range ops {
			fmt.Printf("    %s:%s\n", s.IAMPrefix, op)
		}
		fmt.Println()
	}

	fmt.Println("Enforcement:")
	fmt.Println("  - An SDK middleware rejects every operation not on this list, before")
	fmt.Println("    the request is serialized. There is no flag to disable it.")
	fmt.Printf("  - %s grants exactly these actions and is\n", awsx.PolicyPath)
	fmt.Println("    generated from the same list, with a test that fails on drift.")
	fmt.Println("  - secretsmanager:GetSecretValue is deliberately absent: secret values")
	fmt.Println("    never leave AWS.")
	fmt.Println()
	fmt.Printf("IAM actions: %s\n", strings.Join(awsx.AllIAMActions(), ", "))
}
