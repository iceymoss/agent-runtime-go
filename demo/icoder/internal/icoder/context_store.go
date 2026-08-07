package icoder

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

type contextPlanStore struct{ store *Store }
type contextArtifactStore struct{ store *Store }

func (s contextPlanStore) CreateOrVerify(ctx context.Context, plan agentcontext.Plan) (agentcontext.Plan, error) {
	payload, err := plan.MarshalWire()
	if err != nil {
		return agentcontext.Plan{}, err
	}
	ref := plan.Ref()
	_, err = s.store.db.ExecContext(ctx, `INSERT INTO icoder_context_plans(tenant_key, plan_key, plan_digest, payload, created_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(tenant_key, plan_key) DO NOTHING`, ref.TenantKey, ref.PlanKey, ref.PlanDigest, payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return agentcontext.Plan{}, err
	}
	stored, err := s.Get(ctx, ref)
	if err != nil {
		return agentcontext.Plan{}, err
	}
	storedPayload, _ := stored.MarshalWire()
	if !bytes.Equal(payload, storedPayload) {
		return agentcontext.Plan{}, agentcontext.ErrArtifactConflict
	}
	return stored, nil
}

func (s contextPlanStore) Get(ctx context.Context, ref agentcontext.PlanRef) (agentcontext.Plan, error) {
	var digest string
	var payload []byte
	err := s.store.db.QueryRowContext(ctx, `SELECT plan_digest, payload FROM icoder_context_plans WHERE tenant_key = ? AND plan_key = ?`, ref.TenantKey, ref.PlanKey).Scan(&digest, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return agentcontext.Plan{}, agentcontext.ErrPlanNotFound
	}
	if err != nil {
		return agentcontext.Plan{}, err
	}
	if digest != ref.PlanDigest {
		return agentcontext.Plan{}, agentcontext.ErrPlanDrift
	}
	plan, err := agentcontext.UnmarshalPlan(payload)
	if err != nil {
		return agentcontext.Plan{}, err
	}
	if plan.Ref() != ref {
		return agentcontext.Plan{}, agentcontext.ErrPlanDrift
	}
	return plan, nil
}

func (s contextArtifactStore) CreateOrVerify(ctx context.Context, artifact agentcontext.SummaryArtifact) (agentcontext.SummaryArtifact, error) {
	payload, err := artifact.MarshalWire()
	if err != nil {
		return agentcontext.SummaryArtifact{}, err
	}
	ref := artifact.Ref()
	_, err = s.store.db.ExecContext(ctx, `INSERT INTO icoder_context_artifacts(tenant_key, artifact_key, artifact_digest, payload, created_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(tenant_key, artifact_key) DO NOTHING`, artifact.Source().TenantKey, ref.Key, ref.Digest, payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return agentcontext.SummaryArtifact{}, err
	}
	stored, err := s.Get(ctx, artifact.Source().TenantKey, ref)
	if err != nil {
		return agentcontext.SummaryArtifact{}, err
	}
	storedPayload, _ := stored.MarshalWire()
	if !bytes.Equal(payload, storedPayload) {
		return agentcontext.SummaryArtifact{}, agentcontext.ErrArtifactConflict
	}
	return stored, nil
}

func (s contextArtifactStore) Get(ctx context.Context, tenant agent.TenantKey, ref agentcontext.ArtifactRef) (agentcontext.SummaryArtifact, error) {
	var digest string
	var payload []byte
	err := s.store.db.QueryRowContext(ctx, `SELECT artifact_digest, payload FROM icoder_context_artifacts WHERE tenant_key = ? AND artifact_key = ?`, tenant, ref.Key).Scan(&digest, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return agentcontext.SummaryArtifact{}, agentcontext.ErrArtifactNotFound
	}
	if err != nil {
		return agentcontext.SummaryArtifact{}, err
	}
	if digest != ref.Digest {
		return agentcontext.SummaryArtifact{}, agentcontext.ErrArtifactConflict
	}
	artifact, err := agentcontext.UnmarshalSummaryArtifact(payload)
	if err != nil {
		return agentcontext.SummaryArtifact{}, err
	}
	if artifact.Ref() != ref || artifact.Source().TenantKey != tenant {
		return agentcontext.SummaryArtifact{}, agentcontext.ErrArtifactConflict
	}
	return artifact, nil
}

func (s *Store) SavePivot(ctx context.Context, snapshot SessionSnapshot, pivot agentcontext.PivotRef) error {
	payload, err := marshalString(pivot)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision uint64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM icoder_sessions WHERE id = ?`, snapshot.ID).Scan(&revision); err != nil {
		return err
	}
	if revision != snapshot.Revision {
		return fmt.Errorf("session revision conflict")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO icoder_session_context(session_id, pivot_payload, updated_at) VALUES(?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET pivot_payload = excluded.pivot_payload, updated_at = excluded.updated_at`, snapshot.ID, payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MessagesAfterRevision(ctx context.Context, sessionID string, revision uint64) ([]agent.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM icoder_messages WHERE session_id = ? AND turn_revision > ? ORDER BY ordinal`, sessionID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []agent.Message
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var message agent.Message
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}
