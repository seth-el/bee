package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/seth-el/bee"
)

func runConfig(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("bee config requires validate, effective, or migrate")
	}
	switch args[0] {
	case "validate":
		flags := flag.NewFlagSet("bee config validate", flag.ContinueOnError)
		flags.SetOutput(errorOutput)
		path := flags.String("config", "", "path to the primary JSON config file")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("bee config validate does not accept positional arguments")
		}
		config, err := bee.LoadConfig(*path)
		if err != nil {
			return err
		}
		result := struct {
			Valid         bool     `json:"valid"`
			SchemaVersion int      `json:"schema_version"`
			IgnoredFields []string `json:"ignored_fields,omitempty"`
		}{Valid: true, SchemaVersion: config.SchemaVersion, IgnoredFields: config.UnknownFields()}
		if *jsonOutput {
			return encodeValue(output, result)
		}
		for _, line := range bee.UnknownConfigFieldWarnings(config) {
			fmt.Fprintln(errorOutput, line)
		}
		_, err = fmt.Fprintf(output, "valid config schema_version=%d\n", result.SchemaVersion)
		return err
	case "effective":
		flags := flag.NewFlagSet("bee config effective", flag.ContinueOnError)
		flags.SetOutput(errorOutput)
		path := flags.String("config", "", "path to the primary JSON config file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("bee config effective does not accept positional arguments")
		}
		config, err := bee.LoadConfig(*path)
		if err != nil {
			return err
		}
		data, err := bee.EffectiveConfigJSON(config)
		if err != nil {
			return err
		}
		_, err = output.Write(data)
		return err
	case "migrate":
		flags := flag.NewFlagSet("bee config migrate", flag.ContinueOnError)
		flags.SetOutput(errorOutput)
		path := flags.String("config", "", "path to the primary JSON config file")
		apply := flags.Bool("apply", false, "atomically apply the migration")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("bee config migrate does not accept positional arguments")
		}
		report, migrated, err := bee.MigrateConfigFile(*path, *apply)
		if err != nil {
			return err
		}
		if !*apply {
			if _, err := output.Write(migrated); err != nil {
				return err
			}
			_, err = fmt.Fprintf(errorOutput, "dry-run: schema_version %d -> %d (changed=%t)\n", report.FromVersion, report.ToVersion, report.Changed)
			return err
		}
		_, err = fmt.Fprintf(output, "migrated config schema_version %d -> %d (changed=%t)\n", report.FromVersion, report.ToVersion, report.Changed)
		return err
	default:
		return fmt.Errorf("unknown bee config command %q", args[0])
	}
}
