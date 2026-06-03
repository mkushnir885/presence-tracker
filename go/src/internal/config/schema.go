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

// Schema builds the JSON Schema from the Values struct, fills each field's
// default from defaults(), and applies extra constraints. It is the single
// source of truth for both config validation and the editor form.
func Schema() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[Values](nil)
	if err != nil {
		return nil, fmt.Errorf("config: build schema: %w", err)
	}
	schema.Schema = "https://json-schema.org/draft/2020-12/schema"
	schema.Title = "Presence Tracker config"
	schema.Description = "Generated from go/src/internal/config — do not edit by hand."

	home, _ := os.UserHomeDir()
	if err := injectDefaults(schema, reflect.ValueOf(defaults()), home); err != nil {
		return nil, fmt.Errorf("config: inject defaults: %w", err)
	}
	applyConstraints(schema)
	// Allow $schema so IDEs don't flag the conventional self-reference as an
	// additional property when additionalProperties is false.
	schema.Properties["$schema"] = &jsonschema.Schema{Type: "string"}
	return schema, nil
}

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
		// Show path defaults as ~-relative so the schema isn't tied to the
		// machine's home directory.
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
