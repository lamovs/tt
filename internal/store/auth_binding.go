package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const authBindingKey = "auth_binding"

var (
	ErrCredentialChanged     = errors.New("credential differs from this cache binding; use a separate XDG_DATA_HOME")
	ErrSignedOut             = errors.New("this cache is signed out; run tt login to resume with the same credential")
	ErrCredentialGeneration  = errors.New("credential generation changed; start the operation again")
	ErrUnboundCredentialData = errors.New("this legacy cache has data but no saved credential proves its binding; use a separate XDG_DATA_HOME")
)

// AuthBinding pins credential continuity, not an account identity.
type AuthBinding struct {
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
	SignedOut   bool   `json:"signed_out"`
}

func readAuthBinding(ctx context.Context, q execer) (AuthBinding, bool, error) {
	raw, ok, err := getMeta(ctx, q, authBindingKey)
	if err != nil || !ok {
		return AuthBinding{}, ok, err
	}
	var binding AuthBinding
	if json.Unmarshal([]byte(raw), &binding) != nil || binding.Generation == 0 || binding.Fingerprint == "" && !binding.SignedOut || binding.Fingerprint != "" && !validCredentialFingerprint(binding.Fingerprint) {
		return AuthBinding{}, false, errors.New("invalid cache credential binding")
	}
	return binding, true, nil
}

func validCredentialFingerprint(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32
}

func (s *Store) AuthBinding(ctx context.Context) (AuthBinding, bool, error) {
	return readAuthBinding(ctx, s.db)
}

func (s *Store) HasCredentialData(ctx context.Context) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks)
	 OR EXISTS(SELECT 1 FROM projects) OR EXISTS(SELECT 1 FROM outbox)
	 OR EXISTS(SELECT 1 FROM focus_sessions) OR EXISTS(SELECT 1 FROM resource_entities)
	 OR EXISTS(SELECT 1 FROM meta WHERE key GLOB 'server_task_query_*')`).Scan(&present)
	return present, err
}

func (s *Store) CheckCredential(ctx context.Context, fingerprint string) error {
	if !validCredentialFingerprint(fingerprint) {
		return errors.New("invalid credential fingerprint")
	}
	binding, ok, err := s.AuthBinding(ctx)
	if err != nil {
		return err
	}
	if ok && binding.Fingerprint != "" && binding.Fingerprint != fingerprint {
		return ErrCredentialChanged
	}
	return nil
}

// BindCredential adopts an unbound cache or checks an active binding. It never
// resumes a signed-out cache; only an explicit login may do that.
func (s *Store) BindCredential(ctx context.Context, fingerprint string) (AuthBinding, error) {
	return s.changeAuthBinding(ctx, fingerprint, false, false)
}

func (s *Store) ResumeCredential(ctx context.Context, fingerprint string) (AuthBinding, error) {
	return s.changeAuthBinding(ctx, fingerprint, true, false)
}

func (s *Store) SignOutCredential(ctx context.Context, fingerprint string) (AuthBinding, error) {
	return s.changeAuthBinding(ctx, fingerprint, false, true)
}

func (s *Store) changeAuthBinding(ctx context.Context, fingerprint string, resume, logout bool) (AuthBinding, error) {
	if !validCredentialFingerprint(fingerprint) && !(logout && fingerprint == "") {
		return AuthBinding{}, errors.New("invalid credential fingerprint")
	}
	var result AuthBinding
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		binding, exists, err := readAuthBinding(ctx, tx)
		if err != nil {
			return err
		}
		if !logout && binding.Fingerprint != "" && binding.Fingerprint != fingerprint {
			return ErrCredentialChanged
		}
		if !logout && !resume && binding.SignedOut {
			return ErrSignedOut
		}
		if exists && !logout && !resume {
			result = binding
			return nil
		}
		if binding.Generation == ^uint64(0) {
			return errors.New("credential generation exhausted")
		}
		binding.Generation++
		binding.SignedOut = logout
		if binding.Fingerprint == "" {
			binding.Fingerprint = fingerprint
		}
		raw, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		if err := setMeta(ctx, tx, authBindingKey, string(raw)); err != nil {
			return err
		}
		result = binding
		return nil
	})
	return result, err
}
