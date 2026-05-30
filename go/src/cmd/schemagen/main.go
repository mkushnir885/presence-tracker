package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
)

// schemagen writes config.schema.json and bank.schema.json from the Go schema
// definitions. Run by `just gen` so the committed schemas stay in sync.
func main() {
	dir := flag.String("dir", "..", "output directory; one .json file per schema is written here")
	flag.Parse()

	configSchema, err := config.Schema()
	if err != nil {
		log.Fatalf("schemagen: config schema: %v", err)
	}
	bankSchema := challenges.BankSchema()

	for name, s := range map[string]*jsonschema.Schema{
		"config.schema.json": configSchema,
		"bank.schema.json":   bankSchema,
	} {
		path := filepath.Join(*dir, name)
		if err := writeSchema(path, s); err != nil {
			log.Fatalf("schemagen: write %s: %v", path, err)
		}
		fmt.Fprintf(os.Stderr, "schemagen: wrote %s\n", path)
	}
}

func writeSchema(path string, s *jsonschema.Schema) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o644) //nolint:gosec // schema files are public, world-readable by design
}
