package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
)

// SQLiteManifestStore persists the wire manifests that identify runtime
// generations.
//
// Only durable values are stored. A manifest names the model, prompt, tools, and
// policy a run used; it never contains the executable definition, because an
// executable cannot be serialized and pretending otherwise would let a stale
// record masquerade as a working runtime.
type SQLiteManifestStore struct{ db *sql.DB }

// NewSQLiteManifestStore binds the manifest store to an already-migrated database.
func NewSQLiteManifestStore(db *sql.DB) *SQLiteManifestStore { return &SQLiteManifestStore{db: db} }

var _ coordinator.ManifestStore = (*SQLiteManifestStore)(nil)

// ManifestStore returns the generation store backed by this store's database.
func (s *Store) ManifestStore() *SQLiteManifestStore { return NewSQLiteManifestStore(s.db) }

var manifestMigrations = []string{
	`CREATE TABLE IF NOT EXISTS runtime_manifests (
		tenant_key TEXT NOT NULL,
		definition_digest TEXT NOT NULL,
		manifest_digest TEXT NOT NULL,
		recipe_key TEXT NOT NULL,
		recipe_version TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		payload BLOB NOT NULL,
		PRIMARY KEY (tenant_key, definition_digest)
	)`,
	`CREATE INDEX IF NOT EXISTS runtime_manifests_recent ON runtime_manifests(tenant_key, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS runtime_manifest_pins (
		tenant_key TEXT NOT NULL,
		definition_digest TEXT NOT NULL,
		pin_key TEXT NOT NULL,
		owner_key TEXT NOT NULL,
		retain_until INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_key, definition_digest, pin_key)
	)`,
}

// SaveGeneration records one generation. Re-saving the identical manifest is a
// no-op; the same definition digest with different contents is a conflict,
// because a definition digest is supposed to pin exactly one composition.
func (s *SQLiteManifestStore) SaveGeneration(ctx context.Context, generation coordinator.StoredGeneration, pin coordinator.RetentionPin) (err error) {
	manifest := generation.Manifest
	if err := coordinator.ValidateArtifactManifest(manifest); err != nil {
		return err
	}
	// Pins arrive through the pin argument only. Accepting them inside the stored
	// value as well would give one save two places to disagree about what is
	// being retained.
	if len(generation.Pins) != 0 {
		return fmt.Errorf("%w: pins must be supplied through the pin argument", coordinator.ErrInvariantConflict)
	}
	if pin.PinKey == "" && (pin.OwnerKey != "" || !pin.RetainUntil.IsZero()) {
		return fmt.Errorf("%w: partial retention pin", coordinator.ErrInvariantConflict)
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &err)
	var existing string
	loadErr := tx.QueryRowContext(ctx, `SELECT manifest_digest FROM runtime_manifests WHERE tenant_key = ? AND definition_digest = ?`,
		string(manifest.TenantKey), manifest.Wire.DefinitionDigest).Scan(&existing)
	switch {
	case loadErr == nil:
		if existing != manifest.ManifestDigest {
			return fmt.Errorf("%w: definition digest %q already names a different manifest", coordinator.ErrInvariantConflict, manifest.Wire.DefinitionDigest)
		}
	case errors.Is(loadErr, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_manifests(tenant_key, definition_digest, manifest_digest, recipe_key, recipe_version, created_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			string(manifest.TenantKey), manifest.Wire.DefinitionDigest, manifest.ManifestDigest,
			manifest.Wire.RecipeKey, manifest.Wire.RecipeVersion, nanos(manifest.CreatedAt), payload); err != nil {
			return err
		}
	default:
		return loadErr
	}
	if pin.PinKey != "" {
		if err = savePin(ctx, tx, manifest.TenantKey, manifest.Wire.DefinitionDigest, pin); err != nil {
			return err
		}
	}
	return nil
}

// savePin creates or verifies one retention claim.
//
// A pin records who is retaining a generation, so an owner is required, and a
// second owner may not take over an existing key: that would silently transfer
// responsibility for keeping a run reproducible.
func savePin(ctx context.Context, tx *sql.Tx, tenant agent.TenantKey, digestValue string, pin coordinator.RetentionPin) error {
	if pin.PinKey == "" || pin.OwnerKey == "" {
		return fmt.Errorf("%w: pin key and owner key are required", coordinator.ErrInvariantConflict)
	}
	var owner string
	var retainUntil int64
	err := tx.QueryRowContext(ctx, `SELECT owner_key, retain_until FROM runtime_manifest_pins WHERE tenant_key = ? AND definition_digest = ? AND pin_key = ?`,
		string(tenant), digestValue, pin.PinKey).Scan(&owner, &retainUntil)
	switch {
	case err == nil:
		if owner != pin.OwnerKey || !fromNanos(retainUntil).Equal(pin.RetainUntil) {
			return fmt.Errorf("%w: pin key has different owner or retention horizon", coordinator.ErrInvariantConflict)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO runtime_manifest_pins(tenant_key, definition_digest, pin_key, owner_key, retain_until) VALUES(?, ?, ?, ?, ?)`,
			string(tenant), digestValue, pin.PinKey, pin.OwnerKey, nanos(pin.RetainUntil))
		return err
	default:
		return err
	}
}

// ResolveGeneration returns exactly the requested generation for exactly the
// requested tenant. It never falls back to another tenant or to the newest
// generation, because resuming under a different composition than the one a run
// started with is precisely what this store exists to prevent.
func (s *SQLiteManifestStore) ResolveGeneration(ctx context.Context, scope provider.Scope, definitionDigest string) (stored coordinator.StoredGeneration, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coordinator.StoredGeneration{}, err
	}
	defer finishTx(tx, &err)
	var payload []byte
	loadErr := tx.QueryRowContext(ctx, `SELECT payload FROM runtime_manifests WHERE tenant_key = ? AND definition_digest = ?`,
		string(scope.TenantKey), definitionDigest).Scan(&payload)
	if errors.Is(loadErr, sql.ErrNoRows) {
		return coordinator.StoredGeneration{}, fmt.Errorf("%w: generation %q", coordinator.ErrGenerationUnavailable, definitionDigest)
	}
	if loadErr != nil {
		return coordinator.StoredGeneration{}, loadErr
	}
	var manifest coordinator.ArtifactManifest
	if err = json.Unmarshal(payload, &manifest); err != nil {
		return coordinator.StoredGeneration{}, err
	}
	pins, err := loadPins(ctx, tx, scope.TenantKey, definitionDigest)
	if err != nil {
		return coordinator.StoredGeneration{}, err
	}
	return coordinator.StoredGeneration{Manifest: manifest, Pins: pins}, nil
}

// PinGeneration marks a generation as retained so cleanup cannot remove it while
// something still depends on being able to reconstruct it.
func (s *SQLiteManifestStore) PinGeneration(ctx context.Context, scope provider.Scope, definitionDigest string, pin coordinator.RetentionPin) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &err)
	var exists string
	if err = tx.QueryRowContext(ctx, `SELECT definition_digest FROM runtime_manifests WHERE tenant_key = ? AND definition_digest = ?`,
		string(scope.TenantKey), definitionDigest).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: generation %q", coordinator.ErrGenerationUnavailable, definitionDigest)
		}
		return err
	}
	return savePin(ctx, tx, scope.TenantKey, definitionDigest, pin)
}

// ReleasePin removes one retention claim. Releasing a pin that is not there
// reaches the same end state, so a retried release succeeds; releasing against a
// generation that does not exist does not.
func (s *SQLiteManifestStore) ReleasePin(ctx context.Context, scope provider.Scope, definitionDigest, pinKey string) (err error) {
	if !scope.TenantKey.Valid() || definitionDigest == "" || pinKey == "" {
		return fmt.Errorf("%w: generation %q", coordinator.ErrGenerationUnavailable, definitionDigest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &err)
	var exists string
	if err = tx.QueryRowContext(ctx, `SELECT definition_digest FROM runtime_manifests WHERE tenant_key = ? AND definition_digest = ?`,
		string(scope.TenantKey), definitionDigest).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: generation %q", coordinator.ErrGenerationUnavailable, definitionDigest)
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM runtime_manifest_pins WHERE tenant_key = ? AND definition_digest = ? AND pin_key = ?`,
		string(scope.TenantKey), definitionDigest, pinKey)
	return err
}

// ListGenerations returns the recorded generations, newest first, so an operator
// can see which compositions this workspace has actually run.
func (s *SQLiteManifestStore) ListGenerations(ctx context.Context, scope provider.Scope, limit int) (manifests []coordinator.ArtifactManifest, err error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM runtime_manifests WHERE tenant_key = ? ORDER BY created_at DESC LIMIT ?`, string(scope.TenantKey), limit)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var payload []byte
		if scanErr := rows.Scan(&payload); scanErr != nil {
			return nil, scanErr
		}
		var manifest coordinator.ArtifactManifest
		if unmarshalErr := json.Unmarshal(payload, &manifest); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		manifests = append(manifests, manifest)
	}
	return manifests, rows.Err()
}

func loadPins(ctx context.Context, tx *sql.Tx, tenant agent.TenantKey, definitionDigest string) (pins []coordinator.RetentionPin, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT pin_key, owner_key, retain_until FROM runtime_manifest_pins WHERE tenant_key = ? AND definition_digest = ? ORDER BY pin_key`,
		string(tenant), definitionDigest)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var pin coordinator.RetentionPin
		var retainUntil int64
		if scanErr := rows.Scan(&pin.PinKey, &pin.OwnerKey, &retainUntil); scanErr != nil {
			return nil, scanErr
		}
		pin.RetainUntil = fromNanos(retainUntil)
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}
