package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

func (s *Store) RebindCredential(ctx context.Context, expected AuthBinding, fingerprint string) error {
	if !validCredentialFingerprint(fingerprint) || expected.Fingerprint == "" {
		return errors.New("credential recovery needs an existing binding and a valid replacement")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		binding, exists, err := readAuthBinding(ctx, tx)
		if err != nil {
			return err
		}
		if !exists || binding != expected {
			return ErrCredentialGeneration
		}
		if binding.Generation == ^uint64(0) {
			return errors.New("credential generation exhausted")
		}
		binding.Fingerprint, binding.SignedOut = fingerprint, true
		binding.Generation++
		raw, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		return setMeta(ctx, tx, authBindingKey, string(raw))
	})
}

const browserContinuityKey = "browser_credential_continuity"

type browserContinuity struct {
	Current  string   `json:"current"`
	Accepted []string `json:"accepted"`
}

func readBrowserContinuity(ctx context.Context, q execer) (browserContinuity, error) {
	var result browserContinuity
	raw, exists, err := getMeta(ctx, q, browserContinuityKey)
	if err != nil || !exists {
		return result, err
	}
	if json.Unmarshal([]byte(raw), &result) != nil || !validCredentialFingerprint(result.Current) || !slices.Contains(result.Accepted, result.Current) {
		return result, errors.New("invalid Browser credential continuity")
	}
	for _, fingerprint := range result.Accepted {
		if !validCredentialFingerprint(fingerprint) {
			return result, errors.New("invalid Browser credential continuity")
		}
	}
	return result, nil
}

func (s *Store) RecoverBrowserCredential(ctx context.Context, previous, next string) error {
	if !validCredentialFingerprint(next) || previous != "" && !validCredentialFingerprint(previous) {
		return errors.New("invalid Browser credential fingerprint")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		binding, err := readBrowserContinuity(ctx, tx)
		if err != nil {
			return err
		}
		if binding.Current != "" && previous != "" && binding.Current != previous && binding.Current != next && !slices.Contains(binding.Accepted, previous) {
			return ErrCredentialGeneration
		}
		for _, fingerprint := range []string{previous, next} {
			if fingerprint != "" && !slices.Contains(binding.Accepted, fingerprint) {
				binding.Accepted = append(binding.Accepted, fingerprint)
			}
		}
		binding.Current = next
		raw, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		return setMeta(ctx, tx, browserContinuityKey, string(raw))
	})
}

func (s *Store) BrowserCredentialAllows(ctx context.Context, frozen, current string) (bool, error) {
	if frozen == current && frozen != "" {
		return true, nil
	}
	binding, err := readBrowserContinuity(ctx, s.db)
	return err == nil && binding.Current == current && slices.Contains(binding.Accepted, frozen), err
}
