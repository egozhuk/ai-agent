package integrations

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"AI-agent/internal/store"
)

const googleProvider = "google"

var googleScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/calendar.events",
}

type googleOAuth struct {
	clientID     string
	clientSecret string
	redirectURL  string
	stateSecret  []byte
	tokenCipher  cipher.AEAD
	store        *store.Store
	httpClient   *http.Client
}

type googleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func newGoogleOAuth(clientID, clientSecret, publicBaseURL, encryptionKey string, st *store.Store, httpClient *http.Client) (*googleOAuth, error) {
	if clientID == "" || clientSecret == "" {
		return nil, nil
	}
	if publicBaseURL == "" {
		return nil, errors.New("PUBLIC_BASE_URL is required for Google OAuth")
	}
	baseURL, err := url.Parse(strings.TrimRight(publicBaseURL, "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("PUBLIC_BASE_URL must be an absolute URL, for example https://your-domain.com")
	}
	if baseURL.Scheme != "https" && baseURL.Hostname() != "localhost" && baseURL.Hostname() != "127.0.0.1" {
		return nil, errors.New("PUBLIC_BASE_URL must use https, except localhost during development")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	block, err := aes.NewCipher(normalizeKey(encryptionKey))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	keyHash := sha256.Sum256([]byte(encryptionKey))
	return &googleOAuth{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  baseURL.String() + "/oauth/google/callback",
		stateSecret:  keyHash[:],
		tokenCipher:  aead,
		store:        st,
		httpClient:   httpClient,
	}, nil
}

func (g *googleOAuth) configured() bool {
	return g != nil
}

func (g *googleOAuth) authURL(userID int64) (string, error) {
	if !g.configured() {
		return "", errors.New("Google OAuth is not configured")
	}
	state, err := g.signState(userID)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("client_id", g.clientID)
	values.Set("redirect_uri", g.redirectURL)
	values.Set("response_type", "code")
	values.Set("scope", strings.Join(googleScopes, " "))
	values.Set("access_type", "offline")
	values.Set("include_granted_scopes", "true")
	values.Set("prompt", "consent")
	values.Set("state", state)
	return "https://accounts.google.com/o/oauth2/v2/auth?" + values.Encode(), nil
}

func (g *googleOAuth) redirectURI() string {
	if !g.configured() {
		return ""
	}
	return g.redirectURL
}

func (g *googleOAuth) clientIDPreview() string {
	if !g.configured() || g.clientID == "" {
		return ""
	}
	if len(g.clientID) <= 16 {
		return g.clientID
	}
	return g.clientID[:8] + "..." + g.clientID[len(g.clientID)-8:]
}

func (g *googleOAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !g.configured() {
		http.Error(w, "Google OAuth is not configured", http.StatusServiceUnavailable)
		return
	}
	if errText := r.URL.Query().Get("error"); errText != "" {
		http.Error(w, "Google authorization failed: "+errText, http.StatusBadRequest)
		return
	}
	userID, err := g.verifyState(r.URL.Query().Get("state"))
	if err != nil {
		http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	token, err := g.exchangeCode(r.Context(), code)
	if err != nil {
		http.Error(w, "Token exchange failed", http.StatusBadGateway)
		return
	}
	if err := g.saveToken(userID, token, true); err != nil {
		http.Error(w, "Token save failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>MAX AI Agent</title><p>Google аккаунт подключен. Можно вернуться в MAX.</p>"))
}

func (g *googleOAuth) exchangeCode(ctx context.Context, code string) (googleToken, error) {
	values := url.Values{}
	values.Set("code", code)
	values.Set("client_id", g.clientID)
	values.Set("client_secret", g.clientSecret)
	values.Set("redirect_uri", g.redirectURL)
	values.Set("grant_type", "authorization_code")
	return g.tokenRequest(ctx, values)
}

func (g *googleOAuth) refresh(ctx context.Context, refreshToken string) (googleToken, error) {
	values := url.Values{}
	values.Set("client_id", g.clientID)
	values.Set("client_secret", g.clientSecret)
	values.Set("refresh_token", refreshToken)
	values.Set("grant_type", "refresh_token")
	return g.tokenRequest(ctx, values)
}

func (g *googleOAuth) tokenRequest(ctx context.Context, values url.Values) (googleToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(values.Encode()))
	if err != nil {
		return googleToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return googleToken{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return googleToken{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return googleToken{}, fmt.Errorf("Google token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var token googleToken
	if err := json.Unmarshal(data, &token); err != nil {
		return googleToken{}, err
	}
	return token, nil
}

func (g *googleOAuth) accessToken(ctx context.Context, userID int64) (string, error) {
	account, ok := g.store.Account(userID, googleProvider)
	if !ok {
		return "", errors.New("Google account is not connected")
	}
	accessToken, err := g.decrypt(account.EncryptedAccessToken)
	if err != nil {
		return "", err
	}
	if time.Until(account.Expiry) > time.Minute {
		return accessToken, nil
	}
	refreshToken, err := g.decrypt(account.EncryptedRefreshToken)
	if err != nil || refreshToken == "" {
		return "", errors.New("Google refresh token is missing; reconnect Google")
	}
	token, err := g.refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	if err := g.saveToken(userID, token, false); err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func (g *googleOAuth) saveToken(userID int64, token googleToken, firstConnect bool) error {
	encryptedAccess, err := g.encrypt(token.AccessToken)
	if err != nil {
		return err
	}
	refreshToken := token.RefreshToken
	if refreshToken == "" && !firstConnect {
		if account, ok := g.store.Account(userID, googleProvider); ok {
			refreshToken, _ = g.decrypt(account.EncryptedRefreshToken)
		}
	}
	encryptedRefresh := ""
	if refreshToken != "" {
		encryptedRefresh, err = g.encrypt(refreshToken)
		if err != nil {
			return err
		}
	}
	expiry := time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.ExpiresIn == 0 {
		expiry = time.Now().UTC().Add(time.Hour)
	}
	return g.store.SaveAccount(userID, store.Account{
		Provider:              googleProvider,
		ConnectedAt:           time.Now().UTC(),
		Scopes:                googleScopes,
		EncryptedAccessToken:  encryptedAccess,
		EncryptedRefreshToken: encryptedRefresh,
		TokenType:             token.TokenType,
		Expiry:                expiry,
	})
}

func (g *googleOAuth) signState(userID int64) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := fmt.Sprintf("%d.%d.%s", userID, time.Now().Unix(), base64.RawURLEncoding.EncodeToString(nonce))
	mac := hmac.New(sha256.New, g.stateSecret)
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "." + signature)), nil
}

func (g *googleOAuth) verifyState(state string) (int64, error) {
	data, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return 0, err
	}
	parts := strings.Split(string(data), ".")
	if len(parts) != 4 {
		return 0, errors.New("bad state format")
	}
	payload := strings.Join(parts[:3], ".")
	mac := hmac.New(sha256.New, g.stateSecret)
	_, _ = mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	actual, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return 0, err
	}
	if !hmac.Equal(actual, expected) {
		return 0, errors.New("bad state signature")
	}
	createdAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, err
	}
	if time.Since(time.Unix(createdAt, 0)) > 15*time.Minute {
		return 0, errors.New("state expired")
	}
	return strconv.ParseInt(parts[0], 10, 64)
}

func (g *googleOAuth) encrypt(plaintext string) (string, error) {
	nonce := make([]byte, g.tokenCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := g.tokenCipher.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func (g *googleOAuth) decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	nonceSize := g.tokenCipher.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("encrypted token is too short")
	}
	plaintext, err := g.tokenCipher.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func normalizeKey(secret string) []byte {
	if decoded, err := base64.StdEncoding.DecodeString(secret); err == nil && len(decoded) >= 32 {
		return decoded[:32]
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func (g *googleOAuth) getJSON(ctx context.Context, endpoint, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return g.doJSON(req, nil, out)
}

func (g *googleOAuth) postJSON(ctx context.Context, endpoint, accessToken string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	return g.doJSON(req, payload, out)
}

func (g *googleOAuth) doJSON(req *http.Request, _ []byte, out any) error {
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Google API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
