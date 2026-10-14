/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// boilerplate is the license header the repository's generated files carry.
var boilerplate = filepath.Join("..", "hack", "boilerplate.go.txt")

// committedSpec is the OpenAPI subset make codegen reads.
var committedSpec = filepath.Join("openapi", "openapi.json")

// loadTestSpec loads the committed OpenAPI subset.
func loadTestSpec(t *testing.T) *apiSpec {
	t.Helper()
	spec, err := loadSpec(committedSpec)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestGolden generates the kinds of testdata/generator.yaml, each from a real
// cloudflare-go v7 service, and compares them with their golden files.
func TestGolden(t *testing.T) {
	config := filepath.Join("testdata", "generator.yaml")
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	files, err := generate(cfg, sdk{dir: "."}, loadTestSpec(t), "// Test header.")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(cfg.Kinds) {
		t.Fatalf("generated %d files for %d kinds", len(files), len(cfg.Kinds))
	}
	for name, got := range files {
		golden := filepath.Join("testdata", name+".golden")
		if *update {
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (go test ./generator -run TestGolden -update writes it)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s: review the change, then go test ./generator -run TestGolden -update",
				name, golden)
		}
	}
}

// TestCommittedTypesAreUpToDate fails when the generated types in
// api/v1alpha1 are not what kflare-gen generates from generator.yaml: make
// codegen regenerates them.
func TestCommittedTypesAreUpToDate(t *testing.T) {
	cfg, err := loadConfig("generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(boilerplate)
	if err != nil {
		t.Fatal(err)
	}
	files, err := generate(cfg, sdk{dir: "."}, loadTestSpec(t), string(header))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join("..", "api", "v1alpha1", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("api/v1alpha1/%s is not what kflare-gen generates (%v): run make codegen", name, err)
		}
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{typesFile("gone"), "zz_generated.deepcopy.go", "kept_types.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package v1alpha1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(filepath.Join("testdata", "generator.yaml"), committedSpec, dir, boilerplate); err != nil {
		t.Fatalf("run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{
		"kept_types.go", "zz_generated.deepcopy.go", "zz_generated.testiplist_types.go",
		"zz_generated.testkvnamespace_types.go", "zz_generated.testlist_types.go", "zz_generated.testr2bucket_types.go",
	}; !slices.Equal(names, want) {
		t.Errorf("after run: %q, want %q: the stale generated file removed, the others kept", names, want)
	}
	src, err := os.ReadFile(filepath.Join(dir, "zz_generated.testlist_types.go"))
	if err != nil || !strings.HasPrefix(string(src), "/*\nCopyright") || !strings.Contains(string(src), generatedMarker) {
		t.Errorf("a generated file starts with the license header and carries the generated marker: %v", err)
	}
}

func TestRunRefuses(t *testing.T) {
	config := filepath.Join("testdata", "generator.yaml")
	unknown := writeConfig(t, strings.Replace(validConfig, "kv.NamespaceService", "kv.NoSuchService", 1))
	for name, args := range map[string][4]string{
		"a missing configuration": {"missing.yaml", committedSpec, t.TempDir(), boilerplate},
		"a missing spec":          {config, "missing.json", t.TempDir(), boilerplate},
		"a missing header":        {config, committedSpec, t.TempDir(), "missing.txt"},
		"an unknown service":      {unknown, committedSpec, t.TempDir(), boilerplate},
		"a missing API directory": {config, committedSpec, filepath.Join(t.TempDir(), "missing"), boilerplate},
	} {
		if err := run(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("run with %s: err = nil", name)
		}
	}
}
