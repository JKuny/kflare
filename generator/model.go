/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"slices"
	"strings"
)

// kind is the model of a generated kind: what its types file declares.
type kind struct {
	name, shortName, noun string
	// doc is the kind's documentation, one entry per line.
	doc []string
	// claimLabel is the value of the kind's claim label constant.
	claimLabel string
	// specRules are CEL rules on the spec as a whole.
	specRules []string
	// spec holds the spec's fields, accountRef first; metadata those of
	// status.cloudflareMetadata, the ID first.
	spec, metadata []field
	printColumns   []printColumn
}

// field is one field of a generated struct.
type field struct {
	goName, jsonName, goType string
	doc                      []string
	markers                  []string
	optional                 bool
}

// printColumn is one of the kind's additional printer columns.
type printColumn struct {
	name, typ, jsonPath string
}

// typeString is a string, both as a Go type and as a print column's type.
const typeString = "string"

// immutable returns the CEL rule that keeps a field as it was created.
func immutable(jsonName string) string {
	return fmt.Sprintf(`+kubebuilder:validation:XValidation:rule="self == oldSelf",message="%s is immutable"`, jsonName)
}

// buildKind builds the model of entry's kind from its service. Every field of
// the service's create params and of the object it returns is placed or
// ignored; an entry that names a field the service does not have is refused,
// so that a misspelled name or a field an SDK bump removed shows up.
func buildKind(entry kindConfig, svc *service) (*kind, error) {
	b := &kindBuilder{entry: entry, svc: svc, used: map[string]bool{}}
	k, err := b.build()
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Service, err)
	}
	return k, nil
}

type kindBuilder struct {
	entry kindConfig
	svc   *service
	// used records the entry's field and ignore names that matched a field.
	used map[string]bool
}

func (b *kindBuilder) build() (*kind, error) {
	e := b.entry
	k := &kind{name: e.Kind, shortName: e.ShortName, noun: e.Noun, doc: lines(e.Doc), claimLabel: e.ClaimLabel}

	if !slices.ContainsFunc(b.svc.create, isAccountID) {
		return nil, fmt.Errorf("scope account, but New takes no account_id")
	}
	k.spec = append(k.spec, field{
		goName: "AccountRef", jsonName: "accountRef", goType: "corev1.LocalObjectReference",
		doc: []string{
			fmt.Sprintf("AccountRef references the CloudflareAccount that owns the %s.", e.Noun),
			"Immutable after creation.",
		},
		markers: []string{immutable("accountRef")},
	})

	inCreate := map[string]bool{}
	var specColumns []printColumn
	for _, f := range b.svc.create {
		inCreate[f.apiName] = true
		if isAccountID(f) {
			continue
		}
		if b.ignored(f.apiName) {
			if f.required {
				return nil, fmt.Errorf("%s is required to create the object, so it cannot be ignored", f.apiName)
			}
			continue
		}
		if f.in != inBody {
			return nil, fmt.Errorf("%s is a %s parameter, which kflare-gen cannot map yet; ignore it with a reason",
				f.apiName, f.in)
		}
		out, column, err := b.field(f, true)
		if err != nil {
			return nil, err
		}
		if !b.updatable(f.apiName) {
			out.markers = append(out.markers, immutable(out.jsonName))
			if out.optional {
				// self == oldSelf holds only while the field is set: it must
				// not be added or removed either.
				k.specRules = append(k.specRules, fmt.Sprintf(
					`+kubebuilder:validation:XValidation:rule="has(self.%[1]s) == has(oldSelf.%[1]s)",message="%[1]s is immutable"`,
					out.jsonName))
			}
		}
		k.spec = append(k.spec, out)
		specColumns = append(specColumns, column...)
	}
	for _, f := range b.svc.update {
		if f.in == inBody && !inCreate[f.apiName] && !b.ignored(f.apiName) {
			return nil, fmt.Errorf("%s can be updated but not set at creation, which kflare-gen cannot map yet; "+
				"ignore it with a reason", f.apiName)
		}
	}

	var statusColumns []printColumn
	id, ok := b.objectField(e.ID.Field)
	if !ok {
		return nil, fmt.Errorf("id.field: the object has no field %s", e.ID.Field)
	}
	if !isString(id.typ) {
		return nil, fmt.Errorf("id.field: %s is a %s, not a string", e.ID.Field, id.typ)
	}
	idDoc := e.ID.Doc
	if idDoc == "" {
		idDoc = id.doc
	}
	k.metadata = append(k.metadata, field{
		goName: upperCamel(e.ID.Status), jsonName: e.ID.Status, goType: typeString, doc: lines(idDoc), optional: true,
	})
	idColumn := e.ID.PrintColumn
	if idColumn == "" {
		idColumn = humanize(e.ID.Status)
	}
	statusColumns = append(statusColumns, printColumn{name: idColumn, typ: typeString,
		jsonPath: ".status.cloudflareMetadata." + e.ID.Status})
	for _, f := range b.svc.object {
		// The object's other fields that the spec sets are the spec's.
		if f.apiName == e.ID.Field || inCreate[f.apiName] || b.ignored(f.apiName) {
			continue
		}
		out, column, err := b.field(f, false)
		if err != nil {
			return nil, err
		}
		k.metadata = append(k.metadata, out)
		statusColumns = append(statusColumns, column...)
	}

	k.printColumns = append(append(specColumns, statusColumns...),
		printColumn{name: "Ready", typ: typeString, jsonPath: `.status.conditions[?(@.type=="Ready")].status`},
		printColumn{name: "Age", typ: "date", jsonPath: ".metadata.creationTimestamp"})

	for name := range b.entry.Ignore {
		if !b.used[name] {
			return nil, fmt.Errorf("ignore.%s: the service has no such field", name)
		}
	}
	for name := range b.entry.Fields {
		if !b.used[name] {
			return nil, fmt.Errorf("fields.%s: the service has no such field", name)
		}
	}
	return k, nil
}

// ignored reports whether the entry ignores the field apiName.
func (b *kindBuilder) ignored(apiName string) bool {
	if _, ok := b.entry.Ignore[apiName]; ok {
		b.used[apiName] = true
		return true
	}
	return false
}

// updatable reports whether the service's update takes the field apiName.
func (b *kindBuilder) updatable(apiName string) bool {
	return slices.ContainsFunc(b.svc.update, func(f sdkField) bool { return f.in == inBody && f.apiName == apiName })
}

// objectField returns the object's field apiName.
func (b *kindBuilder) objectField(apiName string) (sdkField, bool) {
	i := slices.IndexFunc(b.svc.object, func(f sdkField) bool { return f.apiName == apiName })
	if i < 0 {
		return sdkField{}, false
	}
	return b.svc.object[i], true
}

// field maps an SDK field to a field of the spec, or of
// status.cloudflareMetadata, applying the entry's adjustments, and returns the
// print column it asks for, if any.
//
// An optional spec field that is not a string is a pointer, so that unset
// differs from false. Status fields are all optional and never pointers, and
// carry no enum: a value Cloudflare adds to an enum must not make kflare's
// status updates fail.
func (b *kindBuilder) field(f sdkField, spec bool) (field, []printColumn, error) {
	adjust, adjusted := b.entry.Fields[f.apiName]
	if adjusted {
		b.used[f.apiName] = true
	}
	goType, column, err := goTypeOf(f)
	if err != nil {
		return field{}, nil, err
	}
	out := field{goName: f.goName, jsonName: lowerCamel(f.goName), goType: goType, optional: !spec || !f.required}
	parent := ".status.cloudflareMetadata."
	if spec {
		parent = ".spec."
		if out.optional && goType != typeString {
			out.goType = "*" + goType
		}
		if len(f.enum) > 0 {
			out.markers = append(out.markers, "+kubebuilder:validation:Enum="+strings.Join(f.enum, ";"))
		}
	}
	doc := adjust.Doc
	if doc == "" {
		doc = f.doc
	}
	out.doc = lines(doc)
	if adjust.MinLength != nil || adjust.MaxLength != nil {
		if goType != typeString {
			return field{}, nil, fmt.Errorf("fields.%s: minLength and maxLength need a string field", f.apiName)
		}
		if adjust.MinLength != nil {
			out.markers = append(out.markers, fmt.Sprintf("+kubebuilder:validation:MinLength=%d", *adjust.MinLength))
		}
		if adjust.MaxLength != nil {
			out.markers = append(out.markers, fmt.Sprintf("+kubebuilder:validation:MaxLength=%d", *adjust.MaxLength))
		}
	}
	var columns []printColumn
	if adjust.PrintColumn != "" {
		columns = append(columns, printColumn{name: adjust.PrintColumn, typ: column, jsonPath: parent + out.jsonName})
	}
	return out, columns, nil
}

// goTypeOf maps an SDK field's type to a Go type for the CRD and the type of
// its print column. Numbers are refused for now: cloudflare-go v7 types every
// number as a float64, so only the OpenAPI spec can tell an integer, which a
// CRD needs.
func goTypeOf(f sdkField) (goType, column string, err error) {
	if basic, ok := f.typ.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.String:
			return typeString, typeString, nil
		case types.Bool:
			return "bool", "boolean", nil
		}
	}
	return "", "", fmt.Errorf("%s is a %s, which kflare-gen cannot map yet; ignore it with a reason", f.apiName, f.typ)
}

// isAccountID reports whether f is the account_id a call's path takes.
func isAccountID(f sdkField) bool {
	return f.in == inPath && f.apiName == "account_id"
}

// isString reports whether t is a string type.
func isString(t types.Type) bool {
	basic, ok := t.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String
}

// lines splits documentation into lines, without trailing blank ones.
func lines(doc string) []string {
	doc = strings.TrimRight(doc, " \n")
	if doc == "" {
		return nil
	}
	out := strings.Split(doc, "\n")
	for i, line := range out {
		out[i] = strings.TrimRight(line, " ")
	}
	return out
}
