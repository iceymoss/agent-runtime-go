package icoder

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/demo/icoder/internal/jsoncodec"
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
}

type SessionInfo struct {
	ID        string
	Revision  uint64
	Messages  int
	UpdatedAt string
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
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate icoder store: %w", err)
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
		if err := jsoncodec.Unmarshal([]byte(payload), &message); err != nil {
			return SessionSnapshot{}, nil, fmt.Errorf("decode stored message: %w", err)
		}
		messages = append(messages, message)
	}
	return snapshot, messages, rows.Err()
}

func (s *Store) CommitTurn(ctx context.Context, snapshot SessionSnapshot, requestID, inputDigest string, user agent.Message, result agent.RunResult) (resultErr error) {
	resultPayload, err := jsoncodec.MarshalString(result)
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
		payload, marshalErr := jsoncodec.MarshalString(message)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_messages(session_id, ordinal, payload, created_at) VALUES(?, ?, ?, ?)`, snapshot.ID, nextOrdinal, payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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
	payload, err := jsoncodec.MarshalString(map[string]any{"outcome": result.Outcome, "stop_reason": result.StopReason, "usage": result.Usage})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO icoder_events(session_id, sequence, event_id, event_type, payload, occurred_at) VALUES(?, ?, ?, ?, ?, ?)`, snapshot.ID, snapshot.Revision+1, requestID+":terminal", "agent.run.terminal", payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
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
