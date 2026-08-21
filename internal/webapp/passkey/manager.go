// Package passkey owns bounded WebAuthn ceremonies.
package passkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

type ceremony struct {
	purpose, userID, sessionID string
	data                       webauthn.SessionData
	expires                    time.Time
}
type Manager struct {
	rpID       string
	engine     *webauthn.WebAuthn
	store      *store.Store
	mu         sync.Mutex
	ceremonies map[string]ceremony
}

func New(rpID, name string, origins []string, state *store.Store) (*Manager, error) {
	if rpID == "" {
		return nil, nil
	}
	engine, err := webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: name, RPOrigins: origins, AttestationPreference: protocol.PreferNoAttestation, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return nil, err
	}
	return &Manager{rpID, engine, state, sync.Mutex{}, map[string]ceremony{}}, nil
}

type user struct {
	id          []byte
	name        string
	credentials []webauthn.Credential
	userID      string
}

func (u user) WebAuthnID() []byte                         { return u.id }
func (u user) WebAuthnName() string                       { return u.name }
func (u user) WebAuthnDisplayName() string                { return u.name }
func (u user) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
func (m *Manager) loadByID(ctx context.Context, id string) (user, error) {
	material, err := m.store.WebAuthnByUserID(ctx, id)
	if err != nil {
		return user{}, err
	}
	return decodeUser(material)
}
func (m *Manager) loadByHandle(ctx context.Context, handle []byte) (user, error) {
	material, err := m.store.WebAuthnByHandle(ctx, handle)
	if err != nil {
		return user{}, err
	}
	return decodeUser(material)
}
func decodeUser(material store.WebAuthnMaterial) (user, error) {
	out := user{id: material.Handle, name: material.Username, userID: material.UserID}
	for _, raw := range material.Credentials {
		var credential webauthn.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return user{}, err
		}
		out.credentials = append(out.credentials, credential)
	}
	return out, nil
}
func (m *Manager) BeginLogin() (string, any, error) {
	options, data, err := m.engine.BeginDiscoverableLogin()
	if err != nil {
		return "", nil, err
	}
	id := uuid.NewString()
	m.put(id, ceremony{"login", "", "", *data, time.Now().Add(5 * time.Minute)})
	return id, options, nil
}
func (m *Manager) FinishLogin(ctx context.Context, id string, credential json.RawMessage) (string, error) {
	item, err := m.take(id, "login", "", "")
	if err != nil {
		return "", err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(credential))
	request.Header.Set("Content-Type", "application/json")
	var resolved user
	validated, err := m.engine.FinishDiscoverableLogin(func(_, handle []byte) (webauthn.User, error) {
		candidate, loadErr := m.loadByHandle(ctx, handle)
		resolved = candidate
		return candidate, loadErr
	}, item.data, request)
	if err != nil {
		return "", errors.New("passkey verification failed")
	}
	if validated.Authenticator.CloneWarning {
		return "", errors.New("passkey sign counter regressed")
	}
	encoded, _ := json.Marshal(validated)
	if err = m.store.UpdateWebAuthnCredential(ctx, resolved.userID, validated.ID, encoded, validated.Authenticator.SignCount, validated.Flags.BackupEligible, validated.Flags.BackupState, time.Now()); err != nil {
		return "", err
	}
	return resolved.userID, nil
}
func (m *Manager) BeginEnrollment(ctx context.Context, userID, sessionID string) (string, any, error) {
	u, err := m.loadByID(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	options, data, err := m.engine.BeginRegistration(u, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired), webauthn.WithExclusions(webauthn.Credentials(u.credentials).CredentialDescriptors()))
	if err != nil {
		return "", nil, err
	}
	id := uuid.NewString()
	m.put(id, ceremony{"enroll", userID, sessionID, *data, time.Now().Add(5 * time.Minute)})
	return id, options, nil
}
func (m *Manager) FinishEnrollment(ctx context.Context, id, userID, sessionID, label string, response json.RawMessage) error {
	item, err := m.take(id, "enroll", userID, sessionID)
	if err != nil {
		return err
	}
	u, err := m.loadByID(ctx, userID)
	if err != nil {
		return err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(response))
	request.Header.Set("Content-Type", "application/json")
	credential, err := m.engine.FinishRegistration(u, item.data, request)
	if err != nil {
		return errors.New("passkey enrollment failed")
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	return m.store.SaveWebAuthnCredential(ctx, userID, m.rpID, label, credential.ID, encoded, time.Now())
}
func (m *Manager) put(id string, value ceremony) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for key, item := range m.ceremonies {
		if !now.Before(item.expires) {
			delete(m.ceremonies, key)
		}
	}
	m.ceremonies[id] = value
}
func (m *Manager) take(id, purpose, userID, sessionID string) (ceremony, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.ceremonies[id]
	delete(m.ceremonies, id)
	if !ok || item.purpose != purpose || item.userID != userID || item.sessionID != sessionID || !time.Now().Before(item.expires) {
		return ceremony{}, errors.New("passkey ceremony is invalid or expired")
	}
	return item, nil
}
