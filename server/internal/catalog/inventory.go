package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// InventoryItem is one volume or VM row in machine_inventory.
type InventoryItem struct {
	MachineID  string
	Kind       string // volume|vm
	ExternalID string
	Name       string
	Details    map[string]any // serialized to details_json
	RCTCapable bool
}

// ReplaceMachineInventory atomically replaces all inventory rows for a machine
// with the provided set (full refresh from InventoryReport).
func (db *DB) ReplaceMachineInventory(ctx context.Context, machineID string, items []InventoryItem) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM machine_inventory WHERE machine_id = ?`, machineID); err != nil {
			return fmt.Errorf("clear inventory: %w", err)
		}
		for _, it := range items {
			if it.Kind == "" || it.ExternalID == "" {
				return fmt.Errorf("inventory item missing kind or external_id")
			}
			details := "{}"
			if it.Details != nil {
				b, err := json.Marshal(it.Details)
				if err != nil {
					return fmt.Errorf("marshal details: %w", err)
				}
				details = string(b)
			}
			rct := 0
			if it.RCTCapable {
				rct = 1
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO machine_inventory (machine_id, kind, external_id, name, details_json, rct_capable)
				VALUES (?, ?, ?, ?, ?, ?)`,
				machineID, it.Kind, it.ExternalID, it.Name, details, rct); err != nil {
				return fmt.Errorf("insert inventory: %w", err)
			}
		}
		return nil
	})
}

// ListMachineInventory returns all inventory rows for a machine.
func (db *DB) ListMachineInventory(ctx context.Context, machineID string) ([]InventoryItem, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT machine_id, kind, external_id, name, details_json, rct_capable
		FROM machine_inventory WHERE machine_id = ?
		ORDER BY kind, external_id`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InventoryItem
	for rows.Next() {
		var it InventoryItem
		var details string
		var rct int
		if err := rows.Scan(&it.MachineID, &it.Kind, &it.ExternalID, &it.Name, &details, &rct); err != nil {
			return nil, err
		}
		it.RCTCapable = rct != 0
		if details != "" && details != "{}" {
			_ = json.Unmarshal([]byte(details), &it.Details)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
