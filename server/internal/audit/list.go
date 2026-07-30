package audit

import (
	"context"
	"fmt"
)

// ListFilter controls ListEvents.
type ListFilter struct {
	Limit int
}

// ListEvents returns recent audit rows newest-first (by rowid DESC).
func (w *Writer) ListEvents(ctx context.Context, f ListFilter) ([]Record, error) {
	if w == nil || w.db == nil {
		return nil, fmt.Errorf("audit: nil writer")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := w.db.SQL().QueryContext(ctx, `
		SELECT id, ts, actor, actor_type, action, target, detail_json, prev_hash, row_hash
		FROM audit_events
		ORDER BY rowid DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TS, &rec.Actor, &rec.ActorType, &rec.Action, &rec.Target, &rec.Detail, &rec.PrevHash, &rec.RowHash); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
