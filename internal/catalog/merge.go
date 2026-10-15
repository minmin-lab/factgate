package catalog

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MergeYAMLFragment appends the top-level sequences of fragment (for example
// the snapshot_publications, products, scopes, knowledge_products and
// kb_publications a publisher emits) to the same-named sequences of base, and
// adds top-level keys base does not have. Scalars and mappings that exist in
// both are refused: a fragment extends a Catalog, it never rewrites one. The
// result still has to pass Parse.
func MergeYAMLFragment(base, fragment []byte) ([]byte, error) {
	var baseDocument, fragmentDocument yaml.Node
	if err := yaml.Unmarshal(base, &baseDocument); err != nil {
		return nil, fmt.Errorf("parse base catalog YAML: %w", err)
	}
	if err := yaml.Unmarshal(fragment, &fragmentDocument); err != nil {
		return nil, fmt.Errorf("parse catalog fragment YAML: %w", err)
	}
	baseMap, err := documentMapping(&baseDocument)
	if err != nil {
		return nil, fmt.Errorf("base catalog: %w", err)
	}
	fragmentMap, err := documentMapping(&fragmentDocument)
	if err != nil {
		return nil, fmt.Errorf("catalog fragment: %w", err)
	}
	for index := 0; index+1 < len(fragmentMap.Content); index += 2 {
		key, value := fragmentMap.Content[index], fragmentMap.Content[index+1]
		existing := mappingValue(baseMap, key.Value)
		switch {
		case existing == nil:
			baseMap.Content = append(baseMap.Content, key, value)
		case existing.Kind == yaml.SequenceNode && value.Kind == yaml.SequenceNode:
			existing.Content = append(existing.Content, value.Content...)
		default:
			return nil, fmt.Errorf("fragment key %q is not an appendable sequence in the base catalog", key.Value)
		}
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&baseDocument); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func documentMapping(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("YAML must be a single mapping document")
	}
	return document.Content[0], nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}
