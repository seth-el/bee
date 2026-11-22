package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/seth-el/bee"
)

func runDoctor(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee doctor", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	home := flags.String("home", "", "Bee home directory (default: BEE_HOME or ~/.bee)")
	configPath := flags.String("config", "", "path to the Bee JSON config")
	jsonMode := flags.Bool("json", false, "emit JSON")
	strict := flags.Bool("strict", false, "treat warnings as failures")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee doctor does not accept positional arguments")
	}
	report := bee.RunDoctor(*home, *configPath)
	if *jsonMode {
		if err := json.NewEncoder(output).Encode(report); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(output, report.Human()); err != nil {
			return err
		}
	}
	if report.Status == bee.CheckFail || (*strict && report.Status == bee.CheckWarn) {
		return fmt.Errorf("bee doctor reported %s", report.Status)
	}
	return nil
}
