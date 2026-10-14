/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"errors"
	"fmt"
	"go/types"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// testServices caches what the tests load: loading a service type-checks its
// package.
var (
	testServicesMu sync.Mutex
	testServices   = map[string]*service{}
)

// loadTestService loads a cloudflare-go v7 service, at the version go.mod
// pins.
func loadTestService(t *testing.T, name string) *service {
	t.Helper()
	testServicesMu.Lock()
	defer testServicesMu.Unlock()
	if svc, ok := testServices[name]; ok {
		return svc
	}
	svc, err := sdk{dir: "."}.service(name)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	testServices[name] = svc
	return svc
}

// summary describes fields compactly, one string each.
func summary(fields []sdkField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		s := fmt.Sprintf("%s %s %s", f.in, f.apiName, types.TypeString(f.typ, (*types.Package).Name))
		if f.required {
			s += " required"
		}
		if len(f.enum) > 0 {
			s += " enum=" + strings.Join(f.enum, ",")
		}
		out = append(out, s)
	}
	return out
}

func TestServiceReadsTheKVNamespaceService(t *testing.T) {
	svc := loadTestService(t, "kv.NamespaceService")
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	check("create", summary(svc.create), []string{
		"path account_id string required",
		"body title string required",
		"body jurisdiction kv.NamespaceNewParamsJurisdiction enum=eu,fedramp,us",
	})
	check("update", summary(svc.update), []string{"path account_id string required", "body title string required"})
	check("object", summary(svc.object), []string{
		"body id string required",
		"body title string required",
		"body jurisdiction kv.NamespaceJurisdiction enum=eu,fedramp,us",
		"body supports_url_encoding bool",
	})
	if svc.create[1].doc != "Human-readable string name for a Workers KV namespace." {
		t.Errorf("title's doc = %q", svc.create[1].doc)
	}
}

func TestServiceTakesEditWithoutUpdate(t *testing.T) {
	svc := loadTestService(t, "r2.BucketService")
	if got, want := summary(svc.update), []string{
		"path account_id string required",
		"header cf-r2-storage-class r2.BucketEditParamsCfR2StorageClass required enum=Standard,InfrequentAccess",
		"header cf-r2-jurisdiction r2.BucketEditParamsCfR2Jurisdiction enum=default,eu,us,fedramp,fedramp-high",
	}; !slices.Equal(got, want) {
		t.Errorf("update = %q, want %q", got, want)
	}
}

func TestServiceRefuses(t *testing.T) {
	for name, want := range map[string]string{
		"kv":                    "not <package>.<type>",
		"kv.":                   "not <package>.<type>",
		"kv.NoSuchService":      "has no type NoSuchService",
		"nosuchpackage.Service": "loading " + sdkModule + "/nosuchpackage",
		"dns.RecordService":     "kflare-gen cannot map unions yet",
	} {
		_, err := sdk{dir: "."}.service(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("service(%q): err = %v, want one containing %q", name, err, want)
		}
	}
}

func TestCallRefusesWhatIsNotACall(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
	}, sdkModule+"/kv")
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("loading kv: %v %v", err, pkgs)
	}
	r := newSDKReader(pkgs[0])
	typ := pkgs[0].Types.Scope().Lookup("NamespaceService").(*types.TypeName)
	methods := types.NewMethodSet(types.NewPointer(typ.Type()))

	if _, _, err := r.call(methods, "Rename"); !errors.Is(err, errNoMethod) {
		t.Errorf("call(Rename): err = %v, want errNoMethod", err)
	}
	if _, _, err := r.call(methods, "ListAutoPaging"); err == nil || !strings.Contains(err.Error(), "is not shaped") {
		t.Errorf("call(ListAutoPaging), which returns one value: err = %v", err)
	}
	if _, _, err := r.call(methods, "List"); err == nil || !strings.Contains(err.Error(), "is not a type of") {
		t.Errorf("call(List), which returns a pagination type: err = %v", err)
	}
	if _, err := r.fields(types.Typ[types.String], false); err == nil {
		t.Error("fields(string): err = nil")
	}
	if docs := r.fieldDocs("NoSuchType"); len(docs) != 0 {
		t.Errorf("fieldDocs(NoSuchType) = %v", docs)
	}
}
