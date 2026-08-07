package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
	"github.com/iceymoss/agent-runtime-go/event"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

type SessionSnapshot struct {
	ID       string
	Revision uint64
	Usage    agent.Usage
	Pivot    *agentcontext.PivotRef
}

type SessionInfo struct {
	ID        string
	Revision  uint64
	Messages  int
	UpdatedAt string
}

type TaskState struct {
	Goal         string            `json:"goal"`
	Status       string            `json:"status"`
	ChangedFiles []string          `json:"changed_files"`
	Checks       []ValidationCheck `json:"checks"`
	Verification string            `json:"verification"`
	OpenIssues   []string          `json:"open_issues"`
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=10000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		closeErr := db.Close()
		return nil, errors.Join(err, closeErr)
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS icoder_sessions (
            id TEXT PRIMARY KEY,
            revision INTEGER NOT NULL DEFAULT 0,
            prompt_tokens INTEGER NOT NULL DEFAULT 0,
            completion_tokens INTEGER NOT NULL DEFAULT 0,
            total_tokens INTEGER NOT NULL DEFAULT 0,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS icoder_messages (
            session_id TEXT NOT NULL,
            ordinal INTEGER NOT NULL,
			turn_revision INTEGER NOT NULL DEFAULT 0,
            payload TEXT NOT NULL,
            created_at TEXT NOT NULL,
            PRIMARY KEY (session_id, ordinal),
            FOREIGN KEY (session_id) REFERENCES icoder_sessions(id)
        )`,
		`CREATE TABLE IF NOT EXISTS icoder_turns (
            session_id TEXT NOT NULL,
            request_id TEXT NOT NULL,
            input_digest TEXT NOT NULL,
            result_payload TEXT NOT NULL,
            PRIMARY KEY (session_id, request_id),
            FOREIGN KEY (session_id) REFERENCES icoder_sessions(id)
        )`,
		`CREATE TABLE IF NOT EXISTS icoder_events (
            session_id TEXT NOT NULL,
            sequence INTEGER NOT NULL,
            event_id TEXT NOT NULL UNIQUE,
            event_type TEXT NOT NULL,
            payload TEXT NOT NULL,
            occurred_at TEXT NOT NULL,
            PRIMARY KEY (session_id, sequence),
            FOREIGN KEY (session_id) REFERENCES icoder_sessions(id)
        )`,
		`CREATE TABLE IF NOT EXISTS icoder_context_plans (
			tenant_key TEXT NOT NULL,
			plan_key TEXT NOT NULL,
			plan_digest TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (tenant_key, plan_key)
		)`,
		`CREATE TABLE IF NOT EXISTS icoder_context_artifacts (
			tenant_key TEXT NOT NULL,
			artifact_key TEXT NOT NULL,
			artifact_digest TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (tenant_key, artifact_key)
		)`,
		`CREATE TABLE IF NOT EXISTS icoder_session_context (
			session_id TEXT PRIMARY KEY,
			pivot_payload BLOB NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES icoder_sessions(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS icoder_task_state (
			session_id TEXT PRIMARY KEY,
			payload BLOB NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES icoder_sessions(id) ON DELETE CASCADE
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate icoder store: %w", err)
		}
	}
	if err := s.ensureMessageTurnRevision(); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureMessageTurnRevision() error {
	rows, err := s.db.Query(`PRAGMA table_info(icoder_messages)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		found = found || name == "turn_revision"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE icoder_messages ADD COLUMN turn_revision INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return s.backfillMessageTurnRevisions()
}

func (s *Store) backfillMessageTurnRevisions() error {
	rows, err := s.db.Query(`SELECT session_id, ordinal, payload FROM icoder_messages ORDER BY session_id, ordinal`)
	if err != nil {
		return err
	}
	type update struct {
		session           string
		ordinal, revision uint64
	}
	var updates []update
	var session string
	var revision uint64
	for rows.Next() {
		var current, payload string
		var ordinal uint64
		if err := rows.Scan(&current, &ordinal, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		if current != session {
			session, revision = current, 0
		}
		var message agent.Message
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			_ = rows.Close()
			return err
		}
		if message.Role == agent.RoleUser {
			revision++
		}
		updates = append(updates, update{current, ordinal, revision})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range updates {
		if _, err := s.db.Exec(`UPDATE icoder_messages SET turn_revision = ? WHERE session_id = ? AND ordinal = ?`, value.revision, value.session, value.ordinal); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Load(ctx context.Context, sessionID string) (snapshot SessionSnapshot, messages []agent.Message, resultErr error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO icoder_sessions(id, created_at, updated_at) VALUES(?, ?, ?) ON CONFLICT(id) DO NOTHING`, sessionID, now, now); err != nil {
		return SessionSnapshot{}, nil, err
	}
	snapshot.ID = sessionID
	if err := s.db.QueryRowContext(ctx, `SELECT revision, prompt_tokens, completion_tokens, total_tokens FROM icoder_sessions WHERE id = ?`, sessionID).Scan(&snapshot.Revision, &snapshot.Usage.PromptTokens, &snapshot.Usage.CompletionTokens, &snapshot.Usage.TotalTokens); err != nil {
		return SessionSnapshot{}, nil, err
	}
	var pivotPayload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT pivot_payload FROM icoder_session_context WHERE session_id = ?`, sessionID).Scan(&pivotPayload); err == nil {
		var pivot agentcontext.PivotRef
		if err := json.Unmarshal(pivotPayload, &pivot); err != nil {
			return SessionSnapshot{}, nil, fmt.Errorf("decode context pivot: %w", err)
		}
		snapshot.Pivot = &pivot
	} else if !errors.Is(err, sql.ErrNoRows) {
		return SessionSnapshot{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM icoder_messages WHERE session_id = ? ORDER BY ordinal`, sessionID)
	if err != nil {
		return SessionSnapshot{}, nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return SessionSnapshot{}, nil, err
		}
		var message agent.Message
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			return SessionSnapshot{}, nil, fmt.Errorf("decode stored message: %w", err)
		}
		messages = append(messages, message)
	}
	return snapshot, messages, rows.Err()
}

func (s *Store) CommitTurn(ctx context.Context, snapshot SessionSnapshot, requestID, inputDigest string, user agent.Message, result agent.RunResult) (resultErr error) {
	resultPayload, err := marshalString(result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, rollbackErr)
		}
	}()
	var existingDigest, existingResult string
	err = tx.QueryRowContext(ctx, `SELECT input_digest, result_payload FROM icoder_turns WHERE session_id = ? AND request_id = ?`, snapshot.ID, requestID).Scan(&existingDigest, &existingResult)
	if err == nil {
		if existingDigest != inputDigest || existingResult != resultPayload {
			return fmt.Errorf("request idempotency conflict")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var nextOrdinal uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal) + 1, 0) FROM icoder_messages WHERE session_id = ?`, snapshot.ID).Scan(&nextOrdinal); err != nil {
		return err
	}
	messages := append([]agent.Message{user}, result.Messages...)
	for _, message := range messages {
		payload, marshalErr := marshalString(message)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_messages(session_id, ordinal, turn_revision, payload, created_at) VALUES(?, ?, ?, ?, ?)`, snapshot.ID, nextOrdinal, snapshot.Revision+1, payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		nextOrdinal++
	}
	update, err := tx.ExecContext(ctx, `UPDATE icoder_sessions SET revision = revision + 1, prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?, total_tokens = total_tokens + ?, updated_at = ? WHERE id = ? AND revision = ?`, result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens, time.Now().UTC().Format(time.RFC3339Nano), snapshot.ID, snapshot.Revision)
	if err != nil {
		return err
	}
	affected, err := update.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf("session revision conflict")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_turns(session_id, request_id, input_digest, result_payload) VALUES(?, ?, ?, ?)`, snapshot.ID, requestID, inputDigest, resultPayload); err != nil {
		return err
	}
	payload, err := marshalString(map[string]any{"outcome": result.Outcome, "stop_reason": result.StopReason, "usage": result.Usage, "summary": summarizeRun(result.Messages)})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_events(session_id, sequence, event_id, event_type, payload, occurred_at) VALUES(?, ?, ?, ?, ?, ?)`, snapshot.ID, snapshot.Revision+1, requestID+":terminal", "agent.run.terminal", payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	summary := summarizeRun(result.Messages)
	status := "completed"
	if summary.Verification == "unverified" {
		status = "needs_validation"
	}
	taskPayload, err := marshalString(TaskState{Goal: user.Text(), Status: status, ChangedFiles: summary.ChangedFiles, Checks: summary.Checks, Verification: summary.Verification, OpenIssues: []string{}})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_task_state(session_id, payload, updated_at) VALUES(?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`, snapshot.ID, taskPayload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadTaskState(ctx context.Context, sessionID string) (*TaskState, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM icoder_task_state WHERE session_id = ?`, sessionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state TaskState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Store) ReplayEvents(ctx context.Context, sessionID string, after uint64, limit int) (events []event.Envelope, resultErr error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, event_id, event_type, payload, occurred_at FROM icoder_events WHERE session_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	for rows.Next() {
		var envelope event.Envelope
		var occurredAt string
		envelope.TenantKey, envelope.StreamKey, envelope.AggregateType, envelope.AggregateKey = "local", "session/"+sessionID, "session", sessionID
		envelope.SchemaVersion, envelope.Reliability = 1, event.ReliabilityTerminal
		if err := rows.Scan(&envelope.Sequence, &envelope.EventID, &envelope.Type, &envelope.Payload, &occurredAt); err != nil {
			return nil, err
		}
		envelope.AggregateRevision = envelope.Sequence
		envelope.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, err
		}
		events = append(events, envelope)
	}
	return events, rows.Err()
}

func (s *Store) ListSessions(ctx context.Context) (sessions []SessionInfo, resultErr error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.revision, COUNT(m.ordinal), s.updated_at FROM icoder_sessions s LEFT JOIN icoder_messages m ON m.session_id = s.id GROUP BY s.id, s.revision, s.updated_at ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var info SessionInfo
		if err := rows.Scan(&info.ID, &info.Revision, &info.Messages, &info.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, info)
	}
	return sessions, rows.Err()
}

func (s *Store) ClearSession(ctx context.Context, sessionID string) (resultErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, rollbackErr)
		}
	}()
	for _, statement := range []string{
		`DELETE FROM icoder_task_state WHERE session_id = ?`,
		`DELETE FROM icoder_session_context WHERE session_id = ?`,
		`DELETE FROM icoder_events WHERE session_id = ?`,
		`DELETE FROM icoder_turns WHERE session_id = ?`,
		`DELETE FROM icoder_messages WHERE session_id = ?`,
		`DELETE FROM icoder_sessions WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, sessionID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
