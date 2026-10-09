// Package actions is the only code that changes a mailbox: the executor that applies a
// rule's actions, and undo. This file holds the types its callers share.
package actions

import (
	"context"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// DecisionRecord says which message to act on and for which decision.
type DecisionRecord struct {
	DecisionID int64       // 0 = no decision, e.g. a correction
	MessageID  int64       // messages.id
	Ref        mail.MsgRef // where the message is now
}

// ActionRecord is one row of the actions table.
type ActionRecord = store.Action

// Executor applies and undoes actions.
type Executor interface {
	// Apply runs acts in order on one message and returns a record per action attempted.
	// A failure stops the remaining actions and is returned with the records so far.
	Apply(ctx context.Context, d DecisionRecord, acts []rules.Action, batchID int64) ([]ActionRecord, error)
	Undo(ctx context.Context, actionID int64) error
	// UndoBatch undoes the actions of a batch the viewer sees on the mailboxes they see.
	UndoBatch(ctx context.Context, v store.Viewer, batchID int64) (Undid, error)
}
