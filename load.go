package semantic

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load parses a YAML model and validates it.
func Load(data []byte) (*Model, error) {
	var m Model
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse model: %w", err)
	}
	if err := m.Index(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadFile reads and parses a YAML model from disk.
func LoadFile(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data)
}
