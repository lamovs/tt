package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type FocusTopic struct {
	ID, Name, Type, CredentialFingerprint string
	Status                                int
	SortOrder                             int64
	PomodoroTime                          int
	Raw                                   json.RawMessage
	RefreshedAt                           time.Time
}

func (s *Store) ReplaceFocusTopics(ctx context.Context, topics []FocusTopic, fingerprint string, now time.Time) error {
	if fingerprint == "" || now.IsZero() {
		return errors.New("focus topic snapshot lacks credential identity or time")
	}
	seen := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if strings.TrimSpace(topic.ID) == "" || strings.TrimSpace(topic.Name) == "" || !utf8.ValidString(topic.ID) || !utf8.ValidString(topic.Name) || utf8.RuneCountInString(topic.ID) > 500 || utf8.RuneCountInString(topic.Name) > 500 || strings.ContainsFunc(topic.ID, unicode.IsControl) || strings.ContainsFunc(topic.Name, unicode.IsControl) || seen[topic.ID] || !json.Valid(topic.Raw) {
			return errors.New("focus topic snapshot has a missing, duplicate or invalid topic")
		}
		seen[topic.ID] = true
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM focus_topics"); err != nil {
			return err
		}
		for _, topic := range topics {
			if _, err := tx.ExecContext(ctx, `INSERT INTO focus_topics
				(id,name,type,status,sort_order,pomodoro_time,raw,credential_fingerprint,refreshed_at)
				VALUES (?,?,?,?,?,?,?,?,?)`, topic.ID, topic.Name, topic.Type, topic.Status, topic.SortOrder,
				topic.PomodoroTime, string(topic.Raw), fingerprint, now.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) FocusTopics(ctx context.Context) ([]FocusTopic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,type,status,sort_order,pomodoro_time,raw,credential_fingerprint,refreshed_at
		FROM focus_topics ORDER BY status, sort_order DESC, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var topics []FocusTopic
	for rows.Next() {
		var topic FocusTopic
		var raw string
		var refreshed int64
		if err := rows.Scan(&topic.ID, &topic.Name, &topic.Type, &topic.Status, &topic.SortOrder, &topic.PomodoroTime, &raw, &topic.CredentialFingerprint, &refreshed); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(raw)) || topic.ID == "" || topic.Name == "" || topic.CredentialFingerprint == "" {
			return nil, errors.New("cached focus topic is invalid")
		}
		topic.Raw = json.RawMessage(raw)
		topic.RefreshedAt = time.Unix(refreshed, 0).UTC()
		topics = append(topics, topic)
	}
	return topics, rows.Err()
}

func (s *Store) ResolveFocusTopic(ctx context.Context, reference string) (FocusTopic, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return FocusTopic{}, errors.New("name a Timer topic")
	}
	topics, err := s.FocusTopics(ctx)
	if err != nil {
		return FocusTopic{}, err
	}
	var exactID []FocusTopic
	var exactName []FocusTopic
	for _, topic := range topics {
		if topic.ID == reference {
			exactID = append(exactID, topic)
		}
		if strings.EqualFold(topic.Name, reference) {
			exactName = append(exactName, topic)
		}
	}
	if len(exactID) == 1 {
		return exactID[0], nil
	}
	if len(exactName) == 1 {
		return exactName[0], nil
	}
	if len(exactName) > 1 {
		ids := make([]string, 0, len(exactName))
		for _, topic := range exactName {
			ids = append(ids, topic.ID)
		}
		sort.Strings(ids)
		return FocusTopic{}, fmt.Errorf("Timer topic name is ambiguous; use one exact ID: %s", strings.Join(ids, ", "))
	}
	return FocusTopic{}, fmt.Errorf("no cached Timer topic matches %q; run tt timer topic ls --remote", reference)
}
