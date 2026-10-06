package store

import (
	"context"
	"fmt"
)

// KeepMessagesDays is how long a message's row is kept. After that it is deleted with its
// decisions and actions, unless one of its actions can still be undone: a move or a flag
// change that is in effect. A recorded "keep" and the Needs review tag changed nothing
// worth putting back, so they do not hold a row.
const KeepMessagesDays = 180

const day = 24 * 3600

// Retain is the retention job's one step: it blanks the snippet of every message first
// seen more than retentionDays ago, and deletes the messages first seen more than
// KeepMessagesDays ago that have no action left to undo (their decisions, actions and
// corrections go with them). It returns how many rows each part touched.
func (s *Store) Retain(ctx context.Context, now int64, retentionDays int) (blanked, deleted int64, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET snippet = NULL WHERE created_at < ? AND snippet IS NOT NULL AND snippet <> ''`, now-int64(retentionDays)*day)
	if err != nil {
		return 0, 0, fmt.Errorf("blank old snippets: %w", err)
	}
	blanked, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE created_at < ? AND NOT EXISTS
		   (SELECT 1 FROM actions a WHERE a.message_id = messages.id AND a.status = 'done' AND a.kind NOT IN ('review', 'keep'))`,
		now-KeepMessagesDays*day)
	if err != nil {
		return blanked, 0, fmt.Errorf("delete old messages: %w", err)
	}
	deleted, _ = res.RowsAffected()
	return blanked, deleted, nil
}
