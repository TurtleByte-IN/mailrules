package api

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// spec is api/openapi.yaml, parsed.
func spec(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("api/openapi.yaml is not well-formed YAML: %v", err)
	}
	return doc
}

// resolve follows a local reference such as #/components/schemas/Rule.
func resolve(doc map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = doc
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

var methods = []string{"get", "post", "put", "patch", "delete"}

// The route table and the OpenAPI contract are two lists of the same thing; keep them
// equal, down to which endpoints are only a contract so far (x-status: planned = 501).
func TestRoutesMatchOpenAPI(t *testing.T) {
	doc := spec(t)
	var inSpec []string
	for path, item := range doc["paths"].(map[string]any) {
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		for method, op := range item.(map[string]any) {
			if slices.Contains(methods, method) {
				planned := op.(map[string]any)["x-status"] == "planned"
				inSpec = append(inSpec, fmt.Sprintf("%s %s planned=%v", strings.ToUpper(method), path, planned))
			}
		}
	}
	var served []string
	for _, r := range (&server{}).routes() {
		served = append(served, fmt.Sprintf("%s %s planned=%v", r.method, r.path, r.planned))
	}
	sort.Strings(inSpec)
	sort.Strings(served)
	if strings.Join(inSpec, "\n") != strings.Join(served, "\n") {
		t.Fatalf("api/openapi.yaml and the route table differ\nspec:\n%s\nserved:\n%s", strings.Join(inSpec, "\n"), strings.Join(served, "\n"))
	}
}

// Every $ref resolves, every operation has an id and says what a planned one answers, and
// no schema requires a property it does not define.
func TestOpenAPIIsSound(t *testing.T) {
	doc := spec(t)
	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	refs := 0
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				refs++
				if _, ok := resolve(doc, ref); !ok {
					t.Errorf("%s: $ref %q does not resolve", where, ref)
				}
			}
			if props, ok := v["properties"].(map[string]any); ok {
				for _, name := range list(v["required"]) {
					if _, ok := props[name]; !ok {
						t.Errorf("%s: required property %q is not defined", where, name)
					}
				}
			}
			for k, child := range v {
				walk(where+"/"+k, child)
			}
		case []any:
			for i, child := range v {
				walk(fmt.Sprintf("%s/%d", where, i), child)
			}
		}
	}
	walk("#", doc)
	if refs == 0 {
		t.Error("no $ref found: the walk is broken")
	}

	ids := map[string]string{}
	for path, item := range doc["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			if !slices.Contains(methods, method) {
				continue
			}
			o := op.(map[string]any)
			at := strings.ToUpper(method) + " " + path
			id, _ := o["operationId"].(string)
			if id == "" || ids[id] != "" {
				t.Errorf("%s: operationId %q is missing or also used by %s", at, id, ids[id])
			}
			ids[id] = at
			responses := o["responses"].(map[string]any)
			if _, has := responses["501"]; has != (o["x-status"] == "planned") {
				t.Errorf("%s: a planned operation documents 501, and only a planned one does", at)
			}
		}
	}
}

func list(v any) []string {
	var out []string
	items, _ := v.([]any)
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// conform checks a served JSON value against a schema of the contract: every required
// property is there and no property is served that the schema does not define. It keeps the
// handlers' JSON and api/openapi.yaml, written by hand in two places, from drifting apart.
func conform(t *testing.T, doc map[string]any, schema string, got any) {
	t.Helper()
	s, ok := resolve(doc, "#/components/schemas/"+schema)
	if !ok {
		t.Fatalf("no schema %s", schema)
	}
	for _, problem := range mismatches(doc, schema, s.(map[string]any), got) {
		t.Errorf("response does not match the contract: %s", problem)
	}
}

// flatten merges a schema's $ref and allOf into its properties and required list.
func flatten(doc map[string]any, s map[string]any, props map[string]any, required *[]string) {
	if ref, ok := s["$ref"].(string); ok {
		if target, ok := resolve(doc, ref); ok {
			flatten(doc, target.(map[string]any), props, required)
		}
	}
	for _, part := range anyList(s["allOf"]) {
		flatten(doc, part.(map[string]any), props, required)
	}
	for name, p := range mapOf(s["properties"]) {
		props[name] = p
	}
	*required = append(*required, list(s["required"])...)
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func mismatches(doc map[string]any, where string, s map[string]any, got any) []string {
	var out []string
	for _, alt := range anyList(s["oneOf"]) { // only used for "this or null"
		if a := alt.(map[string]any); a["type"] != "null" && got != nil {
			return mismatches(doc, where, a, got)
		}
	}
	props, required := map[string]any{}, []string{}
	flatten(doc, s, props, &required)
	target := s
	if ref, ok := s["$ref"].(string); ok {
		if r, ok := resolve(doc, ref); ok {
			target = r.(map[string]any)
		}
	}
	switch v := got.(type) {
	case map[string]any:
		if len(props) == 0 {
			return nil // a free-form object
		}
		for _, name := range required {
			if _, ok := v[name]; !ok {
				out = append(out, where+": required property "+name+" is missing")
			}
		}
		for name, child := range v {
			p, ok := props[name]
			if !ok {
				out = append(out, where+": property "+name+" is not in the contract")
				continue
			}
			out = append(out, mismatches(doc, where+"."+name, p.(map[string]any), child)...)
		}
	case []any:
		if items := mapOf(target["items"]); items != nil {
			for i, child := range v {
				out = append(out, mismatches(doc, fmt.Sprintf("%s[%d]", where, i), items, child)...)
			}
		}
	}
	return out
}
