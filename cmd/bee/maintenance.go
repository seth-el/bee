package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/seth-el/bee"
	"io"
	"time"
)

func runMigrations(kind string, args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet(kind, flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	opsPath := flags.String("ops-config", "", "operations config path")
	manifest := flags.String("manifest", "", "release manifest path")
	apply := flags.Bool("apply", false, "apply migrations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *opsPath == "" || *manifest == "" {
		return errors.New("--ops-config and --manifest are required")
	}
	cfg, err := bee.LoadOpsConfig(*opsPath)
	if err != nil {
		return err
	}
	report, err := bee.PlanMigrations(context.Background(), cfg, *manifest, nil)
	if *apply {
		report, err = bee.ApplyMigrations(context.Background(), cfg, *manifest, nil)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

func runMaintenance(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("maintenance subcommand is required")
	}
	if args[0] == "backup" {
		return runBackup(args[1:], output, errorOutput)
	}
	if args[0] == "restore" {
		return runRestore(args[1:], output, errorOutput)
	}
	if args[0] == "upgrade-check" || args[0] == "migrate" {
		return runMigrations(args[0], args[1:], output, errorOutput)
	}
	if args[0] != "cleanup" {
		return errors.New("unknown maintenance subcommand")
	}
	flags := flag.NewFlagSet("bee maintenance cleanup", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	opsPath := flags.String("ops-config", "", "operations config path")
	apply := flags.Bool("apply", false, "apply deletions; default is dry-run")
	maxItems := flags.Int("max-items", 0, "maximum items")
	maxBytes := flags.Int64("max-bytes", 0, "maximum bytes")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *opsPath == "" {
		return errors.New("--ops-config is required")
	}
	cfg, err := bee.LoadOpsConfig(*opsPath)
	if err != nil {
		return err
	}
	options := bee.CleanupOptions{Now: time.Now().UTC(), DryRun: !*apply, MaxItems: *maxItems, MaxBytes: *maxBytes}
	var report bee.CleanupReport
	if *apply {
		report, err = bee.ApplyCleanup(nil, cfg, options)
	} else {
		report, err = bee.PlanCleanup(nil, cfg, options)
	}
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if len(report.Errors) > 0 {
		return errors.New("cleanup completed with errors")
	}
	return nil
}

func runBackup(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee maintenance backup", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	opsPath := flags.String("ops-config", "", "operations config path")
	destination := flags.String("destination", "", "new backup archive path")
	includeArtifacts := flags.Bool("include-artifacts", false, "include bounded artifact files")
	maxBytes := flags.Int64("max-bytes", 0, "maximum uncompressed backup bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *opsPath == "" || *destination == "" || flags.NArg() != 0 {
		return errors.New("backup requires --ops-config, --destination, and no positional arguments")
	}
	cfg, err := bee.LoadOpsConfig(*opsPath)
	if err != nil {
		return err
	}
	manifest, err := bee.Backup(context.Background(), cfg, bee.BackupOptions{Destination: *destination, IncludeArtifacts: *includeArtifacts, MaxBytes: *maxBytes})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(manifest)
}

func runRestore(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee maintenance restore", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	opsPath := flags.String("ops-config", "", "operations config path")
	source := flags.String("source", "", "backup archive path")
	apply := flags.Bool("apply", false, "apply the validated restore; default is dry-run")
	maxBytes := flags.Int64("max-bytes", 0, "maximum uncompressed restore bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *opsPath == "" || *source == "" || flags.NArg() != 0 {
		return errors.New("restore requires --ops-config, --source, and no positional arguments")
	}
	cfg, err := bee.LoadOpsConfig(*opsPath)
	if err != nil {
		return err
	}
	report, err := bee.Restore(context.Background(), cfg, bee.RestoreOptions{Source: *source, Apply: *apply, MaxBytes: *maxBytes})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if len(report.Conflicts) > 0 || len(report.Errors) > 0 {
		return errors.New("restore has conflicts or errors")
	}
	return nil
}
