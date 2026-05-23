package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// schemaFilename is the name written next to config.json to give editors
// (VSCode, JetBrains) automatic validation + autocomplete via the
// $schema relative reference in the config file.
const schemaFilename = "config.schema.json"

// writeConfigFile renders overrides as canonical JSON with a $schema
// hint and writes it to path atomically. The sibling schema file is
// written next to it so the relative $schema reference resolves.
func writeConfigFile(path string, overrides map[string]any) error {
	if path == "" {
		return fmt.Errorf("config: no path bound; cannot save")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}

	body, err := marshalCanonical(overrides)
	if err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o600); err != nil {
		return err
	}
	return writeSiblingSchema(filepath.Dir(path))
}

// marshalCanonical produces stable JSON with $schema as the first key
// and the overrides body (sorted by Go's encoding/json — alphabetical
// map keys) following. Indented with two spaces and trailing newline.
func marshalCanonical(overrides map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n  \"$schema\": ")
	schemaRef, _ := json.Marshal("./" + schemaFilename)
	buf.Write(schemaRef)

	if len(overrides) == 0 {
		buf.WriteString("\n}\n")
		return buf.Bytes(), nil
	}

	body, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config: marshal: %w", err)
	}
	// body starts with "{\n  ..." and ends with "\n}". Splice the inner
	// lines (everything between "{" and "}") after the $schema entry,
	// keeping the 2-space indentation produced by MarshalIndent.
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, fmt.Errorf("config: unexpected marshal output")
	}
	inner := bytes.TrimSpace(body[1 : len(body)-1])
	buf.WriteString(",\n  ")
	buf.Write(inner)
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// atomicWrite writes data to path via a sibling temp file + rename,
// explicitly chmodding to mode so a pre-existing tighter mode is not
// widened by the rename.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return fmt.Errorf("config: chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("config: rename temp: %w", err)
	}
	return nil
}

// writeSiblingSchema renders the JSON Schema for Values and writes it
// to <dir>/config.schema.json. Overwrites unconditionally — schema is
// deterministic from the binary.
func writeSiblingSchema(dir string) error {
	schema, err := Schema()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal schema: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, schemaFilename), data, 0o644); err != nil {
		return fmt.Errorf("config: write schema: %w", err)
	}
	return nil
}
