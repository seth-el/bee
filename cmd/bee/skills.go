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

func runSkills(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("skill subcommand must be list, inspect, install, test, enable, disable, rollback, or remove")
	}
	switch args[0] {
	case "list":
		return runSkillList(args[1:], output, errorOutput)
	case "inspect":
		return runSkillInspect(args[1:], output, errorOutput)
	case "install":
		return runSkillInstall(args[1:], output, errorOutput)
	case "test":
		return runSkillTest(args[1:], output, errorOutput)
	case "enable":
		return runSkillEnable(args[1:], output, errorOutput)
	case "disable":
		return runSkillDisable(args[1:], output, errorOutput)
	case "rollback":
		return runSkillRollback(args[1:], output, errorOutput)
	case "remove":
		return runSkillRemove(args[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown skill subcommand %q", args[0])
	}
}

func newSkillFlagSet(name string, errorOutput io.Writer) (*flag.FlagSet, *string, *bool) {
	flags := flag.NewFlagSet("bee skill "+name, flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	root := flags.String("root", "", "skill registry root (default: BEE_HOME/skills)")
	jsonMode := flags.Bool("json", false, "emit JSON")
	return flags, root, jsonMode
}

func openSkillRegistry(root string) (*bee.SkillRegistry, error) {
	if strings.TrimSpace(root) == "" {
		return bee.NewDefaultSkillRegistry()
	}
	return bee.NewSkillRegistry(root)
}

func writeSkillValue(output io.Writer, jsonMode bool, value any, human func(io.Writer) error) error {
	if jsonMode {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	return human(output)
}

func runSkillList(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("list", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee skill list does not accept positional arguments")
	}
	registry, err := openSkillRegistry(*root)
	if err != nil {
		return err
	}
	packages, err := registry.List(context.Background())
	if err != nil {
		return err
	}
	return writeSkillValue(output, *jsonMode, packages, func(writer io.Writer) error {
		if len(packages) == 0 {
			_, err := fmt.Fprintln(writer, "No skills installed.")
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

func runSkillInspect(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("inspect", errorOutput)
	version := flags.String("version", "", "installed version (default: active or latest)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee skill inspect requires one skill name")
	}
	registry, err := openSkillRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Inspect(context.Background(), flags.Arg(0), *version)
	if err != nil {
		return err
	}
	return writeSkillValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "%s@%s\n%s\nchecksum: %s\npath: %s\nenabled: %t\nexecutable: %t\n", pkg.Manifest.Name, pkg.Manifest.Version, pkg.Manifest.Description, pkg.Checksum, pkg.Path, pkg.Enabled, pkg.Manifest.Extension != nil)
		return err
	})
}

func runSkillInstall(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("install", errorOutput)
	enable := flags.Bool("enable", false, "enable the installed revision immediately")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee skill install requires one package directory")
	}
	registry, err := openSkillRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Install(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	if *enable {
		pkg, err = registry.Enable(context.Background(), pkg.Manifest.Name, pkg.Manifest.Version)
		if err != nil {
			return err
		}
	}
	return writeSkillValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		status := "installed"
		if pkg.Enabled {
			status = "installed and enabled"
		}
		_, err := fmt.Fprintf(writer, "%s %s@%s\n", status, pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runSkillTest(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("test", errorOutput)
	version := flags.String("version", "", "installed version (default: active or latest)")
	source := flags.String("source", "", "validate a package directory before installation")
	startExtension := flags.Bool("start-extension", false, "start an optional extension and verify its initialize handshake")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source != "" && flags.NArg() != 0 {
		return errors.New("bee skill test --source cannot be combined with a skill name")
	}
	if *source == "" && flags.NArg() != 1 {
		return errors.New("bee skill test requires one installed skill name or --source PATH")
	}
	var report bee.SkillTestReport
	var err error
	if *source != "" {
		report, err = bee.TestSkillSource(context.Background(), *source, *startExtension)
	} else {
		registry, openErr := openSkillRegistry(*root)
		if openErr != nil {
			return openErr
		}
		report, err = registry.TestInstalled(context.Background(), flags.Arg(0), *version, *startExtension)
	}
	if err != nil {
		return err
	}
	return writeSkillValue(output, *jsonMode, report, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "valid %s@%s (%d files, %d bytes, %d instruction bytes)\n", report.Package.Manifest.Name, report.Package.Manifest.Version, report.Files, report.PackageBytes, report.Instructions)
		if err == nil && report.ExtensionTest != "" {
			_, err = fmt.Fprintf(writer, "extension: %s\n", report.ExtensionTest)
		}
		return err
	})
}

func runSkillEnable(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("enable", errorOutput)
	version := flags.String("version", "", "version to enable (default: latest)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee skill enable requires one skill name")
	}
	registry, err := openSkillRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Enable(context.Background(), flags.Arg(0), *version)
	if err != nil {
		return err
	}
	return writeSkillValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "enabled %s@%s\n", pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runSkillDisable(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("disable", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee skill disable requires one skill name")
	}
	registry, err := openSkillRegistry(*root)
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
	return writeSkillValue(output, *jsonMode, result, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "disabled %s (active revision retained: %s)\n", flags.Arg(0), activation.ActiveVersion)
		return err
	})
}

func runSkillRollback(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("rollback", errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("bee skill rollback requires one skill name")
	}
	registry, err := openSkillRegistry(*root)
	if err != nil {
		return err
	}
	pkg, err := registry.Rollback(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	return writeSkillValue(output, *jsonMode, pkg, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "rolled back %s to %s\n", pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}

func runSkillRemove(args []string, output, errorOutput io.Writer) error {
	flags, root, jsonMode := newSkillFlagSet("remove", errorOutput)
	version := flags.String("version", "", "installed version to remove (required)")
	apply := flags.Bool("apply", false, "remove the package; default is dry-run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *version == "" {
		return errors.New("bee skill remove requires one skill name and --version VERSION")
	}
	registry, err := openSkillRegistry(*root)
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
	return writeSkillValue(output, *jsonMode, result, func(writer io.Writer) error {
		action := "would remove"
		if *apply {
			action = "removed"
		}
		_, err := fmt.Fprintf(writer, "%s %s@%s\n", action, pkg.Manifest.Name, pkg.Manifest.Version)
		return err
	})
}
