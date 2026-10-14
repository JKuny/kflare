/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"go/types"
	"slices"
	"strings"
	"testing"
)

// kvEntry is an entry for the KV namespace service that ignores nothing.
func kvEntry() kindConfig {
	return kindConfig{
		Kind: "TestKV", Service: "kv.NamespaceService", Scope: "account", ShortName: "testkv", Noun: "KV namespace",
		Doc: "TestKV is a KV namespace.", ID: idConfig{Field: "id", Status: "namespaceID"},
		ClaimLabel: "kflare.dev/test-kv-namespace-id",
	}
}

// fieldNamed returns the field goName of fields.
func fieldNamed(t *testing.T, fields []field, goName string) field {
	t.Helper()
	i := slices.IndexFunc(fields, func(f field) bool { return f.goName == goName })
	if i < 0 {
		t.Fatalf("no field %s", goName)
	}
	return fields[i]
}

func TestBuildKindMapsTheKVNamespaceService(t *testing.T) {
	k, err := buildKind(kvEntry(), loadTestService(t, "kv.NamespaceService"))
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	names := func(fields []field) (out []string) {
		for _, f := range fields {
			out = append(out, f.goName)
		}
		return out
	}
	if got := names(k.spec); !slices.Equal(got, []string{"AccountRef", "Title", "Jurisdiction"}) {
		t.Errorf("spec = %q", got)
	}
	if got := names(k.metadata); !slices.Equal(got, []string{"NamespaceID", "SupportsURLEncoding"}) {
		t.Errorf("metadata = %q", got)
	}

	if title := fieldNamed(t, k.spec, "Title"); title.optional || len(title.markers) != 0 {
		t.Errorf("title, required and updatable: %+v", title)
	}
	jurisdiction := fieldNamed(t, k.spec, "Jurisdiction")
	if !jurisdiction.optional || jurisdiction.goType != "string" || !slices.Equal(jurisdiction.markers, []string{
		"+kubebuilder:validation:Enum=eu;fedramp;us", immutable("jurisdiction"),
	}) {
		t.Errorf("jurisdiction, an optional create-only enum: %+v", jurisdiction)
	}
	if len(k.specRules) != 1 || !strings.Contains(k.specRules[0], "has(self.jurisdiction) == has(oldSelf.jurisdiction)") {
		t.Errorf("specRules = %q, want the rule that keeps jurisdiction from being added or removed", k.specRules)
	}
	if url := fieldNamed(t, k.metadata, "SupportsURLEncoding"); url.goType != "bool" || !url.optional {
		t.Errorf("a status bool is optional and no pointer: %+v", url)
	}

	columns := make([]string, 0, len(k.printColumns))
	for _, c := range k.printColumns {
		columns = append(columns, c.name+" "+c.typ+" "+c.jsonPath)
	}
	if !slices.Equal(columns, []string{
		"Namespace ID string .status.cloudflareMetadata.namespaceID",
		`Ready string .status.conditions[?(@.type=="Ready")].status`,
		"Age date .metadata.creationTimestamp",
	}) {
		t.Errorf("printColumns = %q", columns)
	}
}

func TestBuildKindMapsBooleans(t *testing.T) {
	str, boolean := types.Typ[types.String], types.Typ[types.Bool]
	svc := &service{
		create: []sdkField{
			{apiName: "account_id", in: "path", typ: str, required: true},
			{goName: "Paused", apiName: "paused", in: "body", typ: boolean},
			{goName: "Proxied", apiName: "proxied", in: "body", typ: boolean, required: true},
		},
		update: []sdkField{{goName: "Paused", apiName: "paused", in: "body", typ: boolean}},
		object: []sdkField{{goName: "ID", apiName: "id", in: "body", typ: str, required: true}},
	}
	entry := kvEntry()
	entry.Fields = map[string]fieldConfig{"paused": {PrintColumn: "Paused"}}
	k, err := buildKind(entry, svc)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if paused := fieldNamed(t, k.spec, "Paused"); paused.goType != "*bool" || !paused.optional {
		t.Errorf("an optional bool is a pointer: %+v", paused)
	}
	if proxied := fieldNamed(t, k.spec, "Proxied"); proxied.goType != "bool" || proxied.optional {
		t.Errorf("a required bool is no pointer: %+v", proxied)
	}
	if k.printColumns[0] != (printColumn{name: "Paused", typ: "boolean", jsonPath: ".spec.paused"}) {
		t.Errorf("printColumns[0] = %+v", k.printColumns[0])
	}
}

func TestBuildKindRefuses(t *testing.T) {
	str := types.Typ[types.String]
	updateOnly := &service{
		create: []sdkField{{apiName: "account_id", in: "path", typ: str, required: true}},
		update: []sdkField{{goName: "Color", apiName: "color", in: "body", typ: str}},
		object: []sdkField{{goName: "ID", apiName: "id", in: "body", typ: str, required: true}},
	}
	maxLength := 1
	with := func(change func(*kindConfig)) kindConfig {
		entry := kvEntry()
		change(&entry)
		return entry
	}
	tests := []struct {
		name    string
		service string
		svc     *service
		entry   kindConfig
		want    string
	}{
		{name: "a number not ignored", service: "rules.ListService", entry: with(func(e *kindConfig) {}),
			want: "num_items is a float64"},
		{name: "an ignore naming no field", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) { e.Ignore = map[string]string{"nope": "why"} }),
			want:  "ignore.nope: the service has no such field"},
		{name: "an adjustment naming no field", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) { e.Fields = map[string]fieldConfig{"nope": {}} }),
			want:  "fields.nope: the service has no such field"},
		{name: "an ignored required field", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) { e.Ignore = map[string]string{"title": "why"} }),
			want:  "title is required to create the object"},
		{name: "lengths on a bool", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) {
				e.Fields = map[string]fieldConfig{"supports_url_encoding": {MaxLength: &maxLength}}
			}),
			want: "minLength and maxLength need a string field"},
		{name: "an unknown ID", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) { e.ID.Field = "uuid" }), want: "the object has no field uuid"},
		{name: "an ID that is not a string", service: "kv.NamespaceService",
			entry: with(func(e *kindConfig) { e.ID.Field = "supports_url_encoding" }), want: "not a string"},
		{name: "a header parameter", service: "r2.BucketService",
			entry: with(func(e *kindConfig) { e.ID.Field = "name" }),
			want:  "cf-r2-jurisdiction is a header parameter"},
		{name: "a zone-level service", service: "workers.RouteService", entry: with(func(e *kindConfig) {}),
			want: "scope account, but New takes no account_id"},
		{name: "a field only an update sets", svc: updateOnly, entry: with(func(e *kindConfig) {}),
			want: "color can be updated but not set at creation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.svc
			if svc == nil {
				svc = loadTestService(t, tc.service)
			}
			_, err := buildKind(tc.entry, svc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildKind: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildKindLeavesEnumsOffStatus(t *testing.T) {
	str := types.Typ[types.String]
	svc := &service{
		create: []sdkField{{apiName: "account_id", in: inPath, typ: str, required: true}},
		object: []sdkField{
			{goName: "ID", apiName: "id", in: inBody, typ: str, required: true},
			{goName: "Status", apiName: "status", in: inBody, typ: str, enum: []string{"active", "pending"}},
		},
	}
	k, err := buildKind(kvEntry(), svc)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if status := fieldNamed(t, k.metadata, "Status"); len(status.markers) != 0 {
		t.Errorf("a status field carries no enum, so a value Cloudflare adds cannot fail a status update: %+v", status)
	}
}
