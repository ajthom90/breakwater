// Package rescan rebuilds the catalog's snapshot index from in-repo manifests.
//
// PLAN: catalog is a rebuildable index — authoritative snapshot records live in
// each machine's vault. `bwctl rescan` (and the server-loss drill) re-index from
// repo directories alone after catalog loss.
package rescan

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// Result summarizes a rescan run.
type Result struct {
	MachinesScanned int
	SnapshotsFound  int
	SnapshotsAdded  int
	Errors          []string
}

// Options configure Rescan.
type Options struct {
	DB       *catalog.DB
	Keystore *keystore.Store
	Vaults   *vault.Manager
	Log      *slog.Logger
}

// Run walks every enrolled machine's vault, lists snapshot manifests, and
// inserts any missing catalog rows (matched by manifest_ref).
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.DB == nil || opts.Keystore == nil || opts.Vaults == nil {
		return nil, fmt.Errorf("rescan: DB, Keystore, and Vaults required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	machines, err := opts.DB.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, m := range machines {
		res.MachinesScanned++
		repoID := m.RepoID
		if repoID == "" {
			repoID = m.ID
		}
		pw, err := opts.Keystore.GetRepoPassword(ctx, repoID)
		if err != nil {
			msg := fmt.Sprintf("machine %s: password: %v", m.ID, err)
			res.Errors = append(res.Errors, msg)
			log.Warn("rescan skip machine", "machine_id", m.ID, "err", err)
			continue
		}
		v, err := opts.Vaults.Open(ctx, repoID, pw)
		if err != nil {
			msg := fmt.Sprintf("machine %s: open vault: %v", m.ID, err)
			res.Errors = append(res.Errors, msg)
			log.Warn("rescan skip machine", "machine_id", m.ID, "err", err)
			continue
		}
		// List all known snapshot kinds.
		var metas []vault.SnapshotMeta
		for _, kind := range []vault.SnapshotKind{vault.KindFileSnapshot, vault.KindImageSnapshot} {
			list, err := v.ListSnapshotRecords(ctx, kind)
			if err != nil {
				msg := fmt.Sprintf("machine %s: list %s: %v", m.ID, kind, err)
				res.Errors = append(res.Errors, msg)
				continue
			}
			metas = append(metas, list...)
		}
		// Also try unfiltered if API supports empty kind — list both already.
		for _, meta := range metas {
			res.SnapshotsFound++
			existing, err := opts.DB.SnapshotByManifestRef(ctx, string(meta.ID))
			if err != nil {
				res.Errors = append(res.Errors, err.Error())
				continue
			}
			if existing != nil {
				continue
			}
			// Load full record for root object id / source.
			rec, err := v.GetSnapshotRecord(ctx, meta.ID)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("get %s: %v", meta.ID, err))
				continue
			}
			kind := "file"
			switch rec.Kind {
			case vault.KindImageSnapshot:
				kind = "image"
			}
			id := ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
			// Preserve vault timestamp when present.
			created := rec.Timestamp
			if created.IsZero() {
				created = meta.Timestamp
			}
			if err := opts.DB.InsertSnapshot(ctx, catalog.Snapshot{
				ID:           id,
				MachineID:    m.ID,
				Kind:         kind,
				Source:       rec.Source,
				ManifestRef:  string(meta.ID),
				RootObjectID: string(rec.RootObjectID),
				JobID:        rec.JobID,
			}); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("insert %s: %v", meta.ID, err))
				continue
			}
			// CreatedAt is set by SQLite default; catalog InsertSnapshot uses now.
			_ = created
			res.SnapshotsAdded++
			log.Info("rescan added snapshot", "machine_id", m.ID, "manifest", meta.ID, "catalog_id", id)
		}
	}
	return res, nil
}
