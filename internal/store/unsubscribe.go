package store

import (
	"context"
	"fmt"
)

// UnsubscribeLink is the one-click unsubscribe link of the newest email from an address on
// one mailbox.
type UnsubscribeLink struct {
	AccountID int64
	URL       string
}

// UnsubscribeLinks lists, for every mailbox the viewer sees that MailRules has not
// unsubscribed from address yet, the one-click link of the newest email from address seen
// since `since` that offers one. A mailbox whose mail from address offers none is not listed.
func (s *Store) UnsubscribeLinks(ctx context.Context, v Viewer, address string, since int64) ([]UnsubscribeLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.account_id, m.unsubscribe_url FROM messages m
		 WHERE m.id IN (SELECT MAX(n.id) FROM messages n
		                WHERE `+inVisible("n.account_id")+` AND n.from_addr = ?1 AND n.unsubscribe_url IS NOT NULL AND n.created_at >= ?2
		                GROUP BY n.account_id)
		   AND NOT EXISTS (SELECT 1 FROM unsubscribes u WHERE u.account_id = m.account_id AND u.address = ?1)
		 ORDER BY m.account_id`, address, since, v.tenantArg(), v.userArg())
	if err != nil {
		return nil, fmt.Errorf("list unsubscribe links: %w", err)
	}
	defer rows.Close()
	var out []UnsubscribeLink
	for rows.Next() {
		var l UnsubscribeLink
		if err := rows.Scan(&l.AccountID, &l.URL); err != nil {
			return nil, fmt.Errorf("list unsubscribe links: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unsubscribe links: %w", err)
	}
	return out, nil
}

// AddUnsubscribe records that MailRules unsubscribed a mailbox from address at host (the
// site that took it; the link is not kept).
func (s *Store) AddUnsubscribe(ctx context.Context, accountID int64, address, host string, now int64) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO unsubscribes (account_id, address, host, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (account_id, address) DO UPDATE SET host = excluded.host, created_at = excluded.created_at`,
		accountID, address, host, now); err != nil {
		return fmt.Errorf("record unsubscribe: %w", err)
	}
	return nil
}
