package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/seth-el/bee"
)

func runHealth(kind string, args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet(kind, flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	opsPath := flags.String("ops-config", "", "operations config path")
	jsonMode := flags.Bool("json", false, "emit JSON")
	human := flags.Bool("human", false, "emit human output")
	checkProvider := flags.Bool("check-provider", false, "run explicit provider check")
	checkChannel := flags.Bool("check-channel", false, "run explicit channel check")
	configPath := flags.String("config", "", "Bee config whose model servers --check-provider probes (default ~/.bee/config.json)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *opsPath == "" {
		return errors.New("--ops-config is required")
	}
	cfg, err := bee.LoadOpsConfig(*opsPath)
	if err != nil {
		return err
	}
	report := bee.CheckHealth(nil, cfg)
	if kind == "readiness" {
		options := bee.ReadinessOptions{CheckProvider: *checkProvider, CheckChannel: *checkChannel}
		if *checkProvider {
			options.ProviderCheck = func(ctx context.Context) error {
				config, err := bee.LoadConfig(*configPath)
				if err != nil {
					return err
				}
				for _, target := range bee.ProviderReachabilityTargets(config) {
					if err := bee.ProbeModelServer(ctx, nil, target.Provider.BaseURL, bee.ProviderHealthTimeout); err != nil {
						return fmt.Errorf("%s: %w", target.Name, err)
					}
				}
				return nil
			}
			if options.Timeout <= 0 {
				options.Timeout = 2*bee.ProviderHealthTimeout + 5*time.Second
			}
		}
		report = bee.CheckReadiness(nil, cfg, options)
	}
	if *human && !*jsonMode {
		fmt.Fprint(output, report.Human())
	} else {
		if err := json.NewEncoder(output).Encode(report); err != nil {
			return err
		}
	}
	if report.Status == bee.CheckFail {
		return errors.New("health/readiness checks failed")
	}
	return nil
}
