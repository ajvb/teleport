/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package local

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"

	"github.com/gokyle/hotp"
	"github.com/gravitational/configure/cstrings"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/bcrypt"

	"github.com/tstranex/u2f"
)

// IdentityService is responsible for managing web users and currently
// user accounts as well
type IdentityService struct {
	backend      backend.Backend
	lockAfter    byte
	lockDuration time.Duration
}

// NewIdentityService returns a new instance of IdentityService object
func NewIdentityService(
	backend backend.Backend,
	lockAfter byte,
	lockDuration time.Duration) *IdentityService {

	return &IdentityService{
		backend:      backend,
		lockAfter:    lockAfter,
		lockDuration: lockDuration,
	}
}

// GetUsers returns a list of users registered with the local auth server
func (s *IdentityService) GetUsers() ([]services.User, error) {
	keys, err := s.backend.GetKeys([]string{"web", "users"})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	out := make([]services.User, len(keys))
	for i, name := range keys {
		u, err := s.GetUser(name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		out[i] = u
	}
	return out, nil
}

// UpsertUser updates parameters about user
func (s *IdentityService) UpsertUser(user services.User) error {

	if !cstrings.IsValidUnixUser(user.GetName()) {
		return trace.BadParameter("'%v is not a valid unix username'", user.GetName())
	}

	for _, l := range user.GetAllowedLogins() {
		if !cstrings.IsValidUnixUser(l) {
			return trace.BadParameter("'%v is not a valid unix username'", l)
		}
	}
	for _, i := range user.GetIdentities() {
		if err := i.Check(); err != nil {
			return trace.Wrap(err)
		}
	}
	data, err := json.Marshal(user)
	if err != nil {
		return trace.Wrap(err)
	}

	err = s.backend.UpsertVal([]string{"web", "users", user.GetName()}, "params", []byte(data), backend.Forever)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// GetUser returns a user by name
func (s *IdentityService) GetUser(user string) (services.User, error) {
	u := services.TeleportUser{Name: user}
	data, err := s.backend.GetVal([]string{"web", "users", user}, "params")
	if err != nil {
		if trace.IsNotFound(err) {
			return &u, nil
		}
		return nil, trace.Wrap(err)
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, trace.Wrap(err)
	}
	return &u, nil
}

// GetUserByOIDCIdentity returns a user by it's specified OIDC Identity, returns first
// user specified with this identity
func (s *IdentityService) GetUserByOIDCIdentity(id services.OIDCIdentity) (services.User, error) {
	users, err := s.GetUsers()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, u := range users {
		for _, uid := range u.GetIdentities() {
			if uid.Equals(&id) {
				return u, nil
			}
		}
	}
	return nil, trace.NotFound("user with identity %v not found", &id)
}

// DeleteUser deletes a user with all the keys from the backend
func (s *IdentityService) DeleteUser(user string) error {
	err := s.backend.DeleteBucket([]string{"web", "users"}, user)
	if err != nil {
		if trace.IsNotFound(err) {
			return trace.NotFound(fmt.Sprintf("user '%v' is not found", user))
		}
	}
	return trace.Wrap(err)
}

// UpsertPasswordHash upserts user password hash
func (s *IdentityService) UpsertPasswordHash(user string, hash []byte) error {
	err := s.backend.UpsertVal([]string{"web", "users", user}, "pwd", hash, 0)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// GetPasswordHash returns the password hash for a given user
func (s *IdentityService) GetPasswordHash(user string) ([]byte, error) {
	hash, err := s.backend.GetVal([]string{"web", "users", user}, "pwd")
	if err != nil {
		if trace.IsNotFound(err) {
			return nil, trace.NotFound("user '%v' is not found", user)
		}
		return nil, trace.Wrap(err)
	}
	return hash, nil
}

// UpsertHOTP upserts HOTP state for user
func (s *IdentityService) UpsertHOTP(user string, otp *hotp.HOTP) error {
	bytes, err := hotp.Marshal(otp)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.UpsertVal([]string{"web", "users", user},
		"hotp", bytes, 0)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// GetHOTP gets HOTP token state for a user
func (s *IdentityService) GetHOTP(user string) (*hotp.HOTP, error) {
	bytes, err := s.backend.GetVal([]string{"web", "users", user},
		"hotp")
	if err != nil {
		if trace.IsNotFound(err) {
			return nil, trace.NotFound("user '%v' is not found", user)
		}
		return nil, trace.Wrap(err)
	}
	otp, err := hotp.Unmarshal(bytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return otp, nil
}

// UpsertWebSession updates or inserts a web session for a user and session id
func (s *IdentityService) UpsertWebSession(user, sid string, session services.WebSession, ttl time.Duration) error {
	bytes, err := json.Marshal(session)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.UpsertVal([]string{"web", "users", user, "sessions"},
		sid, bytes, ttl)
	if trace.IsNotFound(err) {
		return trace.NotFound("user '%v' is not found", user)
	}
	return trace.Wrap(err)
}

// IncreaseLoginAttempts bumps "login attempt" counter for the given user. If the counter
// reaches 'lockAfter' value, it locks the account and returns access denied error.
func (s *IdentityService) IncreaseLoginAttempts(user string) error {
	bucket := []string{"web", "users", user}

	data, _, err := s.backend.GetValAndTTL(bucket, "lock")
	// unexpected error?
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	// bump the attempt count
	if len(data) < 1 {
		data = []byte{0}
	}
	// check the attempt count
	if len(data) > 0 && data[0] >= s.lockAfter {
		return trace.AccessDenied("this account has been locked for %v", s.lockDuration)
	}
	newData := []byte{data[0] + 1}
	// "create val" will create a new login attempt counter, or it will
	// do nothing if it's already there.
	//
	// "compare and swap" will bump the counter +1
	fmt.Printf("here; %#v %#v\n", data, newData)
	s.backend.CreateVal(bucket, "lock", data, s.lockDuration)
	newdata, _, err := s.backend.GetValAndTTL(bucket, "lock")
	fmt.Printf("after create: %#v\n", newdata)
	_, err = s.backend.CompareAndSwap(bucket, "lock", newData, s.lockDuration, data)
	fmt.Printf("here; %#v %#v %v\n", data, newData, err)
	return trace.Wrap(err)
}

// IncreaseLoginAttempts bumps "login attempt" counter for the given user. If the counter
// reaches 'lockAfter' value, it locks the account and returns access denied error.
func (s *IdentityService) IncreaseLoginAttempts2(user string) error {
	bucket := []string{"web", "users", user}

	data, _, err := s.backend.GetValAndTTL(bucket, "lock")
	// unexpected error?
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	newData := []byte{0}
	copy(newData, data)
	// check the attempt count
	if newData[0] >= s.lockAfter {
		return trace.AccessDenied("this account has been locked for %v", s.lockDuration)
	}
	newData[0] += 1
	// "create val" will create a new login attempt counter
	if len(data) == 0 {
		err = s.backend.CreateVal(bucket, "lock", newData, s.lockDuration)
		return trace.Wrap(err)
	}
	// we are going to increase the counter assuming the previous value has not changed
	_, err = s.backend.CompareAndSwap(bucket, "lock", newData, s.lockDuration, data)
	return trace.Wrap(err)
}

// ResetLoginAttempts resets the "login attempt" counter to zero.
func (s *IdentityService) ResetLoginAttempts(user string) error {
	err := s.backend.DeleteKey([]string{"web", "users", user}, "lock")
	if trace.IsNotFound(err) {
		return nil
	}
	return trace.Wrap(err)
}

// GetWebSession returns a web session state for a given user and session id
func (s *IdentityService) GetWebSession(user, sid string) (*services.WebSession, error) {
	val, err := s.backend.GetVal(
		[]string{"web", "users", user, "sessions"},
		sid,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var session services.WebSession
	err = json.Unmarshal(val, &session)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &session, nil
}

// DeleteWebSession deletes web session from the storage
func (s *IdentityService) DeleteWebSession(user, sid string) error {
	err := s.backend.DeleteKey(
		[]string{"web", "users", user, "sessions"},
		sid,
	)
	return err
}

// UpsertPassword upserts new password and HOTP token
func (s *IdentityService) UpsertPassword(user string,
	password []byte) (hotpURL string, hotpQR []byte, err error) {

	if err := services.VerifyPassword(password); err != nil {
		return "", nil, err
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}

	otp, err := hotp.GenerateHOTP(defaults.HOTPTokenDigits, false)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}
	hotpQR, err = otp.QR(user)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}
	hotpURL = otp.URL(user)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}

	err = s.UpsertPasswordHash(user, hash)
	if err != nil {
		return "", nil, err
	}
	err = s.UpsertHOTP(user, otp)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}

	return hotpURL, hotpQR, nil

}

// CheckPassword is called on web user or tsh user login
func (s *IdentityService) CheckPassword(user string, password []byte, hotpToken string) error {
	hash, err := s.GetPasswordHash(user)
	if err != nil {
		return trace.Wrap(err)
	}
	if err = s.IncreaseLoginAttempts(user); err != nil {
		return trace.Wrap(err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, password); err != nil {
		return trace.AccessDenied("passwords do not match")
	}
	otp, err := s.GetHOTP(user)
	if err != nil {
		return trace.Wrap(err)
	}
	if !otp.Scan(hotpToken, defaults.HOTPFirstTokensRange) {
		return trace.AccessDenied("bad one time token")
	}
	defer s.ResetLoginAttempts(user)
	if err := s.UpsertHOTP(user, otp); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// CheckPasswordWOToken checks just password without checking HOTP tokens
// used in case of SSH authentication, when token has been validated
func (s *IdentityService) CheckPasswordWOToken(user string, password []byte) error {
	if err := services.VerifyPassword(password); err != nil {
		return trace.Wrap(err)
	}
	hash, err := s.GetPasswordHash(user)
	if err != nil {
		return trace.Wrap(err)
	}
	if err = s.IncreaseLoginAttempts(user); err != nil {
		return trace.Wrap(err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, password); err != nil {
		return trace.BadParameter("passwords do not match")
	}
	defer s.ResetLoginAttempts(user)
	return nil
}

var (
	userTokensPath   = []string{"addusertokens"}
	u2fRegChalPath   = []string{"adduseru2fchallenges"}
	connectorsPath   = []string{"web", "connectors", "oidc", "connectors"}
	authRequestsPath = []string{"web", "connectors", "oidc", "requests"}
)

// UpsertSignupToken upserts signup token - one time token that lets user to create a user account
func (s *IdentityService) UpsertSignupToken(token string, tokenData services.SignupToken, ttl time.Duration) error {
	if ttl < time.Second || ttl > defaults.MaxSignupTokenTTL {
		ttl = defaults.MaxSignupTokenTTL
	}
	tokenData.Expires = time.Now().UTC().Add(ttl)
	out, err := json.Marshal(tokenData)
	if err != nil {
		return trace.Wrap(err)
	}

	err = s.backend.UpsertVal(userTokensPath, token, out, ttl)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil

}

// GetSignupToken returns signup token data
func (s *IdentityService) GetSignupToken(token string) (*services.SignupToken, error) {
	out, err := s.backend.GetVal(userTokensPath, token)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var data *services.SignupToken
	err = json.Unmarshal(out, &data)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return data, nil
}

// GetSignupTokens returns all non-expired user tokens
func (s *IdentityService) GetSignupTokens() (tokens []services.SignupToken, err error) {
	keys, err := s.backend.GetKeys(userTokensPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, key := range keys {
		token, err := s.GetSignupToken(key)
		if err != nil {
			log.Error(err)
		}
		tokens = append(tokens, *token)
	}
	return tokens, trace.Wrap(err)
}

// DeleteSignupToken deletes signup token from the storage
func (s *IdentityService) DeleteSignupToken(token string) error {
	err := s.backend.DeleteKey(userTokensPath, token)
	return trace.Wrap(err)
}

func (s *IdentityService) UpsertU2FRegisterChallenge(token string, u2fChallenge *u2f.Challenge) error {
	data, err := json.Marshal(u2fChallenge)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.UpsertVal(u2fRegChalPath, token, data, defaults.U2FChallengeTimeout)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (s *IdentityService) GetU2FRegisterChallenge(token string) (*u2f.Challenge, error) {
	data, err := s.backend.GetVal(u2fRegChalPath, token)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	u2fChal := u2f.Challenge{}
	err = json.Unmarshal(data, &u2fChal)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &u2fChal, nil
}

// u2f.Registration cannot be json marshalled due to the pointer in the public key so we have this marshallable version
type MarshallableU2FRegistration struct {
	Raw              []byte `json:"raw"`
	KeyHandle        []byte `json:"keyhandle"`
	MarshalledPubKey []byte `json:"marshalled_pubkey"`

	// AttestationCert is not needed for authentication so we don't need to store it
}

func (s *IdentityService) UpsertU2FRegistration(user string, u2fReg *u2f.Registration) error {
	marshalledPubkey, err := x509.MarshalPKIXPublicKey(&u2fReg.PubKey)
	if err != nil {
		return trace.Wrap(err)
	}

	marshallableReg := MarshallableU2FRegistration{
		Raw:              u2fReg.Raw,
		KeyHandle:        u2fReg.KeyHandle,
		MarshalledPubKey: marshalledPubkey,
	}

	data, err := json.Marshal(marshallableReg)
	if err != nil {
		return trace.Wrap(err)
	}

	err = s.backend.UpsertVal([]string{"web", "users", user}, "u2fregistration", data, backend.Forever)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (s *IdentityService) GetU2FRegistration(user string) (*u2f.Registration, error) {
	data, err := s.backend.GetVal([]string{"web", "users", user}, "u2fregistration")
	if err != nil {
		return nil, trace.Wrap(err)
	}

	marshallableReg := MarshallableU2FRegistration{}
	err = json.Unmarshal(data, &marshallableReg)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pubkeyInterface, err := x509.ParsePKIXPublicKey(marshallableReg.MarshalledPubKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pubkey, ok := pubkeyInterface.(*ecdsa.PublicKey)
	if !ok {
		return nil, trace.Wrap(errors.New("failed to convert crypto.PublicKey back to ecdsa.PublicKey"))
	}

	return &u2f.Registration{
		Raw:             marshallableReg.Raw,
		KeyHandle:       marshallableReg.KeyHandle,
		PubKey:          *pubkey,
		AttestationCert: nil,
	}, nil
}

type U2FRegistrationCounter struct {
	Counter uint32 `json:"counter"`
}

func (s *IdentityService) UpsertU2FRegistrationCounter(user string, counter uint32) error {
	data, err := json.Marshal(U2FRegistrationCounter{
		Counter: counter,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	err = s.backend.UpsertVal([]string{"web", "users", user}, "u2fregistrationcounter", data, backend.Forever)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (s *IdentityService) GetU2FRegistrationCounter(user string) (counter uint32, e error) {
	data, err := s.backend.GetVal([]string{"web", "users", user}, "u2fregistrationcounter")
	if err != nil {
		return 0, trace.Wrap(err)
	}

	u2fRegCounter := U2FRegistrationCounter{}
	err = json.Unmarshal(data, &u2fRegCounter)
	if err != nil {
		return 0, trace.Wrap(err)
	}

	return u2fRegCounter.Counter, nil
}

func (s *IdentityService) UpsertU2FSignChallenge(user string, u2fChallenge *u2f.Challenge) error {
	data, err := json.Marshal(u2fChallenge)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.UpsertVal([]string{"web", "users", user}, "u2fsignchallenge", data, defaults.U2FChallengeTimeout)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (s *IdentityService) GetU2FSignChallenge(user string) (*u2f.Challenge, error) {
	data, err := s.backend.GetVal([]string{"web", "users", user}, "u2fsignchallenge")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	u2fChal := u2f.Challenge{}
	err = json.Unmarshal(data, &u2fChal)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &u2fChal, nil
}

// UpsertOIDCConnector upserts OIDC Connector
func (s *IdentityService) UpsertOIDCConnector(connector services.OIDCConnector, ttl time.Duration) error {
	if err := connector.Check(); err != nil {
		return trace.Wrap(err)
	}
	data, err := json.Marshal(connector)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.UpsertVal(connectorsPath, connector.ID, data, ttl)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// DeleteOIDCConnector deletes OIDC Connector
func (s *IdentityService) DeleteOIDCConnector(connectorID string) error {
	err := s.backend.DeleteKey(connectorsPath, connectorID)
	return trace.Wrap(err)
}

// GetOIDCConnector returns OIDC connector data, , withSecrets adds or removes client secret from return results
func (s *IdentityService) GetOIDCConnector(id string, withSecrets bool) (*services.OIDCConnector, error) {
	out, err := s.backend.GetVal(connectorsPath, id)
	if err != nil {
		if trace.IsNotFound(err) {
			return nil, trace.NotFound("OpenID connector '%v' is not configured", id)
		}
		return nil, trace.Wrap(err)
	}
	var data *services.OIDCConnector
	err = json.Unmarshal(out, &data)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !withSecrets {
		data.ClientSecret = ""
	}
	return data, nil
}

// GetOIDCConnectors returns registered connectors, withSecrets adds or removes client secret from return results
func (s *IdentityService) GetOIDCConnectors(withSecrets bool) ([]services.OIDCConnector, error) {
	connectorIDs, err := s.backend.GetKeys(connectorsPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	connectors := make([]services.OIDCConnector, 0, len(connectorIDs))
	for _, id := range connectorIDs {
		connector, err := s.GetOIDCConnector(id, withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		connectors = append(connectors, *connector)
	}
	return connectors, nil
}

// CreateOIDCAuthRequest creates new auth request
func (s *IdentityService) CreateOIDCAuthRequest(req services.OIDCAuthRequest, ttl time.Duration) error {
	if err := req.Check(); err != nil {
		return trace.Wrap(err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return trace.Wrap(err)
	}
	err = s.backend.CreateVal(authRequestsPath, req.StateToken, data, ttl)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// GetOIDCAuthRequest returns OIDC auth request if found
func (s *IdentityService) GetOIDCAuthRequest(stateToken string) (*services.OIDCAuthRequest, error) {
	data, err := s.backend.GetVal(authRequestsPath, stateToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var req *services.OIDCAuthRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, trace.Wrap(err)
	}
	return req, nil
}
