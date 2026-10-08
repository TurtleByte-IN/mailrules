package store

import (
	"context"
	"fmt"
)

// UndoDays is how long an action stays undoable. Older ones are history: undo refuses them
// (actions.ErrTooOld) and they no longer hold their message's row.
const UndoDays = 30

// KeepMessagesDays is how long a message's row is kept. After that it is deleted with its
// decisions and actions, unless one of its actions can still be undone: a move or a flag
// change that is in effect and was made in the last UndoDays. A recorded "keep" changed
// nothing worth putting back, so it does not hold a row.
const KeepMessagesDays = 180

const day = 24 * 3600

// Retain is the retention job's one step for one tenant: it blanks the snippet of every
// message of the tenant's mailboxes first seen more than retentionDays ago (the tenant's
// retention_days), and deletes the tenant's messages first seen more than KeepMessagesDays
// ago that have no action left to undo, that is, none in effect from the last UndoDays
// (their decisions, actions and corrections go with them). It returns how many rows each
// part touched.
func (s *Store) Retain(ctx context.Context, tenantID, now int64, retentionDays int) (blanked, deleted int64, err error) {
	const ofTenant = `account_id IN (SELECT id FROM accounts WHERE tenant_id = ?)`
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET snippet = NULL WHERE `+ofTenant+` AND created_at < ? AND snippet IS NOT NULL AND snippet <> ''`,
		tenantID, now-int64(retentionDays)*day)
	if err != nil {
		return 0, 0, fmt.Errorf("blank old snippets: %w", err)
	}
	blanked, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE `+ofTenant+` AND created_at < ? AND NOT EXISTS
		   (SELECT 1 FROM actions a WHERE a.message_id = messages.id AND a.status = 'done' AND a.kind <> 'keep'
		    AND a.created_at >= ?)`,
		tenantID, now-KeepMessagesDays*day, now-UndoDays*day)
	if err != nil {
		return blanked, 0, fmt.Errorf("delete old messages: %w", err)
	}
	deleted, _ = res.RowsAffected()
	return blanked, deleted, nil
}
