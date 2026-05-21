package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schema returns a fresh JSON Schema describing Config. It is built by
// reflecting over the struct (via jsonschema.For), then applying defaults
// from Defaults() and the static constraints declared in
// applyConstraints (see config.go). Callers that need to validate use
// ResolvedSchema, which caches a resolved copy.
//
// Home-directory prefixes in path defaults are rewritten to "~/..." so
// the emitted schema is identical across user accounts — purely
// annotation, with no effect on validation.
func Schema() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[Config](nil)
	if err != nil {
		return nil, fmt.Errorf("config: build schema: %w", err)
	}
	schema.Schema = "https://json-schema.org/draft/2020-12/schema"
	schema.Title = "Presence Tracker config"
	schema.Description = "Generated from go/src/internal/config — do not edit by hand."

	home, _ := os.UserHomeDir()
	if err := injectDefaults(schema, reflect.ValueOf(Defaults()), home); err != nil {
		return nil, fmt.Errorf("config: inject defaults: %w", err)
	}
	applyConstraints(schema)
	return schema, nil
}

// ResolvedSchema returns a cached *jsonschema.Resolved for Config; the
// same Resolved is safe for concurrent use by multiple Load calls.
func ResolvedSchema() (*jsonschema.Resolved, error) {
	resolvedSchemaOnce.Do(func() {
		s, err := Schema()
		if err != nil {
			resolvedSchemaErr = err
			return
		}
		resolvedSchema, resolvedSchemaErr = s.Resolve(nil)
		if resolvedSchemaErr != nil {
			resolvedSchemaErr = fmt.Errorf("config: resolve schema: %w", resolvedSchemaErr)
		}
	})
	return resolvedSchema, resolvedSchemaErr
}

var (
	resolvedSchemaOnce sync.Once
	resolvedSchema     *jsonschema.Resolved
	resolvedSchemaErr  error
)

// injectDefaults walks schema and v in lockstep, setting Default on every
// leaf property to the corresponding field's value. Nested structs
// recurse; slices/maps are left without a default. Any string default
// starting with home is rewritten to "~/..." so the emitted schema is
// reproducible across machines.
func injectDefaults(schema *jsonschema.Schema, v reflect.Value, home string) error {
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := jsonName(field)
		if name == "" || name == "-" {
			continue
		}
		prop, ok := schema.Properties[name]
		if !ok {
			continue
		}
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct {
			if err := injectDefaults(prop, fv, home); err != nil {
				return err
			}
			continue
		}

		val := fv.Interface()
		if s, ok := val.(string); ok && home != "" && strings.HasPrefix(s, home) {
			val = "~" + s[len(home):]
		}
		raw, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("marshal default for %s: %w", name, err)
		}
		prop.Default = raw
	}
	return nil
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	for i, c := range tag {
		if c == ',' {
			return tag[:i]
		}
	}
	return tag
}
