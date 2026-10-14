/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command kflare-gen generates the API types of the kinds in
// generator/generator.yaml from cloudflare-go v7, the SDK Cloudflare
// generates from its OpenAPI spec, and from a committed subset of that spec,
// which adds what the SDK's types leave out: lengths, patterns, ranges,
// defaults and integers. Run it from the repository's root:
//
//	kflare-gen            generates the types (make codegen, which runs before
//	                      controller-gen)
//	kflare-gen sync-spec  rewrites the spec subset from a full spec (make
//	                      sync-spec)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The repository's paths, relative to its root.
const (
	defaultConfig = "generator/generator.yaml"
	defaultSpec   = "generator/openapi/openapi.json"
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "sync-spec" {
		err = syncSpecCommand(os.Args[2:])
	} else {
		err = generateCommand(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kflare-gen:", err)
		os.Exit(1)
	}
}

func generateCommand(args []string) error {
	flags := flag.NewFlagSet("kflare-gen", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "the kinds to generate")
	specPath := flags.String("spec", defaultSpec, "the OpenAPI spec subset")
	apiDir := flags.String("api", "api/v1alpha1", "the API package the types files go to")
	headerPath := flags.String("header", "hack/boilerplate.go.txt", "the license header of every generated file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return run(*configPath, *specPath, *apiDir, *headerPath)
}

func syncSpecCommand(args []string) error {
	flags := flag.NewFlagSet("kflare-gen sync-spec", flag.ContinueOnError)
	var configs stringList
	flags.Var(&configs, "config", "a configuration whose kinds' operations the subset holds (repeatable)")
	from := flags.String("from", "", "the full OpenAPI spec, Cloudflare's api-schemas openapi.json")
	source := flags.String("source", "", "where the full spec comes from, recorded in the subset")
	out := flags.String("out", defaultSpec, "the subset to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *source == "" {
		return fmt.Errorf("sync-spec needs -from and -source")
	}
	if len(configs) == 0 {
		configs = stringList{defaultConfig}
	}
	return syncSpec(configs, sdk{dir: "."}, *from, *source, *out)
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// run generates the types files of the kinds in the configuration at
// configPath into apiDir, and removes the types files kflare-gen wrote for
// kinds the configuration no longer lists.
func run(configPath, specPath, apiDir, headerPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	spec, err := loadSpec(specPath)
	if err != nil {
		return err
	}
	header, err := os.ReadFile(headerPath)
	if err != nil {
		return err
	}
	files, err := generate(cfg, sdk{dir: "."}, spec, string(header))
	if err != nil {
		return err
	}

	existing, err := filepath.Glob(filepath.Join(apiDir, typesFile("*")))
	if err != nil {
		return err
	}
	for _, path := range existing {
		if _, ok := files[filepath.Base(path)]; !ok {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(apiDir, name), src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// generate returns the types file of every kind in cfg, by file name.
func generate(cfg *config, s sdk, spec *apiSpec, header string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, entry := range cfg.Kinds {
		svc, err := s.service(entry.Service)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Kind, err)
		}
		k, err := buildKind(entry, svc, spec)
		if err != nil {
			return nil, err
		}
		src, err := emitTypes(k, header)
		if err != nil {
			return nil, err
		}
		files[typesFile(strings.ToLower(entry.Kind))] = src
	}
	return files, nil
}

// typesFile is the name of a kind's types file. The zz_generated prefix keeps
// generated files together, after the hand-written ones.
func typesFile(lowerKind string) string {
	return "zz_generated." + lowerKind + "_types.go"
}
