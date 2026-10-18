// Package testfixture holds the Datawiz-generated demo snapshot for Cloud tests.
package testfixture

import (
	"bytes"
	"embed"
	"encoding/json"

	"energylens.local/cloud/internal/democost"
)

//go:embed demo_snapshot_golden.json
var fixtures embed.FS

func DemoSnapshot() democost.Snapshot {
	data, err := fixtures.ReadFile("demo_snapshot_golden.json")
	if err != nil {
		panic(err)
	}
	var snapshot democost.Snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&snapshot); err != nil {
		panic(err)
	}
	if err = snapshot.Validate(); err != nil {
		panic(err)
	}
	return snapshot
}
