package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/myers/drawbar/pkg/types"
)

// Type aliases for the shared types used by the entrypoint.
type StepDef = types.ManifestStep
type StepResult = types.StepResult
type StateEvent = types.StateEvent

func loadManifest(path string) (*types.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	var m types.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, nil
}
