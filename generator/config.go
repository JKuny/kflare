/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// config is generator/generator.yaml: one entry per kind kflare-gen generates.
type config struct {
	Kinds []kindConfig `json:"kinds"`
}

// kindConfig is one kind's entry. Every field of the kind's cloudflare-go v7
// service becomes a field of the kind (fields are opt-out), except those the
// entry ignores.
type kindConfig struct {
	// Kind is the API type's name, such as KVNamespace.
	Kind string `json:"kind"`
	// Service is the cloudflare-go v7 service the kind calls, as
	// <package>.<type>, the package's path being relative to the module: the
	// KV namespaces service is kv.NamespaceService.
	Service string `json:"service"`
	// Scope is what a resource of the kind names its parent with. The only
	// scope so far is account: an accountRef, for the account_id the
	// service's calls take.
	Scope string `json:"scope"`
	// ShortName is the kind's short name for kubectl, such as cfkv.
	ShortName string `json:"shortName"`
	// Noun is what docs and messages call the Cloudflare object, such as "KV
	// namespace".
	Noun string `json:"noun"`
	// Doc is the kind's documentation, from the type's name on.
	Doc string `json:"doc"`
	// ID names the object's ID and where status records it.
	ID idConfig `json:"id"`
	// ClaimLabel is the label holding the ID of the object a resource
	// manages.
	ClaimLabel string `json:"claimLabel"`
	// Fields adjusts fields, by their API name.
	Fields map[string]fieldConfig `json:"fields,omitempty"`
	// Fixed leaves out of the spec fields that kflare always sends with the
	// same value, by their API name: an IPList's list kind is always ip.
	Fixed map[string]string `json:"fixed,omitempty"`
	// Ignore leaves out fields, by their API name, each with the reason.
	Ignore map[string]string `json:"ignore,omitempty"`
}

// idConfig names an object's ID.
type idConfig struct {
	// Field is the ID's API name in the object, such as id.
	Field string `json:"field"`
	// Status is the ID's name in status.cloudflareMetadata, such as
	// namespaceID.
	Status string `json:"status"`
	// Doc replaces the SDK's documentation of the ID.
	Doc string `json:"doc,omitempty"`
	// PrintColumn names the ID's column; by default, the status name in
	// words ("Namespace ID").
	PrintColumn string `json:"printColumn,omitempty"`
}

// fieldConfig adjusts one field.
type fieldConfig struct {
	// Doc replaces the SDK's documentation of the field.
	Doc string `json:"doc,omitempty"`
	// PrintColumn adds a column for the field, with this name.
	PrintColumn string `json:"printColumn,omitempty"`
	// Validation replaces what the OpenAPI spec says the field's value may
	// be. The spec is not always the live API.
	Validation *validationConfig `json:"validation,omitempty"`
}

// validationConfig replaces a field's validation, with the reason.
type validationConfig struct {
	// MinLength, MaxLength and Pattern bound a string field.
	MinLength *int64 `json:"minLength,omitempty"`
	MaxLength *int64 `json:"maxLength,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	// Minimum and Maximum bound an integer field.
	Minimum *int64 `json:"minimum,omitempty"`
	Maximum *int64 `json:"maximum,omitempty"`
	// Reason says why the spec's validation does not do, such as what a live
	// check found.
	Reason string `json:"reason"`
}

// validation returns the validation v replaces a field's with.
func (v *validationConfig) validation() validation {
	out := validation{minLength: v.MinLength, maxLength: v.MaxLength, pattern: v.Pattern}
	if v.Minimum != nil {
		minimum := float64(*v.Minimum)
		out.minimum = &minimum
	}
	if v.Maximum != nil {
		maximum := float64(*v.Maximum)
		out.maximum = &maximum
	}
	return out
}

var (
	kindName   = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	shortName  = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	statusName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
)

// loadConfig reads and checks the configuration at path. Unknown keys are
// refused, so that a misspelled one does not go unnoticed.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &config{}
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validate checks what can be checked without the SDK. Field names are
// checked against the SDK when the kind is built.
func (c *config) validate() error {
	if len(c.Kinds) == 0 {
		return fmt.Errorf("no kinds")
	}
	seen := map[string]bool{}
	for i, k := range c.Kinds {
		if err := k.validate(); err != nil {
			return fmt.Errorf("kinds[%d] (%s): %w", i, k.Kind, err)
		}
		if seen[k.Kind] {
			return fmt.Errorf("kinds[%d]: %s is listed twice", i, k.Kind)
		}
		seen[k.Kind] = true
	}
	return nil
}

func (k *kindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case strings.Count(k.Service, ".") == 0 || strings.HasPrefix(k.Service, ".") || strings.HasSuffix(k.Service, "."):
		return fmt.Errorf("service %q is not <package>.<type>", k.Service)
	case k.Scope != "account":
		return fmt.Errorf("scope %q is not supported: the only scope is account", k.Scope)
	case !shortName.MatchString(k.ShortName):
		return fmt.Errorf("shortName %q is not lower-case letters and digits", k.ShortName)
	case strings.TrimSpace(k.Noun) == "":
		return fmt.Errorf("noun is empty")
	case !strings.HasPrefix(k.Doc, k.Kind+" "):
		return fmt.Errorf("doc does not start with %q", k.Kind+" ")
	case k.ID.Field == "":
		return fmt.Errorf("id.field is empty")
	case !statusName.MatchString(k.ID.Status):
		return fmt.Errorf("id.status %q is not a lowerCamel name", k.ID.Status)
	case !strings.HasPrefix(k.ClaimLabel, "kflare.dev/"):
		return fmt.Errorf("claimLabel %q does not start with kflare.dev/", k.ClaimLabel)
	}
	for name, reason := range k.Ignore {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("ignore.%s gives no reason", name)
		}
		if _, ok := k.Fields[name]; ok {
			return fmt.Errorf("%s is both ignored and adjusted in fields", name)
		}
		if _, ok := k.Fixed[name]; ok {
			return fmt.Errorf("%s is both ignored and fixed", name)
		}
	}
	for name := range k.Fixed {
		if _, ok := k.Fields[name]; ok {
			return fmt.Errorf("%s is both fixed and adjusted in fields", name)
		}
	}
	for name, f := range k.Fields {
		if f.Validation != nil {
			if err := f.Validation.validate(); err != nil {
				return fmt.Errorf("fields.%s.validation: %w", name, err)
			}
		}
	}
	return nil
}

func (v *validationConfig) validate() error {
	switch {
	case strings.TrimSpace(v.Reason) == "":
		return fmt.Errorf("gives no reason")
	case v.MinLength != nil && v.MaxLength != nil && *v.MinLength > *v.MaxLength:
		return fmt.Errorf("minLength is above maxLength")
	case v.Minimum != nil && v.Maximum != nil && *v.Minimum > *v.Maximum:
		return fmt.Errorf("minimum is above maximum")
	case strings.Contains(v.Pattern, "`"):
		return fmt.Errorf("the pattern holds a backquote, which a kubebuilder marker cannot")
	}
	if _, err := regexp.Compile(v.Pattern); err != nil {
		return fmt.Errorf("the pattern does not compile: %w", err)
	}
	return nil
}
