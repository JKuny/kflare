/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fullSpec stands in for Cloudflare's full spec: one path a kind uses, with
// a path-level parameter and nested $refs, and what no kind uses.
const fullSpec = `{
	"openapi": "3.0.3",
	"info": {"title": "Cloudflare API", "version": "4.0.0", "description": "dropped"},
	"paths": {
		"/accounts/{account_id}/things": {
			"parameters": [{"$ref": "#/components/parameters/account_id"}],
			"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/thing"}}}}},
			"delete": {"responses": {"200": {"$ref": "#/components/responses/unused"}}}
		},
		"/accounts/{account_id}/others": {"get": {"responses": {"200": {"$ref": "#/components/responses/unused"}}}}
	},
	"components": {
		"parameters": {"account_id": {"in": "path", "name": "account_id", "schema": {"$ref": "#/components/schemas/id"}}},
		"responses": {"unused": {"description": "unused"}},
		"schemas": {
			"id": {"type": "string", "maxLength": 32},
			"thing": {"properties": {
				"name": {"$ref": "#/components/schemas/a~1name"}, "size": {"type": "integer", "maximum": 10}}},
			"a/name": {"type": "string", "maxLength": 1.5e2},
			"unused": {"type": "string"}
		}
	}}`

func decodeSpec(t *testing.T, data string) map[string]any {
	t.Helper()
	var spec map[string]any
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestSpecSubset(t *testing.T) {
	subset, err := specSubset(decodeSpec(t, fullSpec),
		[]route{{verb: "post", path: "/accounts/{id}/things"}, {verb: "post", path: "/accounts/{id}/things"}}, nil,
		"the source")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(subset)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSpec(t, string(encoded))

	paths := got["paths"].(map[string]any)
	if !slices.Equal(slices.Sorted(maps.Keys(paths)), []string{"/accounts/{account_id}/things"}) {
		t.Errorf("paths = %v, want only the one the route names", slices.Sorted(maps.Keys(paths)))
	}
	item := paths["/accounts/{account_id}/things"].(map[string]any)
	if !slices.Equal(slices.Sorted(maps.Keys(item)), []string{"parameters", "post"}) {
		t.Errorf("the path keeps its parameters and the route's verb only: %v", slices.Sorted(maps.Keys(item)))
	}
	components := got["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	if !slices.Equal(slices.Sorted(maps.Keys(schemas)), []string{"a/name", "id", "thing"}) {
		t.Errorf("schemas = %v, want those the operation reaches, through nested $refs", slices.Sorted(maps.Keys(schemas)))
	}
	if _, ok := components["responses"]; ok {
		t.Error("a response only other operations use is left out")
	}
	if components["parameters"].(map[string]any)["account_id"] == nil {
		t.Error("the path's parameter is kept")
	}
	if !bytes.Contains(encoded, []byte(`"maxLength":1.5e2`)) {
		t.Errorf("numbers are copied as written: %s", encoded)
	}
	info := got["info"].(map[string]any)
	if info["x-kflare-source"] != "the source" || info["title"] != "Cloudflare API" || info["description"] != nil {
		t.Errorf("info = %v", info)
	}
}

func TestSpecSubsetRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		spec, want string
		r          route
	}{
		"a missing path": {fullSpec, "no path for", route{verb: "get", path: "/accounts/{a}/missing"}},
		"a missing verb": {fullSpec, "has no put", route{verb: "put", path: "/accounts/{a}/things"}},
		"a $ref elsewhere": {`{"paths": {"/x": {"get": {"$ref": "other.json#/x"}}}}`, "does not point into",
			route{verb: "get", path: "/x"}},
		"a dangling $ref": {`{"paths": {"/x": {"get": {"$ref": "#/components/schemas/missing"}}}}`, "is not in the spec",
			route{verb: "get", path: "/x"}},
		"a $ref in a list": {`{"paths": {"/x": {"get": [{"$ref": 5}]}}}`, "does not point into",
			route{verb: "get", path: "/x"}},
	} {
		if _, err := specSubset(decodeSpec(t, tc.spec), []route{tc.r}, nil, ""); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
}

func TestSplitRef(t *testing.T) {
	if section, name := splitRef("#/components/schemas/a~1b~0c"); section != "schemas" || name != "a/b~c" {
		t.Errorf("splitRef = %q, %q", section, name)
	}
}

// TestCommittedSubsetReproduces runs sync-spec on the committed subset itself:
// it holds every operation and component the configured kinds reach, so the
// result is the subset again, byte for byte.
func TestCommittedSubsetReproduces(t *testing.T) {
	committed, err := os.ReadFile(committedSpec)
	if err != nil {
		t.Fatal(err)
	}
	var subset struct {
		Info struct {
			Source string `json:"x-kflare-source"`
		} `json:"info"`
	}
	if err := json.Unmarshal(committed, &subset); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "openapi.json")
	err = syncSpec([]string{"generator.yaml", filepath.Join("testdata", "generator.yaml")}, sdk{dir: "."},
		committedSpec, subset.Info.Source, out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, committed) {
		t.Error("sync-spec on the committed subset changes it: the subset is not what the kinds reach")
	}
}

func TestSyncSpecRefuses(t *testing.T) {
	notJSON := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(notJSON, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := writeConfig(t, strings.Replace(validConfig, "kv.NamespaceService", "kv.NoSuchService", 1))
	if err := syncSpec([]string{"generator.yaml"}, sdk{dir: "."}, committedSpec, "source",
		filepath.Join(t.TempDir(), "missing", "openapi.json")); err == nil {
		t.Error("syncSpec into a missing directory: err = nil")
	}
	for name, args := range map[string]struct {
		configs []string
		from    string
	}{
		"a missing configuration": {[]string{"missing.yaml"}, committedSpec},
		"an unknown service":      {[]string{unknown}, committedSpec},
		"a missing spec":          {[]string{"generator.yaml"}, "missing.json"},
		"a spec that is not JSON": {[]string{"generator.yaml"}, notJSON},
	} {
		out := filepath.Join(t.TempDir(), "openapi.json")
		if err := syncSpec(args.configs, sdk{dir: "."}, args.from, "source", out); err == nil {
			t.Errorf("syncSpec with %s: err = nil", name)
		}
	}
}

func TestCommands(t *testing.T) {
	if err := syncSpecCommand(nil); err == nil || !strings.Contains(err.Error(), "needs -from and -source") {
		t.Errorf("sync-spec without -from: err = %v", err)
	}
	if err := syncSpecCommand([]string{"-nope"}); err == nil {
		t.Error("sync-spec with an unknown flag: err = nil")
	}
	if err := generateCommand([]string{"-nope"}); err == nil {
		t.Error("kflare-gen with an unknown flag: err = nil")
	}
	var list stringList
	if err := list.Set("a"); err != nil || list.Set("b") != nil || list.String() != "a,b" {
		t.Errorf("stringList = %q", list.String())
	}
}

func TestSpecSubsetCopiesSchemas(t *testing.T) {
	subset, err := specSubset(decodeSpec(t, fullSpec), nil, []string{"thing"}, "the source")
	if err != nil {
		t.Fatal(err)
	}
	schemas := subset["components"].(map[string]any)["schemas"].(map[string]any)
	if !slices.Equal(slices.Sorted(maps.Keys(schemas)), []string{"a/name", "thing"}) {
		t.Errorf("schemas = %v, want thing and what it reaches", slices.Sorted(maps.Keys(schemas)))
	}
	if _, err := specSubset(decodeSpec(t, fullSpec), nil, []string{"Missing"}, ""); err == nil {
		t.Error("a schema the spec lacks: err = nil")
	}
}
