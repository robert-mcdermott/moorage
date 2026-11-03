package manifest

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSchemaMatchesTypes checks that the JSON Schema and the Go types name
// the same keys, table by table.
func TestSchemaMatchesTypes(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	defs := root["$defs"].(map[string]any)
	resolve := func(node map[string]any) map[string]any {
		for {
			ref, ok := node["$ref"].(string)
			if !ok {
				return node
			}
			node = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
	}
	var walk func(path string, typ reflect.Type, node map[string]any)
	walk = func(path string, typ reflect.Type, node map[string]any) {
		node = resolve(node)
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		switch typ.Kind() {
		case reflect.Struct:
			if typ == reflect.TypeOf(Duration(0)) || typ == reflect.TypeOf(Size(0)) {
				return
			}
			props, _ := node["properties"].(map[string]any)
			seen := map[string]bool{}
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				key, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
				if key == "" || key == "-" {
					continue
				}
				seen[key] = true
				sub, ok := props[key].(map[string]any)
				if !ok {
					t.Errorf("%s.%s is in the Go type but not in the schema", path, key)
					continue
				}
				ft := f.Type
				if typ == reflect.TypeOf(Manifest{}) && key == "processes" {
					ft = reflect.TypeOf(map[string]Process{})
					sub = resolve(sub)["additionalProperties"].(map[string]any)["oneOf"].([]any)[1].(map[string]any)
					walk(path+"."+key+".*", ft.Elem(), sub)
					continue
				}
				walk(path+"."+key, ft, sub)
			}
			var extra []string
			for key := range props {
				if !seen[key] {
					extra = append(extra, key)
				}
			}
			sort.Strings(extra)
			for _, key := range extra {
				t.Errorf("%s.%s is in the schema but not in the Go type", path, key)
			}
		case reflect.Map:
			if sub, ok := node["additionalProperties"].(map[string]any); ok {
				walk(path+".*", typ.Elem(), sub)
			}
		case reflect.Slice:
			if sub, ok := node["items"].(map[string]any); ok {
				walk(path+"[]", typ.Elem(), sub)
			}
		}
	}
	walk("manifest", reflect.TypeOf(Manifest{}), root)
}
