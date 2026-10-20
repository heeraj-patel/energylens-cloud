package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"energylens.local/cloud/internal/helpexport"
)

func identityHelpExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export-unresolved-device-help", flag.ContinueOnError)
	output := fs.String("output", "", "private unresolved_device_help.jsonl output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *output == "" {
		return errors.New("usage: export-unresolved-device-help -output /private/path/unresolved_device_help.jsonl")
	}
	pool, err := operatorPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	count, err := helpexport.ExportHelp(ctx, pool, *output)
	if err != nil {
		return err
	}
	fmt.Printf("exported %d unresolved device identities\n", count)
	return nil
}
