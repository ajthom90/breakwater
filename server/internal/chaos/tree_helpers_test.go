package chaos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

func writeTreeNoT(ctx context.Context, v vault.Vault, files map[string]string) (vault.ObjectID, error) {
	entries := make([]format.TreeEntry, 0, len(files))
	for name, content := range files {
		oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader([]byte(content)))
		if err != nil {
			return "", err
		}
		entries = append(entries, format.TreeEntry{
			Name: name, Type: format.EntryFile, Size: int64(len(content)), ObjectID: string(oid),
		})
	}
	tree := format.TreeObject{Version: format.FormatVersion, Entries: entries}
	raw, err := json.Marshal(tree)
	if err != nil {
		return "", err
	}
	return v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(raw))
}

// putSnapDirectNoT inserts a snapshot without using testing.T (safe from worker goroutines).
func putSnapDirectNoT(ctx context.Context, db *catalog.DB, v vault.Vault, machineID, name, payload string, ts time.Time) (string, vault.ObjectID, error) {
	root, err := writeTreeNoT(ctx, v, map[string]string{name: payload})
	if err != nil {
		return "", "", err
	}
	rec, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp: ts, RootObjectID: root, Source: "/chaos",
	})
	if err != nil {
		return "", "", err
	}
	id := ulid.Make().String()
	if err := db.InsertSnapshot(ctx, catalog.Snapshot{
		ID: id, MachineID: machineID, Kind: "file", Source: "/chaos",
		ManifestRef: string(rec), RootObjectID: string(root), CreatedAt: ts,
	}); err != nil {
		return "", "", fmt.Errorf("insert: %w", err)
	}
	return id, root, nil
}
