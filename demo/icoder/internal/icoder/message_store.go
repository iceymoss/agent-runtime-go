package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

// SQLiteMessageService persists message aggregates.
//
// It is deliberately thin. Every legality decision - which state transitions are
// allowed, what a redacted message may contain, how tool calls and results must
// correlate across a branch - comes from the message package's exported state
// machine. This type owns only the three things storage actually owns:
// allocating a branch position, comparing revisions atomically, and reading rows
// back in the required order.
type SQLiteMessageService struct{ db *sql.DB }

// NewSQLiteMessageService binds the message aggregate store to an
// already-migrated database.
func NewSQLiteMessageService(db *sql.DB) *SQLiteMessageService { return &SQLiteMessageService{db: db} }

var _ message.Service = (*SQLiteMessageService)(nil)

// Messages returns the message aggregate service backed by this store's database.
func (s *Store) Messages() *SQLiteMessageService { return NewSQLiteMessageService(s.db) }

var messageMigrations = []string{
	`CREATE TABLE IF NOT EXISTS message_aggregates (
		tenant_key TEXT NOT NULL,
		message_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		branch_key TEXT NOT NULL,
		branch_ordinal INTEGER NOT NULL,
		state TEXT NOT NULL,
		revision INTEGER NOT NULL,
		visible_at_revision INTEGER NOT NULL DEFAULT 0,
		created_command BLOB NOT NULL,
		payload BLOB NOT NULL,
		PRIMARY KEY (tenant_key, message_key),
		UNIQUE (tenant_key, session_key, branch_key, branch_ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS message_aggregates_visible
		ON message_aggregates(tenant_key, session_key, visible_at_revision, branch_key, branch_ordinal)`,
}

func (s *SQLiteMessageService) begin(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

func loadMessage(ctx context.Context, tx *sql.Tx, tenant agent.TenantKey, key message.MessageKey) (message.Snapshot, message.CreateCommand, error) {
	var payload, created []byte
	err := tx.QueryRowContext(ctx, `SELECT payload, created_command FROM message_aggregates WHERE tenant_key = ? AND message_key = ?`,
		string(tenant), string(key)).Scan(&payload, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return message.Snapshot{}, message.CreateCommand{}, message.ErrMessageNotFound
	}
	if err != nil {
		return message.Snapshot{}, message.CreateCommand{}, err
	}
	var snapshot message.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return message.Snapshot{}, message.CreateCommand{}, err
	}
	var command message.CreateCommand
	if err := json.Unmarshal(created, &command); err != nil {
		return message.Snapshot{}, message.CreateCommand{}, err
	}
	return snapshot, command, nil
}

func writeMessage(ctx context.Context, tx *sql.Tx, snapshot message.Snapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE message_aggregates SET state = ?, revision = ?, visible_at_revision = ?, payload = ? WHERE tenant_key = ? AND message_key = ?`,
		string(snapshot.State), snapshot.Revision, snapshot.VisibleAtRevision, payload,
		string(snapshot.TenantKey), string(snapshot.MessageKey))
	return err
}

// Create records a new message aggregate.
//
// The branch position is allocated here because only storage can see the branch,
// and it is allocated inside the same transaction as the insert so two
// concurrent creates cannot both claim the same slot.
func (s *SQLiteMessageService) Create(ctx context.Context, command message.CreateCommand) (snapshot message.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return message.Snapshot{}, err
	}
	defer finishTx(tx, &err)

	existing, created, loadErr := loadMessage(ctx, tx, command.TenantKey, command.MessageKey)
	if loadErr == nil {
		if message.SameCreate(command, created, existing.BranchOrdinal) {
			return existing, nil
		}
		return message.Snapshot{}, fmt.Errorf("%w: message key %q has different immutable input", message.ErrIdempotencyConflict, command.MessageKey)
	}
	if !errors.Is(loadErr, message.ErrMessageNotFound) {
		return message.Snapshot{}, loadErr
	}

	ordinal := command.BranchOrdinal
	if ordinal == 0 {
		if ordinal, err = nextBranchOrdinal(ctx, tx, command); err != nil {
			return message.Snapshot{}, err
		}
	} else if used, usedErr := ordinalUsed(ctx, tx, command, ordinal); usedErr != nil {
		return message.Snapshot{}, usedErr
	} else if used {
		return message.Snapshot{}, fmt.Errorf("%w: branch ordinal %d is already used", message.ErrOrdinalConflict, ordinal)
	}

	snapshot, err = message.ApplyCreate(command, ordinal, time.Now().UTC())
	if err != nil {
		return message.Snapshot{}, err
	}
	siblings, err := branchSiblings(ctx, tx, snapshot)
	if err != nil {
		return message.Snapshot{}, err
	}
	if err = message.ValidateBranchCorrelation(snapshot, siblings); err != nil {
		return message.Snapshot{}, err
	}

	normalized := command
	if normalized.State == "" {
		normalized.State = message.StateBuilding
	}
	commandPayload, err := json.Marshal(normalized)
	if err != nil {
		return message.Snapshot{}, err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return message.Snapshot{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_aggregates(tenant_key, message_key, session_key, branch_key, branch_ordinal, state, revision, visible_at_revision, created_command, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(snapshot.TenantKey), string(snapshot.MessageKey), snapshot.SessionKey, snapshot.BranchKey,
		snapshot.BranchOrdinal, string(snapshot.State), snapshot.Revision, snapshot.VisibleAtRevision,
		commandPayload, payload); err != nil {
		// The unique index on the branch position is the real arbiter when two
		// writers race for the same slot.
		return message.Snapshot{}, fmt.Errorf("%w: branch ordinal %d is already used", message.ErrOrdinalConflict, ordinal)
	}
	return snapshot, nil
}

// SaveSnapshot replaces the cumulative fields under revision control.
func (s *SQLiteMessageService) SaveSnapshot(ctx context.Context, command message.SaveCommand) (message.Snapshot, error) {
	return s.mutate(ctx, command.TenantKey, command.MessageKey, func(current message.Snapshot) (message.Snapshot, error) {
		return message.ApplySave(current, command, time.Now().UTC())
	})
}

// Tombstone redacts a message through the same guards a save uses.
func (s *SQLiteMessageService) Tombstone(ctx context.Context, command message.TombstoneCommand) (message.Snapshot, error) {
	return s.mutate(ctx, command.TenantKey, command.MessageKey, func(current message.Snapshot) (message.Snapshot, error) {
		return message.ApplyTombstone(current, command, time.Now().UTC())
	})
}

// mutate is the load, apply, store cycle. The whole cycle runs in one
// transaction so the revision the state machine compared against is the revision
// that is still stored when the write lands.
func (s *SQLiteMessageService) mutate(ctx context.Context, tenant agent.TenantKey, key message.MessageKey, apply func(message.Snapshot) (message.Snapshot, error)) (snapshot message.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return message.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	current, _, err := loadMessage(ctx, tx, tenant, key)
	if err != nil {
		return message.Snapshot{}, err
	}
	next, err := apply(current)
	if err != nil {
		return message.Snapshot{}, err
	}
	siblings, err := branchSiblings(ctx, tx, next)
	if err != nil {
		return message.Snapshot{}, err
	}
	if err = message.ValidateBranchCorrelation(next, siblings); err != nil {
		return message.Snapshot{}, err
	}
	if err = writeMessage(ctx, tx, next); err != nil {
		return message.Snapshot{}, err
	}
	return next, nil
}

// Get returns one message aggregate.
func (s *SQLiteMessageService) Get(ctx context.Context, query message.GetQuery) (snapshot message.Snapshot, err error) {
	if err := message.ValidateGetQuery(query); err != nil {
		return message.Snapshot{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return message.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	snapshot, _, err = loadMessage(ctx, tx, query.TenantKey, query.MessageKey)
	return snapshot, err
}

// ListBranch returns one branch in position order.
func (s *SQLiteMessageService) ListBranch(ctx context.Context, query message.ListBranchQuery) (snapshots []message.Snapshot, err error) {
	if err := message.ValidateListBranchQuery(query); err != nil {
		return nil, err
	}
	snapshots, err = s.query(ctx,
		`SELECT payload FROM message_aggregates WHERE tenant_key = ? AND session_key = ? AND branch_key = ? ORDER BY branch_ordinal, message_key`,
		string(query.TenantKey), query.SessionKey, query.BranchKey)
	if err != nil {
		return nil, err
	}
	message.SortBranch(snapshots)
	return snapshots, nil
}

// ListVisible returns the messages that are part of the conversation at one
// session revision, in the order they became visible.
func (s *SQLiteMessageService) ListVisible(ctx context.Context, query message.ListVisibleQuery) (snapshots []message.Snapshot, err error) {
	if err := message.ValidateListVisibleQuery(query); err != nil {
		return nil, err
	}
	snapshots, err = s.query(ctx,
		`SELECT payload FROM message_aggregates WHERE tenant_key = ? AND session_key = ? AND visible_at_revision > 0 AND visible_at_revision <= ? ORDER BY visible_at_revision, branch_key, branch_ordinal, message_key`,
		string(query.TenantKey), query.SessionKey, query.Revision)
	if err != nil {
		return nil, err
	}
	message.SortVisible(snapshots)
	return snapshots, nil
}

func (s *SQLiteMessageService) query(ctx context.Context, statement string, args ...any) (snapshots []message.Snapshot, err error) {
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	snapshots = make([]message.Snapshot, 0)
	for rows.Next() {
		var payload []byte
		if scanErr := rows.Scan(&payload); scanErr != nil {
			return nil, scanErr
		}
		var snapshot message.Snapshot
		if unmarshalErr := json.Unmarshal(payload, &snapshot); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func nextBranchOrdinal(ctx context.Context, tx *sql.Tx, command message.CreateCommand) (uint64, error) {
	var highest uint64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(branch_ordinal), 0) FROM message_aggregates WHERE tenant_key = ? AND session_key = ? AND branch_key = ?`,
		string(command.TenantKey), command.SessionKey, command.BranchKey).Scan(&highest)
	return highest + 1, err
}

func ordinalUsed(ctx context.Context, tx *sql.Tx, command message.CreateCommand, ordinal uint64) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_aggregates WHERE tenant_key = ? AND session_key = ? AND branch_key = ? AND branch_ordinal = ?`,
		string(command.TenantKey), command.SessionKey, command.BranchKey, ordinal).Scan(&count)
	return count > 0, err
}

// branchSiblings returns the other messages of the candidate's branch, which is
// the context the branch-correlation rule needs.
func branchSiblings(ctx context.Context, tx *sql.Tx, candidate message.Snapshot) (snapshots []message.Snapshot, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM message_aggregates WHERE tenant_key = ? AND session_key = ? AND branch_key = ? AND message_key != ?`,
		string(candidate.TenantKey), candidate.SessionKey, candidate.BranchKey, string(candidate.MessageKey))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var payload []byte
		if scanErr := rows.Scan(&payload); scanErr != nil {
			return nil, scanErr
		}
		var snapshot message.Snapshot
		if unmarshalErr := json.Unmarshal(payload, &snapshot); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}
