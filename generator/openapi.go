/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// apiSpec is the part of Cloudflare's OpenAPI spec kflare-gen reads: the
// committed subset that sync-spec writes. The SDK decides which fields a kind
// has; the spec only adds what the SDK's types leave out: lengths, patterns,
// ranges, defaults, integers, and Stainless's hints.
type apiSpec struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas       map[string]*schema      `json:"schemas"`
		RequestBodies map[string]*requestBody `json:"requestBodies"`
		Responses     map[string]*response    `json:"responses"`
	} `json:"components"`

	// byRoute indexes the paths by their normalized route.
	byRoute map[string]string
}

// operation is one method of a path.
type operation struct {
	RequestBody *requestBody         `json:"requestBody"`
	Responses   map[string]*response `json:"responses"`
}

type requestBody struct {
	Ref     string               `json:"$ref"`
	Content map[string]mediaType `json:"content"`
}

type response struct {
	Ref     string               `json:"$ref"`
	Content map[string]mediaType `json:"content"`
}

type mediaType struct {
	Schema *schema `json:"schema"`
}

// schema is a JSON schema, as far as kflare-gen reads one.
type schema struct {
	Ref              string             `json:"$ref"`
	Type             string             `json:"type"`
	Properties       map[string]*schema `json:"properties"`
	AllOf            []*schema          `json:"allOf"`
	Enum             []any              `json:"enum"`
	Default          any                `json:"default"`
	MinLength        *int64             `json:"minLength"`
	MaxLength        *int64             `json:"maxLength"`
	Pattern          string             `json:"pattern"`
	Minimum          *float64           `json:"minimum"`
	Maximum          *float64           `json:"maximum"`
	ExclusiveMinimum bool               `json:"exclusiveMinimum"`
	ExclusiveMaximum bool               `json:"exclusiveMaximum"`
	// Sensitive is Stainless's mark for a secret, such as a password.
	Sensitive bool `json:"x-sensitive"`
}

// loadSpec reads the spec at path.
func loadSpec(path string) (*apiSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	spec, err := parseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// parseSpec reads a spec.
func parseSpec(data []byte) (*apiSpec, error) {
	spec := &apiSpec{}
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, err
	}
	spec.byRoute = map[string]string{}
	for path := range spec.Paths {
		spec.byRoute[normalizeRoute(path)] = path
	}
	return spec, nil
}

// pathParameter matches a path parameter, such as {account_id}.
var pathParameter = regexp.MustCompile(`\{[^}]*\}`)

// normalizeRoute drops the names of a route's path parameters, which the SDK's
// api.md and the spec do not always spell alike.
func normalizeRoute(path string) string {
	return pathParameter.ReplaceAllString(path, "{}")
}

// operation returns the operation for an SDK call's route.
func (s *apiSpec) operation(r route) (*operation, error) {
	path, ok := s.byRoute[normalizeRoute(r.path)]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no %s %s: run make sync-spec", r.verb, r.path)
	}
	raw, ok := s.Paths[path][r.verb]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no %s %s: run make sync-spec", r.verb, r.path)
	}
	op := &operation{}
	if err := json.Unmarshal(raw, op); err != nil {
		return nil, fmt.Errorf("%s %s: %w", r.verb, r.path, err)
	}
	return op, nil
}

// requestProperties returns the properties of an operation's JSON request
// body.
func (s *apiSpec) requestProperties(op *operation) (map[string]*schema, error) {
	body := op.RequestBody
	if body != nil && body.Ref != "" {
		var err error
		if body, err = component(s.Components.RequestBodies, body.Ref, "requestBodies"); err != nil {
			return nil, err
		}
	}
	if body == nil {
		return nil, nil
	}
	media, ok := body.Content["application/json"]
	if !ok {
		return nil, nil
	}
	return s.properties(media.Schema)
}

// resultProperties returns the properties of the result an operation's
// successful JSON response wraps.
func (s *apiSpec) resultProperties(op *operation) (map[string]*schema, error) {
	resp, ok := op.Responses["200"]
	if !ok {
		resp, ok = op.Responses["201"]
	}
	if !ok {
		return nil, fmt.Errorf("no 200 or 201 response")
	}
	if resp.Ref != "" {
		var err error
		if resp, err = component(s.Components.Responses, resp.Ref, "responses"); err != nil {
			return nil, err
		}
	}
	media, ok := resp.Content["application/json"]
	if !ok {
		return nil, fmt.Errorf("no JSON response")
	}
	envelope, err := s.properties(media.Schema)
	if err != nil {
		return nil, err
	}
	result, ok := envelope["result"]
	if !ok {
		return nil, fmt.Errorf("the response has no result")
	}
	return s.properties(result)
}

// properties returns a schema's properties, including those of the schemas it
// combines with allOf. When two of them declare the same property, the one
// with properties of its own wins: Cloudflare's response envelopes declare a
// generic result next to the specific one.
func (s *apiSpec) properties(sch *schema) (map[string]*schema, error) {
	sch, err := s.resolve(sch)
	if err != nil || sch == nil {
		return nil, err
	}
	props := map[string]*schema{}
	merge := func(name string, p *schema) error {
		existing, ok := props[name]
		if !ok {
			props[name] = p
			return nil
		}
		existingProps, err := s.properties(existing)
		if err != nil {
			return err
		}
		newProps, err := s.properties(p)
		if err != nil {
			return err
		}
		if len(newProps) > len(existingProps) {
			props[name] = p
		}
		return nil
	}
	for _, part := range sch.AllOf {
		partProps, err := s.properties(part)
		if err != nil {
			return nil, err
		}
		for name, p := range partProps {
			if err := merge(name, p); err != nil {
				return nil, err
			}
		}
	}
	for name, p := range sch.Properties {
		if err := merge(name, p); err != nil {
			return nil, err
		}
	}
	return props, nil
}

// resolve follows a schema's $ref.
func (s *apiSpec) resolve(sch *schema) (*schema, error) {
	for sch != nil && sch.Ref != "" {
		next, err := component(s.Components.Schemas, sch.Ref, "schemas")
		if err != nil {
			return nil, err
		}
		sch = next
	}
	return sch, nil
}

// component returns the component a $ref names in one of the spec's
// component maps.
func component[T any](components map[string]*T, ref, kind string) (*T, error) {
	name, ok := strings.CutPrefix(ref, "#/components/"+kind+"/")
	if !ok {
		return nil, fmt.Errorf("$ref %s is not a %s component", ref, kind)
	}
	c, ok := components[name]
	if !ok {
		return nil, fmt.Errorf("$ref %s is not in the OpenAPI subset: run make sync-spec", ref)
	}
	return c, nil
}

// fieldSpec is what the spec says about one field: its JSON type, its
// validation and its default, Stainless's hints included.
type fieldSpec struct {
	typ        string
	validation validation
	enum       []string
	// def is the default as a marker value, "" when there is none.
	def       string
	sensitive bool
}

// fieldSpec reads a property's schema, following $refs and allOf. A keyword
// the property sets itself wins over one it combines in.
func (s *apiSpec) fieldSpec(prop *schema) (fieldSpec, error) {
	var out fieldSpec
	var visit func(*schema) error
	visit = func(sch *schema) error {
		sch, err := s.resolve(sch)
		if err != nil || sch == nil {
			return err
		}
		if out.typ == "" {
			out.typ = sch.Type
		}
		v := &out.validation
		if v.minLength == nil {
			v.minLength = sch.MinLength
		}
		if v.maxLength == nil {
			v.maxLength = sch.MaxLength
		}
		if v.pattern == "" {
			v.pattern = sch.Pattern
		}
		if v.minimum == nil && sch.Minimum != nil {
			v.minimum, v.exclusiveMinimum = sch.Minimum, sch.ExclusiveMinimum
		}
		if v.maximum == nil && sch.Maximum != nil {
			v.maximum, v.exclusiveMaximum = sch.Maximum, sch.ExclusiveMaximum
		}
		if out.enum == nil && len(sch.Enum) > 0 {
			for _, value := range sch.Enum {
				str, ok := value.(string)
				if !ok {
					return fmt.Errorf("enum value %v is not a string", value)
				}
				out.enum = append(out.enum, str)
			}
		}
		if out.def == "" && sch.Default != nil {
			literal, err := defaultLiteral(sch.Default)
			if err != nil {
				return err
			}
			out.def = literal
		}
		out.sensitive = out.sensitive || sch.Sensitive
		for _, part := range sch.AllOf {
			if err := visit(part); err != nil {
				return err
			}
		}
		return nil
	}
	return out, visit(prop)
}

// defaultLiteral writes a default as the value of a +kubebuilder:default
// marker. Strings are always quoted, so that one such as "true" stays a
// string.
func defaultLiteral(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf("%q", v), nil
	case bool:
		return fmt.Sprintf("%t", v), nil
	case float64:
		if v != float64(int64(v)) {
			return "", fmt.Errorf("default %v is not an integer", v)
		}
		return fmt.Sprintf("%d", int64(v)), nil
	}
	return "", fmt.Errorf("default %v is a %T, which kflare-gen cannot map yet", value, value)
}
