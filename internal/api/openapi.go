package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// The OpenAPI document is generated from the route table and the API's types,
// so that it cannot describe anything else. A copy is checked in as
// docs/openapi.json, and a test fails when they differ.

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.openapiDoc())
}

var pathParam = regexp.MustCompile(`\{([a-z]+)\}`)

// openAPIDocument returns the OpenAPI 3.1 document of routes.
func openAPIDocument(routes []route) []byte {
	schemas := map[string]any{}
	paths := map[string]any{}
	for _, rt := range routes {
		item, _ := paths[rt.path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[rt.path] = item
		}
		item[strings.ToLower(rt.method)] = operation(rt, schemas)
	}
	schemaFor(reflect.TypeFor[apitypes.Problem](), schemas)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Moorage API",
			"version":     "1",
			"description": "The REST API of a Moorage server. Lists that can grow are paged with ?limit and ?cursor. Errors are application/problem+json documents with a stable code.",
		},
		"servers": []any{map[string]any{"url": basePath}},
		"paths":   paths,
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"token":   map[string]any{"type": "http", "scheme": "bearer", "description": "An API token."},
				"session": map[string]any{"type": "apiKey", "in": "cookie", "name": "__Host-moorage_session", "description": "A browser's session; requests that change anything also send the X-CSRF-Token header."},
			},
			"responses": map[string]any{
				"Problem": map[string]any{
					"description": "The request failed.",
					"content":     map[string]any{apitypes.ProblemType: map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}},
				},
			},
		},
	}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func operation(rt route, schemas map[string]any) map[string]any {
	op := map[string]any{
		"operationId":          operationID(rt),
		"summary":              rt.summary,
		"x-moorage-permission": rt.perm.String(),
		"tags":                 []any{strings.SplitN(strings.TrimPrefix(rt.path, "/"), "/", 2)[0]},
	}
	var params []any
	for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
		params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
	}
	names := make([]string, 0, len(rt.query))
	for name := range rt.query {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		params = append(params, map[string]any{"name": name, "in": "query", "description": rt.query[name], "schema": map[string]any{"type": "string"}})
	}
	if params != nil {
		op["parameters"] = params
	}
	switch {
	case rt.request != nil:
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{
			"application/json": map[string]any{"schema": schemaFor(reflect.TypeOf(rt.request), schemas)},
		}}
	case rt.multipart:
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{
			"multipart/form-data": map[string]any{"schema": map[string]any{
				"type":        "object",
				"description": "Source alone (" + apitypes.SourcePart + "), or a plan followed by its files (" + apitypes.PlanPart + ", then " + apitypes.ArchivePart + "). Files are a gzip-compressed tar.",
				"properties": map[string]any{
					apitypes.SourcePart:  map[string]any{"type": "string", "contentEncoding": "binary"},
					apitypes.PlanPart:    map[string]any{"type": "string"},
					apitypes.ArchivePart: map[string]any{"type": "string", "contentEncoding": "binary"},
				},
			}},
		}}
	}
	status := rt.status
	if status == 0 {
		status = http.StatusOK
	}
	ok := map[string]any{"description": http.StatusText(status)}
	switch {
	case rt.response != nil:
		ok["content"] = map[string]any{"application/json": map[string]any{"schema": schemaFor(reflect.TypeOf(rt.response), schemas)}}
	case rt.stream != nil:
		ok["description"] = "A stream of lines, each a JSON document."
		ok["content"] = map[string]any{apitypes.NDJSONType: map[string]any{"schema": schemaFor(reflect.TypeOf(rt.stream), schemas)}}
	}
	problemRef := map[string]any{"$ref": "#/components/responses/Problem"}
	responses := map[string]any{fmt.Sprint(status): ok, "default": problemRef}
	if rt.perm == public {
		op["security"] = []any{}
	} else {
		op["security"] = []any{map[string]any{"token": []any{}}, map[string]any{"session": []any{}}}
		responses["401"], responses["403"] = problemRef, problemRef
	}
	op["responses"] = responses
	return op
}

// operationID names an operation from its method and path.
func operationID(rt route) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(rt.method))
	for _, part := range strings.FieldsFunc(rt.path, func(r rune) bool { return strings.ContainsRune("/{}.-", r) }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

var timeType = reflect.TypeFor[time.Time]()

// schemaFor returns the JSON Schema of a type, adding structs to schemas.
func schemaFor(t reflect.Type, schemas map[string]any) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == reflect.TypeFor[json.RawMessage]():
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaFor(t.Elem(), schemas)
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), schemas)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), schemas)}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		name := schemaName(t)
		if _, seen := schemas[name]; !seen {
			schemas[name] = nil // for a type that refers to itself
			schemas[name] = structSchema(t, schemas)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	panic("no schema for " + t.String())
}

// schemaName names a struct; Page[apitypes.App] is AppPage.
func schemaName(t reflect.Type) string {
	name := t.Name()
	if inner, ok := strings.CutPrefix(name, "Page["); ok {
		inner = strings.TrimSuffix(inner, "]")
		return inner[strings.LastIndex(inner, ".")+1:] + "Page"
	}
	return name
}

func structSchema(t reflect.Type, schemas map[string]any) map[string]any {
	properties := map[string]any{}
	var required []any
	addFields(t, properties, &required, schemas)
	out := map[string]any{"type": "object", "properties": properties}
	if required != nil {
		out["required"] = required
	}
	return out
}

func addFields(t reflect.Type, properties map[string]any, required *[]any, schemas map[string]any) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			addFields(f.Type, properties, required, schemas)
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		properties[name] = schemaFor(f.Type, schemas)
		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
			*required = append(*required, name)
		}
	}
}
