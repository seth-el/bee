package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/seth-el/bee"
)

func runMemorySemantic(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("memory semantic command is required: rebuild, status, or verify")
	}
	switch args[0] {
	case "rebuild":
		return memorySemanticRebuild(args[1:], output, errorOutput)
	case "status":
		return memorySemanticStatus(args[1:], output, errorOutput)
	case "verify":
		return memorySemanticVerify(args[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown memory semantic command %q", args[0])
	}
}

func memorySemanticRebuild(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory semantic rebuild", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "Bee config path (default: BEE_HOME/config.json when present)")
	memoryRoot := flags.String("memory-root", "", "Bee memory root (default: configured assistant memory root or workspace/memory)")
	indexRoot := flags.String("index-root", "", "semantic index root (default: configured semantic index root or state/memory-semantic)")
	endpoint := flags.String("endpoint", "", "OpenAI-compatible embeddings endpoint override")
	model := flags.String("model", "", "embedding model override")
	apiKeyFile := flags.String("api-key-file", "", "owner-only embedding API key file override")
	timeoutSeconds := flags.Int("timeout-seconds", 0, "embedding timeout override in seconds")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory semantic command does not accept positional arguments")
	}
	config, err := loadSemanticCommandConfig(*configPath)
	if err != nil {
		return err
	}
	semantic := config.Semantic
	if strings.TrimSpace(*endpoint) != "" {
		semantic.Endpoint, semantic.BaseURL = strings.TrimSpace(*endpoint), ""
	}
	if strings.TrimSpace(*model) != "" {
		semantic.Model = strings.TrimSpace(*model)
	}
	if strings.TrimSpace(*apiKeyFile) != "" {
		semantic.APIKeyFile = strings.TrimSpace(*apiKeyFile)
	}
	if *timeoutSeconds != 0 {
		semantic.TimeoutSeconds = *timeoutSeconds
	}
	if err := semantic.Validate(); err != nil {
		return err
	}
	resolvedMemoryRoot, err := semanticMemoryRoot(config, *memoryRoot)
	if err != nil {
		return err
	}
	resolvedIndexRoot, err := semanticIndexRoot(semantic, *indexRoot)
	if err != nil {
		return err
	}
	embedder, err := semantic.NewEmbedder()
	if err != nil {
		return err
	}
	manifest, err := bee.BuildSemanticIndex(context.Background(), resolvedMemoryRoot, resolvedIndexRoot, semantic.EffectiveModel(), embedder)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(manifest)
}
func memorySemanticStatus(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory semantic status", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "Bee config path (default: BEE_HOME/config.json when present)")
	indexRoot := flags.String("index-root", "", "semantic index root override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory semantic command does not accept positional arguments")
	}
	config, err := loadSemanticCommandConfig(*configPath)
	if err != nil {
		return err
	}
	resolvedIndexRoot, err := semanticIndexRoot(config.Semantic, *indexRoot)
	if err != nil {
		return err
	}
	manifest, err := bee.SemanticIndexStatus(resolvedIndexRoot)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(manifest)
}
func memorySemanticVerify(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory semantic verify", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "Bee config path (default: BEE_HOME/config.json when present)")
	memoryRoot := flags.String("memory-root", "", "Bee memory root override")
	indexRoot := flags.String("index-root", "", "semantic index root override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory semantic command does not accept positional arguments")
	}
	config, err := loadSemanticCommandConfig(*configPath)
	if err != nil {
		return err
	}
	resolvedMemoryRoot, err := semanticMemoryRoot(config, *memoryRoot)
	if err != nil {
		return err
	}
	resolvedIndexRoot, err := semanticIndexRoot(config.Semantic, *indexRoot)
	if err != nil {
		return err
	}
	manifest, err := bee.VerifySemanticIndex(resolvedMemoryRoot, resolvedIndexRoot)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(manifest)
}

func loadSemanticCommandConfig(path string) (bee.Config, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		return bee.LoadConfig(path)
	}
	defaultPath, err := bee.DefaultConfigPath()
	if err != nil {
		return bee.Config{}, err
	}
	if _, err := os.Stat(defaultPath); err == nil {
		return bee.LoadConfig(defaultPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return bee.Config{}, err
	}
	return bee.DefaultConfig(), nil
}

func semanticMemoryRoot(config bee.Config, override string) (string, error) {
	root := strings.TrimSpace(override)
	if root == "" {
		root = strings.TrimSpace(config.Assistant.MemoryRoot)
	}
	if root == "" {
		return bee.DefaultBeeMemoryRoot()
	}
	return bee.ResolveBeeHomePath(root)
}

func semanticIndexRoot(config bee.SemanticConfig, override string) (string, error) {
	if strings.TrimSpace(override) == "" {
		return config.EffectiveIndexRoot()
	}
	return bee.ResolveBeeHomePath(override)
}
