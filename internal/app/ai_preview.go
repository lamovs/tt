package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/store"
)

const aiPreviewPrefix = "ai_preview_"

// The texts stay short enough to fit one line after the "tt: ai: " they are
// printed behind.
var (
	ErrAIPreviewUnavailable = errors.New("preview is unavailable: never saved, already accepted, or expired")
	ErrAIPreviewInvalid     = errors.New("preview failed validation")
	ErrAIPreviewExpired     = errors.New("preview has expired, and its dates would be stale; run tt ai again")
)

// SaveAIPreview stores plan under the hex SHA-256 of its JSON and returns that
// id. Previews older than AIPlanLifetime are removed first.
func SaveAIPreview(ctx context.Context, st *store.Store, plan AIPlan, now time.Time) (string, error) {
	if err := PruneAIPreviews(ctx, st, now); err != nil {
		return "", err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	return id, st.SetMeta(ctx, aiPreviewPrefix+id, string(raw))
}

// ClaimAIPreview takes a saved preview out of the cache and returns its plan,
// so one preview is applied at most once. The raw text lets a caller put it
// back with RestoreAIPreview when applying failed and changed nothing. Every
// claim removes the previews older than AIPlanLifetime, the one asked for
// included, which is still reported as expired.
func ClaimAIPreview(ctx context.Context, st *store.Store, id string, now time.Time) (AIPlan, string, error) {
	if len(id) != sha256.Size*2 {
		return AIPlan{}, "", ErrAIPreviewInvalid
	}
	if _, err := hex.DecodeString(id); err != nil {
		return AIPlan{}, "", ErrAIPreviewInvalid
	}
	text, exists, err := st.Meta(ctx, aiPreviewPrefix+id)
	if err != nil {
		return AIPlan{}, "", err
	}
	if err := PruneAIPreviews(ctx, st, now); err != nil {
		return AIPlan{}, "", err
	}
	if !exists {
		return AIPlan{}, "", ErrAIPreviewUnavailable
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != id {
		return AIPlan{}, "", ErrAIPreviewInvalid
	}
	var plan AIPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil || plan.Version != aiPlanVersion {
		return AIPlan{}, "", ErrAIPreviewInvalid
	}
	if now.Sub(plan.Created) > AIPlanLifetime || plan.Created.After(now.Add(time.Minute)) {
		return AIPlan{}, "", ErrAIPreviewExpired
	}
	res, err := st.DB().ExecContext(ctx, `DELETE FROM meta WHERE key = ? AND value = ?`, aiPreviewPrefix+id, text)
	if err != nil {
		return AIPlan{}, "", fmt.Errorf("claim preview: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return AIPlan{}, "", ErrAIPreviewUnavailable
	}
	return plan, text, nil
}

func RestoreAIPreview(ctx context.Context, st *store.Store, id, text string) error {
	return st.SetMeta(ctx, aiPreviewPrefix+id, text)
}

// PruneAIPreviews removes the saved previews older than AIPlanLifetime, and
// any that no longer reads as one. A preview holds copies of the tasks it
// changes, notes included, so it is not kept past the time it can be used.
func PruneAIPreviews(ctx context.Context, st *store.Store, now time.Time) error {
	rows, err := st.DB().QueryContext(ctx, `SELECT key, value FROM meta WHERE key GLOB 'ai_preview_*'`)
	if err != nil {
		return fmt.Errorf("read previews: %w", err)
	}
	var stale []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return fmt.Errorf("read previews: %w", err)
		}
		var plan struct {
			Created time.Time `json:"created"`
		}
		if json.Unmarshal([]byte(value), &plan) != nil || now.Sub(plan.Created) > AIPlanLifetime {
			stale = append(stale, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read previews: %w", err)
	}
	rows.Close()
	for _, key := range stale {
		if _, err := st.DB().ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key); err != nil {
			return fmt.Errorf("remove expired preview: %w", err)
		}
	}
	return nil
}
