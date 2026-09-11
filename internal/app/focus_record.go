package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/movsar/tt/internal/store"
)

type FocusRecordPreview struct {
	ID     string            `json:"id"`
	Record store.FocusRecord `json:"record"`
	Upload string            `json:"upload"`
}

func (r *Resources) PrepareFocusRecord(ctx context.Context, record store.FocusRecord) (FocusRecordPreview, error) {
	if err := store.ValidateFocusRecord(record); err != nil {
		return FocusRecordPreview{}, err
	}
	record.Start, record.End = record.Start.UTC(), record.End.UTC()
	if record.TaskID != "" {
		if _, err := r.store.Task(ctx, record.TaskID); err != nil {
			return FocusRecordPreview{}, err
		}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return FocusRecordPreview{}, err
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if err := r.store.SetMeta(ctx, "focus_record_preview_"+id, string(raw)); err != nil {
		return FocusRecordPreview{}, err
	}
	return FocusRecordPreview{ID: id, Record: record, Upload: "existing focus uploader; repeated acceptance never creates a second local session"}, nil
}

func (r *Resources) ApplyFocusRecord(ctx context.Context, id string) (store.TimerSession, error) {
	if len(id) != 64 {
		return store.TimerSession{}, errors.New("invalid focus preview ID")
	}
	text, exists, err := r.store.Meta(ctx, "focus_record_preview_"+id)
	if err != nil {
		return store.TimerSession{}, err
	}
	if !exists {
		return store.TimerSession{}, errors.New("focus preview is unavailable")
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != id {
		return store.TimerSession{}, errors.New("focus preview failed validation")
	}
	var record store.FocusRecord
	if err := decodeResourceInput(json.RawMessage(text), &record); err != nil {
		return store.TimerSession{}, err
	}
	return r.store.ImportFocusRecord(ctx, id, record)
}
