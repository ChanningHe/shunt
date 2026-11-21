package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pelletier/go-toml/v2"
	yaml "go.yaml.in/yaml/v3"
)

// Every format goes through the same pipeline so there is exactly one set of
// struct tags and one strictness rule:
//
//	decode: format parser -> map[string]any -> JSON -> strict encoding/json
//	encode: encoding/json -> map[string]any -> format marshaller
//
// Map keys are sorted by all three marshallers, so encoding is deterministic.
// Comments and key order of hand-edited files are not preserved.

const (
	formatTOML = "toml"
	formatYAML = "yaml"
	formatJSON = "json"
)

// extensions lists the accepted file extensions in discovery order.
var extensions = []string{"toml", "yaml", "yml", "json"}

func formatOf(ext string) string {
	if ext == "yml" {
		return formatYAML
	}
	return ext
}

// decode strictly decodes data in format into v; unknown fields are errors.
// A blank file decodes to the zero value in every format.
func decode(format string, data []byte, v any) error {
	tree, err := parse(format, data)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("%s: %w", format, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func parse(format string, data []byte) (map[string]any, error) {
	tree := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return tree, nil
	}
	var err error
	switch format {
	case formatTOML:
		err = toml.Unmarshal(data, &tree)
	case formatYAML:
		err = yaml.Unmarshal(data, &tree)
	case formatJSON:
		err = json.Unmarshal(data, &tree)
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
	return tree, err
}

// encode renders v in format. Empty fields tagged omitempty stay absent.
func encode(format string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	tree := map[string]any{}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	if tree == nil { // a nil map marshals to null
		tree = map[string]any{}
	}
	var buf bytes.Buffer
	switch format {
	case formatTOML:
		err = toml.NewEncoder(&buf).Encode(tree)
	case formatYAML:
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err = enc.Encode(tree); err == nil {
			err = enc.Close()
		}
	case formatJSON:
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		err = enc.Encode(tree)
	default:
		err = fmt.Errorf("unknown format %q", format)
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
