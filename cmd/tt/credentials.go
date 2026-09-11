package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

const credentialsFileName = "oauth.json"

type appCredentials struct {
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret"`
	RedirectURI     string   `json:"redirect_uri"`
	RefreshToken    string   `json:"refresh_token,omitempty"`
	Method          string   `json:"method,omitempty"`
	RequestedScopes []string `json:"requested_scopes,omitempty"`
	ReturnedScopes  []string `json:"returned_scopes,omitempty"`
	ScopesKnown     bool     `json:"scopes_known,omitempty"`
}

type credentialFileInfo struct {
	Path         string
	ResolvedPath string
	PathVerified bool
	Read         bool
	Mode         os.FileMode
	Insecure     bool
}

type credentialDirInfo struct {
	Path         string
	ResolvedPath string
	PathVerified bool
	Mode         os.FileMode
	Insecure     bool
}

func hasRefreshToken(value string) bool { return strings.TrimSpace(value) != "" }

func credentialsPath() (string, error) {
	tokenPath, err := api.TokenPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(tokenPath), credentialsFileName), nil
}

func inspectCredentialDir() (credentialDirInfo, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentialDirInfo{}, err
	}
	return inspectCredentialDirAt(filepath.Dir(path))
}

func inspectCredentialDirAt(dir string) (credentialDirInfo, error) {
	opened, err := api.InspectDirectory(dir)
	if err != nil {
		return credentialDirInfo{Path: dir}, err
	}
	mode := opened.Mode
	return credentialDirInfo{
		Path:         dir,
		ResolvedPath: opened.ResolvedPath,
		PathVerified: opened.PathVerified,
		Mode:         mode,
		Insecure:     mode&0o077 != 0,
	}, nil
}

func loadCredentials() (creds appCredentials, ok bool, err error) {
	creds, _, ok, err = loadCredentialsInfo()
	return creds, ok, err
}

func loadCredentialsInfo() (creds appCredentials, info credentialFileInfo, ok bool, err error) {
	path, err := credentialsPath()
	if err != nil {
		return appCredentials{}, credentialFileInfo{}, false, err
	}
	file, err := api.ReadRegularFile(path)
	if err != nil {
		if os.IsNotExist(err) && !errors.Is(err, api.ErrUnusableLink) {
			return appCredentials{}, credentialFileInfo{Path: path}, false, nil
		}
		return appCredentials{}, credentialFileInfo{Path: path}, false, fmt.Errorf("read %s: %w", path, err)
	}
	info = credentialFileInfo{
		Path:         path,
		ResolvedPath: file.ResolvedPath,
		PathVerified: file.PathVerified,
		Read:         true,
		Mode:         file.Mode,
		Insecure:     file.Mode&0o077 != 0,
	}
	if err := json.Unmarshal(file.Data, &creds); err != nil {
		return appCredentials{}, info, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return creds, info, true, nil
}

func saveCredentials(c appCredentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	return writeSecretFile(path, string(data)+"\n")
}

func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func acquireAuthLock(ctx context.Context, tokenPath string) (*store.Lock, error) {
	if err := prepareSecretDir(filepath.Dir(tokenPath)); err != nil {
		return nil, err
	}
	path := tokenPath + ".auth-lock"
	for {
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("credential lock is not a regular file")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		lock, err := store.AcquireLock(path)
		if !errors.Is(err, store.ErrLocked) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func selectedTokenAt(path string) (string, error) {
	if token := strings.TrimSpace(os.Getenv(tokenEnvVar)); token != "" {
		return token, nil
	}
	info, err := api.LoadTokenFrom(path)
	return info.Value, err
}

type credentialSession struct {
	tokenPath, cachePath, fingerprint string
	generation                        uint64
	stored                            bool
}

func managedCredential(ctx context.Context, token string) (credentialSession, error) {
	var session credentialSession
	if strings.TrimSpace(token) == "" {
		return session, errors.New("no credential is available")
	}
	if flaw, _ := inspectTokenValue(token); flaw == tokenUnsendable {
		return session, errors.New("credential contains characters that cannot be sent")
	}
	var err error
	session.tokenPath, err = api.TokenPath()
	if err != nil {
		return session, err
	}
	session.cachePath, err = store.DefaultPath()
	if err != nil {
		return session, err
	}
	lock, err := acquireAuthLock(ctx, session.tokenPath)
	if err != nil {
		return session, err
	}
	defer lock.Release()
	selected, readErr := selectedTokenAt(session.tokenPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return session, readErr
	}
	session.stored = selected != ""
	if session.stored && selected != token {
		return session, store.ErrCredentialChanged
	}
	st, err := store.Open(ctx, session.cachePath)
	if err != nil {
		return session, err
	}
	defer st.Close()
	session.fingerprint = credentialFingerprint(token)
	if err := bindSavedCredentialForLegacy(ctx, st, session.tokenPath); err != nil {
		return session, err
	}
	binding, err := st.BindCredential(ctx, session.fingerprint)
	session.generation = binding.Generation
	return session, err
}

func (session credentialSession) guard(ctx context.Context) (func(), error) {
	lock, err := acquireAuthLock(ctx, session.tokenPath)
	if err != nil {
		return nil, err
	}
	release := func() { _ = lock.Release() }
	fail := func(err error) (func(), error) { release(); return nil, err }
	selected, err := selectedTokenAt(session.tokenPath)
	if err != nil && (!errors.Is(err, os.ErrNotExist) || session.stored) {
		return fail(err)
	}
	if selected != "" && credentialFingerprint(selected) != session.fingerprint || selected == "" && session.stored {
		return fail(store.ErrCredentialChanged)
	}
	inspection, err := store.Inspect(ctx, session.cachePath)
	if err != nil {
		return fail(err)
	}
	binding, exists, err := inspection.Store.AuthBinding(ctx)
	inspection.Close()
	if err != nil {
		return fail(err)
	}
	if !exists || binding.Generation != session.generation {
		return fail(store.ErrCredentialGeneration)
	}
	if binding.SignedOut {
		return fail(store.ErrSignedOut)
	}
	if binding.Fingerprint != session.fingerprint {
		return fail(store.ErrCredentialChanged)
	}
	return release, nil
}

func bindSavedCredentialForLegacy(ctx context.Context, st *store.Store, tokenPath string) error {
	binding, bound, err := st.AuthBinding(ctx)
	if err != nil || bound && binding.Fingerprint != "" {
		return err
	}
	hasData, err := st.HasCredentialData(ctx)
	if err != nil || !hasData {
		return err
	}
	old, err := api.LoadTokenFrom(tokenPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old.Value == "" {
		return store.ErrUnboundCredentialData
	}
	if flaw, _ := inspectTokenValue(old.Value); flaw == tokenUnsendable {
		return store.ErrUnboundCredentialData
	}
	if bound {
		return store.ErrUnboundCredentialData
	}
	_, err = st.BindCredential(ctx, credentialFingerprint(old.Value))
	return err
}

func checkedAuthToken(token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := managedCredential(ctx, token)
	if err != nil {
		return "", openAuthAdvice(err)
	}
	return token, nil
}

func managedAPIClient(token string, opts ...api.Option) *api.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := managedCredential(ctx, token)
	guard := session.guard
	if err != nil {
		guard = func(context.Context) (func(), error) { return nil, err }
	}
	opts = append(opts, api.WithRequestGuard(guard), api.WithErrorAdvice(openAuthAdvice))
	return api.NewClient(token, version, opts...)
}

func saveLoginCredential(ctx context.Context, token string, credentials *appCredentials) error {
	path, err := api.TokenPath()
	if err != nil {
		return err
	}
	lock, err := acquireAuthLock(ctx, path)
	if err != nil {
		return err
	}
	defer lock.Release()
	st, err := store.Open(ctx, "")
	if err != nil {
		return err
	}
	defer st.Close()
	if err := bindSavedCredentialForLegacy(ctx, st, path); err != nil {
		return err
	}
	fingerprint := credentialFingerprint(token)
	if err := st.CheckCredential(ctx, fingerprint); err != nil {
		if !errors.Is(err, store.ErrCredentialChanged) || ctx.Value(sameAccountRecoveryKey{}) != true {
			return openAuthAdvice(err)
		}
		if strings.TrimSpace(os.Getenv(tokenEnvVar)) != "" {
			return errors.New("unset TT_TOKEN before recovering a saved Open API credential")
		}
		binding, exists, err := st.AuthBinding(ctx)
		if err != nil {
			return recoveryError(err)
		}
		if !exists {
			return recoveryError(store.ErrCredentialGeneration)
		}
		check := api.NewClient(token, version, api.WithMaxRetries(1), api.WithErrorAdvice(openAuthAdvice))
		if _, err := check.ListProjects(ctx); err != nil {
			return recoveryError(err)
		}
		if err := st.RebindCredential(ctx, binding, fingerprint); err != nil {
			return recoveryError(err)
		}
	}
	// Disable remote access before file replacement. An interrupted save remains
	// signed out, even if one of the credential files was already replaced.
	if _, err := st.SignOutCredential(ctx, fingerprint); err != nil {
		return err
	}
	if err := writeSecretFile(path, token); err != nil {
		return err
	}
	if credentials != nil {
		if err := saveCredentials(*credentials); err != nil {
			return err
		}
	} else {
		old, ok, err := loadCredentials()
		if err != nil {
			credentialPath, pathErr := credentialsPath()
			if pathErr != nil {
				return pathErr
			}
			if err := os.Remove(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if ok {
			old.RefreshToken, old.Method, old.RequestedScopes, old.ReturnedScopes, old.ScopesKnown = "", "token", nil, nil, false
			if err := saveCredentials(old); err != nil {
				return err
			}
		}
	}
	_, err = st.ResumeCredential(ctx, fingerprint)
	return err
}

func logoutCredential(ctx context.Context) error {
	path, err := api.TokenPath()
	if err != nil {
		return err
	}
	lock, err := acquireAuthLock(ctx, path)
	if err != nil {
		return err
	}
	defer lock.Release()
	st, err := store.Open(ctx, "")
	if err != nil {
		return err
	}
	defer st.Close()
	fingerprint := ""
	if token, err := selectedTokenAt(path); err == nil && token != "" {
		fingerprint = credentialFingerprint(token)
	}
	binding, bound, err := st.AuthBinding(ctx)
	if err != nil {
		return err
	}
	if !bound || binding.Fingerprint == "" {
		hasData, err := st.HasCredentialData(ctx)
		if err != nil {
			return err
		}
		if hasData {
			fingerprint = ""
			old, err := api.LoadTokenFrom(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if old.Value != "" {
				fingerprint = credentialFingerprint(old.Value)
			}
		}
	}
	if _, err := st.SignOutCredential(ctx, fingerprint); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	credentials, ok, readErr := loadCredentials()
	if readErr != nil {
		credentialPath, err := credentialsPath()
		if err != nil {
			return err
		}
		if err := os.Remove(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if ok {
		credentials.RefreshToken = ""
		if err := saveCredentials(credentials); err != nil {
			return err
		}
	}
	return nil
}
