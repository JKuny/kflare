/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command kflare-gen generates the API types of the kinds in
// generator/generator.yaml from cloudflare-go v7, the SDK Cloudflare
// generates from its OpenAPI spec. Run it from the repository's root, through
// make codegen, which runs before controller-gen: controller-gen then turns
// the types into CRDs and deepcopy functions as it does for hand-written
// ones.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	configPath := flag.String("config", "generator/generator.yaml", "the kinds to generate")
	apiDir := flag.String("api", "api/v1alpha1", "the API package the types files go to")
	headerPath := flag.String("header", "hack/boilerplate.go.txt", "the license header of every generated file")
	flag.Parse()

	if err := run(*configPath, *apiDir, *headerPath); err != nil {
		fmt.Fprintln(os.Stderr, "kflare-gen:", err)
		os.Exit(1)
	}
}

// run generates the types files of the kinds in the configuration at
// configPath into apiDir, and removes the types files kflare-gen wrote for
// kinds the configuration no longer lists.
func run(configPath, apiDir, headerPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	header, err := os.ReadFile(headerPath)
	if err != nil {
		return err
	}
	files, err := generate(cfg, sdk{dir: "."}, string(header))
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
func generate(cfg *config, s sdk, header string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, entry := range cfg.Kinds {
		svc, err := s.service(entry.Service)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Kind, err)
		}
		k, err := buildKind(entry, svc)
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
