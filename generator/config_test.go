/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validConfig is a configuration loadConfig accepts.
const validConfig = `kinds:
  - kind: KVNamespace
    service: kv.NamespaceService
    scope: account
    shortName: cfkv
    noun: KV namespace
    doc: KVNamespace is a Workers KV namespace.
    id: {field: id, status: namespaceID}
    claimLabel: kflare.dev/kv-namespace-id
    adopt: {by: title}
    sample: {title: example}
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "generator.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Kinds) != 1 || cfg.Kinds[0].Kind != "KVNamespace" || cfg.Kinds[0].ID.Status != "namespaceID" {
		t.Errorf("loadConfig = %+v", cfg)
	}

	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("loadConfig of a missing file: err = nil")
	}
}

// dependent returns a dependents key with one dependent.
func dependent(kind, refs, reason string) string {
	return "    dependents:\n      - kind: " + kind + "\n        refs: " + refs + "\n        reason: " + reason + "\n"
}

func TestLoadConfigRefuses(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(validConfig, old) {
			t.Fatalf("validConfig has no %q", old)
		}
		return strings.Replace(validConfig, old, new, 1)
	}
	tests := []struct{ name, config, want string }{
		{"YAML that does not parse", "kinds: [", "error converting YAML"},
		{"an unknown key", validConfig + "    colour: blue\n", "unknown field"},
		{"no kinds", "kinds: []\n", "no kinds"},
		{"a kind listed twice", validConfig + strings.TrimPrefix(validConfig, "kinds:\n"), "listed twice"},
		{"a kind that is not a Go name", replace("kind: KVNamespace", "kind: kv-namespace"), "not a Go type name"},
		{"a service without a type", replace("service: kv.NamespaceService", "service: kv"), "not <package>.<type>"},
		{"a service ending in a dot", replace("service: kv.NamespaceService", "service: kv."), "not <package>.<type>"},
		{"another scope", replace("scope: account", "scope: zone"), "the only scope is account"},
		{"a short name in capitals", replace("shortName: cfkv", "shortName: CFKV"), "lower-case"},
		{"no noun", replace("noun: KV namespace", `noun: ""`), "noun is empty"},
		{"a doc not starting with the kind", replace("doc: KVNamespace is", "doc: It is"), "does not start with"},
		{"no ID field", replace("{field: id,", `{field: "",`), "id.field is empty"},
		{"an ID status name in capitals", replace("status: namespaceID", "status: NamespaceID"), "lowerCamel"},
		{"a claim label outside kflare.dev", replace("kflare.dev/kv-namespace-id", "example.com/id"), "kflare.dev/"},
		{"an ignore without a reason", validConfig + "    ignore:\n      jurisdiction: \" \"\n", "gives no reason"},
		{"a field both ignored and adjusted", validConfig + "    ignore:\n      title: why\n    fields:\n      title: {}\n",
			"both ignored and adjusted"},
		{"a validation without a reason", validConfig + "    fields:\n      title: {validation: {maxLength: 2}}\n",
			"fields.title.validation: gives no reason"},
		{"a minimum length above the maximum",
			validConfig + "    fields:\n      title: {validation: {minLength: 5, maxLength: 2, reason: x}}\n",
			"minLength is above maxLength"},
		{"a minimum above the maximum",
			validConfig + "    fields:\n      size: {validation: {minimum: 5, maximum: 2, reason: x}}\n",
			"minimum is above maximum"},
		{"a pattern that does not compile",
			validConfig + "    fields:\n      title: {validation: {pattern: \"[a-\", reason: x}}\n",
			"the pattern does not compile"},
		{"a pattern with a backquote",
			validConfig + "    fields:\n      title: {validation: {pattern: \"^`$\", reason: x}}\n",
			"backquote"},
		{"a field both ignored and fixed", validConfig + "    ignore:\n      kind: why\n    fixed:\n      kind: ip\n",
			"both ignored and fixed"},
		{"a field both fixed and adjusted", validConfig + "    fixed:\n      kind: ip\n    fields:\n      kind: {}\n",
			"both fixed and adjusted"},
		{"no adoption key", replace("adopt: {by: title}", "adopt: {}"), "adopt.by is empty"},
		{"no sample", replace("sample: {title: example}", "sample: {}"), "sample is empty"},
		{"a dependent that is not a Go name", validConfig + dependent("worker-script", "bound", "why"),
			"not a Go type name"},
		{"a dependent listed twice", validConfig + dependent("WorkerScript", "bound", "why") +
			strings.TrimPrefix(dependent("WorkerScript", "bound", "why"), "    dependents:\n"), "listed twice"},
		{"a dependent's refs that is exported", validConfig + dependent("WorkerScript", "Bound", "why"),
			"not an unexported Go name"},
		{"a dependent without a reason", validConfig + dependent("WorkerScript", "bound", `" "`), "gives no reason"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.config))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadConfig: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
