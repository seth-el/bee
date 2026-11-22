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

func runPlugins(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("plugin subcommand must be list, inspect, install, test, enable, disable, rollback, or remove")
	}
	switch args[0] {
	case "list":
		return runPluginList(args[1:], output, errorOutput)
	case "inspect":
		return runPluginInspect(args[1:], output, errorOutput)
	case "install":
		return runPluginInstall(args[1:], output, errorOutput)
	case "test":
		return runPluginTest(args[1:], output, errorOutput)
	case "enable":
		return runPluginEnable(args[1:], output, errorOutput)
	case "disable":
		return runPluginDisable(args[1:], output, errorOutput)
	case "rollback":
		return runPluginRollback(args[1:], output, errorOutput)
	case "remove":
		return runPluginRemove(args[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown plugin subcommand %q", args[0])
	}
}

func newPluginFlagSet(name string, errorOutput io.Writer) (*flag.FlagSet, *string, *bool) {
	flags := flag.NewFlagSet("bee plugin "+name, flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	root := flags.String("root", "", "plugin registry root (default: BEE_HOME/plugins)")
	jsonMode := flags.Bool("json", false, "emit JSON")
	return flags, root, jsonMode
}

func openPluginRegistry(root string) (*bee.PluginRegistry, error) {
	if strings.TrimSpace(root) == "" {
		return bee.NewDefaultPluginRegistry()
	}
	return bee.NewPluginRegistry(root)
}

func writePluginValue(output io.Writer, jsonMode bool, value any, human func(io.Writer) error) error {
	if jsonMode {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	return human(output)
}

func runPluginList(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("list", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee plugin list does not accept positional arguments")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	packages, err := registry.List(context.Background())
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, packages, func(writer io.Writer) error {
		if len(packages) == 0 {
			_, err := fmt.Fprintln(writer, "No plugins installed.")
			return err
		}
		for _, pkg := range packages {
			status := "disabled"
			if pkg.Enabled {
				status = "enabled"
			} else if pkg.Active {
				status = "inactive"
			}
			if _, err := fmt.Fprintf(writer, "%s@%s  %s  %s\n", pkg.Manifest.Name, pkg.Manifest.Version, status, pkg.Manifest.Description); err != nil {
				return err
			}
		}
		return nil
	})
}

func runPluginInspect(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("inspect", errorOutput)
	version := flags.String("version", "", "installed version (default: active or latest)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee plugin inspect requires one plugin name")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Inspect(context.Background(), flags.Arg(0), *version)
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "%s@%s\n%s\nchecksum: %s\npath: %s\nenabled: %t\ntools: %s\n", pkg.Manifest.Name, pkg.Manifest.Version, pkg.Manifest.Description, pkg.Checksum, pkg.Path, pkg.Enabled, strings.Join(pkg.Manifest.Extension.Tools, ", "))
		return err
	})
}

func runPluginInstall(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("install", errorOutput)
	enable := flags.Bool("enable", false, "enable the installed revision immediately")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee plugin install requires one package directory")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Install(context.Background(), flags.Arg(0))
	if err == nil && *enable {
		pkg, err = registry.Enable(context.Background(), pkg.Manifest.Name, pkg.Manifest.Version)
	}
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		status := "installed"
		if pkg.Enabled {
			status = "installed and enabled"
		}
		_, err := fmt.Fprintf(writer, "%s %s@%s\n", status, pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runPluginTest(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("test", errorOutput)
	version := flags.String("version", "", "installed version (default: active or latest)")
	source := flags.String("source", "", "validate a package directory before installation")
	startExtension := flags.Bool("start-extension", false, "start the plugin and verify its initialize handshake")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source != "" && flags.NArg() != 0 {
		return errors.New("bee plugin test --source cannot be combined with a plugin name")
	}
	if *source == "" && flags.NArg() != 1 {
		return errors.New("bee plugin test requires one installed plugin name or --source PATH")
	}
	var report bee.PluginTestReport
	var err error
	if *source != "" {
		report, err = bee.TestPluginSource(context.Background(), *source, *startExtension)
	} else {
		registry, openErr := openPluginRegistry(*root)
		if openErr != nil {
			return openErr
		}
		report, err = registry.TestInstalled(context.Background(), flags.Arg(0), *version, *startExtension)
	}
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, report, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "valid %s@%s (%d files, %d bytes)\n", report.Package.Manifest.Name, report.Package.Manifest.Version, report.Files, report.PackageBytes)
		if err == nil && report.ExtensionTest != "" {
			_, err = fmt.Fprintf(writer, "extension: %s\n", report.ExtensionTest)
		}
		return err
	})
}

func runPluginEnable(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("enable", errorOutput)
	version := flags.String("version", "", "version to enable (default: latest)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee plugin enable requires one plugin name")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Enable(context.Background(), flags.Arg(0), *version)
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "enabled %s@%s; restart Bee to load its tools\n", pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runPluginDisable(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("disable", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee plugin disable requires one plugin name")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	activation, err := registry.Disable(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	result := struct {
		Name       string              `json:"name"`
		Activation bee.SkillActivation `json:"activation"`
	}{Name: flags.Arg(0), Activation: activation}
	return writePluginValue(output, *jsonMode, result, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "disabled %s; restart Bee to unload its tools\n", flags.Arg(0))
		return err
	})
}

func runPluginRollback(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("rollback", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee plugin rollback requires one plugin name")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Rollback(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	return writePluginValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "rolled back %s to %s; restart Bee to load it\n", pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runPluginRemove(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newPluginFlagSet("remove", errorOutput)
	version := flags.String("version", "", "installed version to remove (required)")
	apply := flags.Bool("apply", false, "remove the package; default is dry-run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *version == "" {
		return errors.New("bee plugin remove requires one plugin name and --version VERSION")
	}
	registry, err := openPluginRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Inspect(context.Background(), flags.Arg(0), *version)
	if err != nil {
		return err
	}
	if err := registry.CheckRemove(context.Background(), pkg.Manifest.Name, pkg.Manifest.Version); err != nil {
		return err
	}
	result := struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Applied bool   `json:"applied"`
	}{Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Applied: *apply}
	if *apply {
		if err := registry.Remove(context.Background(), pkg.Manifest.Name, pkg.Manifest.Version); err != nil {
			return err
		}
	}
	return writePluginValue(output, *jsonMode, result, func(writer io.Writer) error {
		action := "would remove"
		if *apply {
			action = "removed"
		}
		_, err := fmt.Fprintf(writer, "%s %s@%s\n", action, pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}
