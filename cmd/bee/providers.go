package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/seth-el/bee"
)

func runProviders(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 || args[0] != "accept" {
		return errors.New("providers subcommand must be `accept`")
	}
	return providersAccept(args[1:], output, errorOutput)
}

func providersAccept(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee providers accept", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "path to the Bee JSON config")
	profiles := flags.String("profiles", "", "comma-separated profiles to probe (default: main and child roles)")
	allowCloud := flags.Bool("allow-cloud-probe", false, "explicitly permit requests to manual/cloud profiles")
	timeout := flags.Duration("timeout", bee.DefaultProviderAcceptanceTimeout, "maximum duration per endpoint probe")
	nonce := flags.String("nonce", "", "bounded acceptance nonce")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *timeout <= 0 || *timeout > bee.MaxProviderAcceptanceTimeout {
		return fmt.Errorf("--timeout must be between 1ms and %s", bee.MaxProviderAcceptanceTimeout)
	}
	config, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	var selected []string
	if strings.TrimSpace(*profiles) != "" {
		for _, value := range strings.Split(*profiles, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				return errors.New("--profiles contains an empty profile name")
			}
			selected = append(selected, value)
		}
	}
	report := bee.RunProviderAcceptance(context.Background(), config, selected, bee.ProviderAcceptanceOptions{AllowCloudProbe: *allowCloud, Timeout: *timeout, Nonce: *nonce})
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if !report.Passed {
		return errors.New("provider endpoint acceptance failed")
	}
	return nil
}
