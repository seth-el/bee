package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/seth-el/bee"
)

func runInit(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee init", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	home := flags.String("home", "", "Bee home directory (default: BEE_HOME or ~/.bee)")
	jsonMode := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee init does not accept positional arguments")
	}
	report, err := bee.InitializeBee(*home)
	if err != nil {
		return err
	}
	if *jsonMode {
		return json.NewEncoder(output).Encode(report)
	}
	status := "preserved"
	if report.ConfigCreated {
		status = "created"
	}
	_, err = fmt.Fprintf(output, "Bee home initialized: %s\nconfig: %s (%s)\n", report.Home, report.ConfigPath, status)
	return err
}
