package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	if ep := session.EndpointOverride(); ep != "" {
		fmt.Fprintf(os.Stderr, "endpoint override: %s — requests are NOT going to AWS's public endpoints\n", ep)
	}

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
	writeSummary(os.Stderr, inv, path)
}

// writeSummary prints the scan result for a human.
//
// Access denials are grouped by IAM action. A warning is recorded per resource,
// so one missing permission in an account with 200 Lambda functions is 200
// warnings — found against a real account, where two denied actions already
// printed ten lines of SDK error text. The person reading this needs one answer:
// which permission to add. The full per-resource detail stays in the inventory.
//
// Every other warning kind is a finding about a specific resource — a role that
// no longer exists, an environment that cannot be read — and is listed on its own.
func writeSummary(w io.Writer, inv *inventory.Inventory, path string) {
	byType := map[string]int{}
	for _, r := range inv.Resources {
		byType[r.Type]++
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	fmt.Fprintf(w, "\nfound %d resources:\n", len(inv.Resources))
	for _, t := range types {
		fmt.Fprintf(w, "  %-28s %d\n", t, byType[t])
	}

	denied := map[string]int{}
	var findings []inventory.Warning
	for _, wn := range inv.Warnings {
		if wn.Kind == "access-denied" {
			denied[iamAction(wn.Service, wn.Op)]++
			continue
		}
		findings = append(findings, wn)
	}

	if len(inv.Warnings) > 0 {
		fmt.Fprintf(w, "\nthis inventory is incomplete:\n")
	}
	if len(denied) > 0 {
		actions := make([]string, 0, len(denied))
		for a := range denied {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		for _, a := range actions {
			fmt.Fprintf(w, "  [access-denied] %-32s %d call(s) refused\n", a, denied[a])
		}
		fmt.Fprintf(w, "  → grant %s, or accept a partial graph (policy: %s)\n",
			strings.Join(actions, ", "), awsx.PolicyPath)
	}
	for _, f := range findings {
		fmt.Fprintf(w, "  [%s] %s", f.Kind, f.Service)
		if f.Op != "" {
			fmt.Fprintf(w, ":%s", f.Op)
		}
		fmt.Fprintf(w, " — %s\n", f.Message)
	}

	fmt.Fprintf(w, "\nwrote %s\n", path)
}

// iamAction names the permission behind a denied SDK call, using the same
// allow-list the policy is generated from, so the hint cannot name an action the
// shipped policy does not contain.
func iamAction(sdkID, op string) string {
	for _, s := range awsx.Services() {
		if s.SDKID != sdkID {
			continue
		}
		if acts, ok := s.IAMActions[op]; ok && len(acts) > 0 {
			return strings.Join(acts, "+")
		}
		return s.IAMPrefix + ":" + op
	}
	if op == "" {
		return sdkID
	}
	return sdkID + ":" + op
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
		if len(s.IAMResources) > 0 {
			fmt.Printf("    (granted as %s, only on %s)\n",
				strings.Join(s.IAMActionsFor(), ", "), strings.Join(s.IAMResources, ", "))
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
	fmt.Println("  - apigateway:GET is scoped to API definitions. API key values, usage")
	fmt.Println("    plans, custom domains and client certificates are outside it.")
	fmt.Println()
	fmt.Printf("IAM actions: %s\n", strings.Join(awsx.AllIAMActions(), ", "))
}
