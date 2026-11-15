package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/liskl/k8s-notes-examples/crossplane/function/input/v1beta1"
)

// The provider-sql kinds this function composes (namespaced group).
const sqlAPIVersion = "postgresql.sql.m.crossplane.io/v1alpha1"

// A schema name PostgreSQL keeps as written, and that also fits a Kubernetes
// name once "_" becomes "-". PostgreSQL itself allows "$" too, but "$" can
// never appear in a metadata.name, so it is refused here.
var validName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// PostgreSQL cuts identifiers longer than this many bytes without a word.
const maxNameBytes = 63

// Schema is one entry of the XR's spec.schemas.
type Schema struct {
	Name   string `json:"name"`
	Access string `json:"access,omitempty"` // read (default) or write
}

// SchemaStatus is one entry of the XR's status.schemas.
type SchemaStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// Function fans an XR's spec.schemas out into provider-sql resources.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Debug("Running function", "tag", req.GetMeta().GetTag())

	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		rsp := response.To(req, response.DefaultTTL)
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	// Two ways to break it on purpose, for the book's break pages.
	switch in.Break {
	case "panic":
		panic("function-appdb-schemas: break: panic was asked for")
	case "bare-response":
		// No response.To: the desired state of earlier steps is not copied
		// forward, so core sees an empty desired state.
		return &fnv1.RunFunctionResponse{Meta: &fnv1.ResponseMeta{Tag: req.GetMeta().GetTag()}}, nil
	}

	// Copies the desired state and the context of earlier steps forward.
	rsp := response.To(req, response.DefaultTTL)

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}
	xrName := oxr.Resource.GetName()

	var schemas []Schema
	if err := oxr.Resource.GetValueInto("spec.schemas", &schemas); err != nil {
		// No spec.schemas: nothing to add. Earlier steps' resources stay.
		f.log.Debug("No spec.schemas", "xr", xrName)
		schemas = nil
	}

	if err := checkNames(schemas, in.Reserved); err != nil {
		response.Fatal(rsp, err)
		response.ConditionFalse(rsp, "SchemasValid", "InvalidSchemaName").
			WithMessage(err.Error()).
			TargetComposite()
		return rsp, nil
	}

	pc := in.ProviderConfig
	if pc == "" {
		pc = xrName
	}
	db := xrName
	role := xrName + "-app"

	desired, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composed resources"))
		return rsp, nil
	}
	observed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	status := make([]any, 0, len(schemas))
	for _, s := range schemas {
		k8sName := xrName + "-" + strings.ReplaceAll(s.Name, "_", "-")
		privs := []any{"USAGE"}
		tablePrivs := []any{"SELECT"}
		if s.Access == "write" {
			privs = append(privs, "CREATE")
			tablePrivs = append(tablePrivs, "INSERT", "UPDATE", "DELETE")
		}

		parts := map[resource.Name]map[string]any{
			resource.Name("schema-" + s.Name): {
				"kind": "Schema",
				"metadata": map[string]any{
					"name":        k8sName,
					"annotations": map[string]any{"crossplane.io/external-name": s.Name},
				},
				// No owner named: the schema belongs to the role the provider logs in as.
				// The app's role gets in through the Grant below, and only as far as
				// its access says.
				"forProvider": map[string]any{"database": db},
			},
			resource.Name("grant-" + s.Name): {
				"kind":     "Grant",
				"metadata": map[string]any{"name": k8sName + "-grant"},
				"forProvider": map[string]any{
					"database": db, "schema": s.Name, "role": role, "privileges": privs,
				},
			},
			resource.Name("defpriv-" + s.Name): {
				"kind":     "DefaultPrivileges",
				"metadata": map[string]any{"name": k8sName + "-tables"},
				"forProvider": map[string]any{
					// Tables postgres makes in this schema are granted to the app role.
					"database": db, "schema": s.Name, "role": role, "targetRole": "postgres",
					"objectType": "table", "privileges": tablePrivs,
				},
			},
		}

		allReady := true
		for name, p := range parts {
			cd := composed.New()
			cd.SetAPIVersion(sqlAPIVersion)
			cd.SetKind(p["kind"].(string))
			if err := cd.SetValue("metadata", p["metadata"]); err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot build %s", name))
				return rsp, nil
			}
			if err := cd.SetValue("spec.providerConfigRef", map[string]any{"kind": "ProviderConfig", "name": pc}); err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot build %s", name))
				return rsp, nil
			}
			if err := cd.SetValue("spec.forProvider", p["forProvider"]); err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot build %s", name))
				return rsp, nil
			}

			// Readiness from the resource's own Ready condition, once observed.
			ready := resource.ReadyFalse
			if o, ok := observed[name]; ok && o.Resource.GetCondition("Ready").Status == "True" {
				ready = resource.ReadyTrue
			}
			if ready != resource.ReadyTrue {
				allReady = false
			}
			desired[name] = &resource.DesiredComposed{Resource: cd, Ready: ready}
		}
		status = append(status, map[string]any{"name": s.Name, "ready": allReady})
	}

	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Write status.schemas on the XR. Only status is applied from here.
	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composite resource"))
		return rsp, nil
	}
	if err := dxr.Resource.SetValue("status.schemas", status); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set status.schemas"))
		return rsp, nil
	}
	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	response.Normalf(rsp, "composed %d schema(s)", len(schemas))
	return rsp, nil
}

// checkNames refuses a name PostgreSQL would change or reserves, and duplicates.
func checkNames(schemas []Schema, reserved []string) error {
	stop := map[string]bool{"public": true, "information_schema": true}
	for _, r := range reserved {
		stop[r] = true
	}
	seen := map[string]bool{}
	for i, s := range schemas {
		switch {
		case len(s.Name) > maxNameBytes:
			return fmt.Errorf("spec.schemas[%d]: %q is %d bytes; PostgreSQL would cut it to %d", i, s.Name, len(s.Name), maxNameBytes)
		case !validName.MatchString(s.Name):
			return fmt.Errorf("spec.schemas[%d]: %q must match %s", i, s.Name, validName)
		case strings.HasPrefix(s.Name, "pg_") || stop[s.Name]:
			return fmt.Errorf("spec.schemas[%d]: %q is reserved", i, s.Name)
		case seen[s.Name]:
			return fmt.Errorf("spec.schemas[%d]: %q is listed twice", i, s.Name)
		case s.Access != "" && s.Access != "read" && s.Access != "write":
			return fmt.Errorf("spec.schemas[%d]: access %q must be read or write", i, s.Access)
		}
		seen[s.Name] = true
	}
	return nil
}
