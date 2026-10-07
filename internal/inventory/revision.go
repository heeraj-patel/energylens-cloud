package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const InventoryRevisionHeader = "X-EnergyLens-Inventory-Revision"

// inventoryRevision is an opaque, deterministic token scoped to one home and
// data mode. JSON encoding fixes the field order for the appliance snapshot.
func inventoryRevision(mode, home string, appliances []Appliance) string {
	data, _ := json.Marshal(struct {
		Mode       string      `json:"mode"`
		Home       string      `json:"home"`
		Appliances []Appliance `json:"appliances"`
	}{mode, home, appliances})
	sum := sha256.Sum256(data)
	return "v1-" + hex.EncodeToString(sum[:])
}
