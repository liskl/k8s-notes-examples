package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
)

const input = `{"apiVersion":"appdbschemas.fn.k8s-notes.example/v1beta1","kind":"Input"}`

func xr(schemas string) *fnv1.State {
	return &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(`{
		"apiVersion": "platform.k8s-notes.example/v1alpha1",
		"kind": "AppDatabase",
		"metadata": {"name": "web", "namespace": "team-a"},
		"spec": {"schemas": ` + schemas + `}
	}`)}}
}

func run(t *testing.T, req *fnv1.RunFunctionRequest) *fnv1.RunFunctionResponse {
	t.Helper()
	f := &Function{log: logging.NewNopLogger()}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("RunFunction returned a Go error: %v", err)
	}
	return rsp
}

func fatal(rsp *fnv1.RunFunctionResponse) string {
	for _, r := range rsp.GetResults() {
		if r.GetSeverity() == fnv1.Severity_SEVERITY_FATAL {
			return r.GetMessage()
		}
	}
	return ""
}

func keys(rsp *fnv1.RunFunctionResponse) []string {
	var k []string
	for name := range rsp.GetDesired().GetResources() {
		k = append(k, name)
	}
	return k
}

func TestOneSchema(t *testing.T) {
	rsp := run(t, &fnv1.RunFunctionRequest{
		Input:    resource.MustStructJSON(input),
		Observed: xr(`[{"name":"sales","access":"write"}]`),
	})
	if m := fatal(rsp); m != "" {
		t.Fatalf("unexpected fatal: %s", m)
	}
	want := &fnv1.Resource{
		Resource: resource.MustStructJSON(`{
			"apiVersion": "postgresql.sql.m.crossplane.io/v1alpha1",
			"kind": "Schema",
			"metadata": {"name": "web-sales", "annotations": {"crossplane.io/external-name": "sales"}},
			"spec": {
				"providerConfigRef": {"kind": "ProviderConfig", "name": "web"},
				"forProvider": {"database": "web"}
			}
		}`),
		Ready: fnv1.Ready_READY_FALSE,
	}
	if diff := cmp.Diff(want, rsp.GetDesired().GetResources()["schema-sales"], protocmp.Transform()); diff != "" {
		t.Errorf("schema-sales: -want, +got:\n%s", diff)
	}
	g := rsp.GetDesired().GetResources()["grant-sales"].GetResource().AsMap()
	privs := g["spec"].(map[string]any)["forProvider"].(map[string]any)["privileges"]
	if diff := cmp.Diff([]any{"USAGE", "CREATE"}, privs); diff != "" {
		t.Errorf("grant-sales privileges: -want, +got:\n%s", diff)
	}
	if len(rsp.GetDesired().GetResources()) != 3 {
		t.Errorf("want 3 resources, got %v", keys(rsp))
	}
	st := rsp.GetDesired().GetComposite().GetResource().AsMap()["status"]
	if diff := cmp.Diff(map[string]any{"schemas": []any{map[string]any{"name": "sales", "ready": false}}}, st); diff != "" {
		t.Errorf("status: -want, +got:\n%s", diff)
	}
}

func TestTwoSchemasKeepOtherStepsResources(t *testing.T) {
	desired := xr(`[]`)
	desired.Resources = map[string]*fnv1.Resource{
		"cluster": {Resource: resource.MustStructJSON(`{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster"}`)},
	}
	rsp := run(t, &fnv1.RunFunctionRequest{
		Input:    resource.MustStructJSON(input),
		Observed: xr(`[{"name":"sales"},{"name":"audit_log"}]`),
		Desired:  desired,
	})
	if len(rsp.GetDesired().GetResources()) != 7 {
		t.Fatalf("want 7 resources (1 kept + 6 new), got %v", keys(rsp))
	}
	if _, ok := rsp.GetDesired().GetResources()["cluster"]; !ok {
		t.Errorf("the earlier step's cluster was dropped")
	}
	n := rsp.GetDesired().GetResources()["schema-audit_log"].GetResource().AsMap()["metadata"].(map[string]any)["name"]
	if n != "web-audit-log" {
		t.Errorf("metadata.name: want web-audit-log, got %v", n)
	}
}

func TestObservedReady(t *testing.T) {
	obs := xr(`[{"name":"sales"}]`)
	obs.Resources = map[string]*fnv1.Resource{
		"schema-sales":  {Resource: resource.MustStructJSON(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)},
		"grant-sales":   {Resource: resource.MustStructJSON(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)},
		"defpriv-sales": {Resource: resource.MustStructJSON(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)},
	}
	rsp := run(t, &fnv1.RunFunctionRequest{Input: resource.MustStructJSON(input), Observed: obs})
	for _, k := range []string{"schema-sales", "grant-sales", "defpriv-sales"} {
		if got := rsp.GetDesired().GetResources()[k].GetReady(); got != fnv1.Ready_READY_TRUE {
			t.Errorf("%s: want READY_TRUE, got %v", k, got)
		}
	}
	st := rsp.GetDesired().GetComposite().GetResource().AsMap()["status"].(map[string]any)["schemas"]
	if diff := cmp.Diff([]any{map[string]any{"name": "sales", "ready": true}}, st); diff != "" {
		t.Errorf("status.schemas: -want, +got:\n%s", diff)
	}
}

func TestNoSchemas(t *testing.T) {
	obs := &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(`{
		"apiVersion":"platform.k8s-notes.example/v1alpha1","kind":"AppDatabase",
		"metadata":{"name":"web","namespace":"team-a"},"spec":{}}`)}}
	rsp := run(t, &fnv1.RunFunctionRequest{Input: resource.MustStructJSON(input), Observed: obs})
	if m := fatal(rsp); m != "" {
		t.Fatalf("unexpected fatal: %s", m)
	}
	if len(rsp.GetDesired().GetResources()) != 0 {
		t.Errorf("want no resources, got %v", keys(rsp))
	}
}

func TestBadNames(t *testing.T) {
	cases := map[string]struct {
		schemas  string
		reserved []any
		want     string
	}{
		"ReservedName":  {schemas: `[{"name":"public"}]`, want: `"public" is reserved`},
		"PgPrefix":      {schemas: `[{"name":"pg_stats"}]`, want: `"pg_stats" is reserved`},
		"InputReserved": {schemas: `[{"name":"admin"}]`, reserved: []any{"admin"}, want: `"admin" is reserved`},
		"Uppercase":     {schemas: `[{"name":"Sales"}]`, want: `"Sales" must match`},
		"Dollar":        {schemas: `[{"name":"a$b"}]`, want: `"a$b" must match`},
		"TooLong":       {schemas: `[{"name":"` + strings.Repeat("a", 64) + `"}]`, want: `is 64 bytes; PostgreSQL would cut it to 63`},
		"Duplicate":     {schemas: `[{"name":"sales"},{"name":"sales"}]`, want: `"sales" is listed twice`},
		"BadAccess":     {schemas: `[{"name":"sales","access":"admin"}]`, want: `access "admin" must be read or write`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in, _ := structpb.NewStruct(map[string]any{
				"apiVersion": "appdbschemas.fn.k8s-notes.example/v1beta1", "kind": "Input", "reserved": tc.reserved,
			})
			if tc.reserved == nil {
				in = resource.MustStructJSON(input)
			}
			rsp := run(t, &fnv1.RunFunctionRequest{Input: in, Observed: xr(tc.schemas)})
			if m := fatal(rsp); !strings.Contains(m, tc.want) {
				t.Errorf("fatal: want it to contain %q, got %q", tc.want, m)
			}
			if len(rsp.GetConditions()) != 1 || rsp.GetConditions()[0].GetType() != "SchemasValid" {
				t.Errorf("want one SchemasValid condition, got %v", rsp.GetConditions())
			}
		})
	}
}

func TestBareResponseDropsDesired(t *testing.T) {
	desired := xr(`[]`)
	desired.Resources = map[string]*fnv1.Resource{"cluster": {Resource: resource.MustStructJSON(`{"kind":"Cluster"}`)}}
	rsp := run(t, &fnv1.RunFunctionRequest{
		Input:    resource.MustStructJSON(`{"apiVersion":"appdbschemas.fn.k8s-notes.example/v1beta1","kind":"Input","break":"bare-response"}`),
		Observed: xr(`[]`),
		Desired:  desired,
	})
	if rsp.GetDesired() != nil {
		t.Errorf("a bare response should carry no desired state, got %v", rsp.GetDesired())
	}
}
