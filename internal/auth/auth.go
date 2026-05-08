package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/linda/linda_cam/internal/config"
)

const (
	cookieName    = "linda_sess"
	sessionMaxAge = 30 * 24 * time.Hour
)

type Manager struct {
	store *config.Store
}

func New(store *config.Store) *Manager {
	return &Manager{store: store}
}

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func (m *Manager) CheckPassword(pw string) bool {
	cfg := m.store.Get()
	if cfg.PasswordHash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(pw)) == nil
}

func (m *Manager) sessionKey() ([]byte, error) {
	cfg := m.store.Get()
	if cfg.SessionKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		cfg.SessionKey = hex.EncodeToString(key)
		if _, err := m.store.Update(func(c *config.Config) { c.SessionKey = cfg.SessionKey }); err != nil {
			return nil, err
		}
	}
	return hex.DecodeString(cfg.SessionKey)
}

func (m *Manager) IssueCookie(w http.ResponseWriter) error {
	key, err := m.sessionKey()
	if err != nil {
		return err
	}
	exp := time.Now().Add(sessionMaxAge).Unix()
	payload := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	val := payload + "." + base64.RawURLEncoding.EncodeToString(sig)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(exp, 0),
	})
	return nil
}

func (m *Manager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

func (m *Manager) IsAuthenticated(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	key, err := m.sessionKey()
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	return hmac.Equal(sig, mac.Sum(nil))
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.IsAuthenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- HTTP handlers ---

type loginReq struct {
	Password string `json:"password"`
}

func (m *Manager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !m.CheckPassword(req.Password) {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	if err := m.IssueCookie(w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	m.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) HandleFirstRun(w http.ResponseWriter, r *http.Request) {
	if !m.store.FirstRun() {
		http.Error(w, "already initialized", http.StatusConflict)
		return
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 6 {
		http.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := m.store.Update(func(c *config.Config) { c.PasswordHash = hash }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.IssueCookie(w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type changePwReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (m *Manager) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePwReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !m.CheckPassword(req.OldPassword) {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if len(req.NewPassword) < 6 {
		http.Error(w, "new password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := m.store.Update(func(c *config.Config) { c.PasswordHash = hash }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionInfo struct {
	Authenticated bool `json:"authenticated"`
	FirstRun      bool `json:"first_run"`
}

func (m *Manager) HandleSession(w http.ResponseWriter, r *http.Request) {
	info := sessionInfo{
		Authenticated: m.IsAuthenticated(r),
		FirstRun:      m.store.FirstRun(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

var ErrNotAuthed = errors.New("not authenticated")

// Verify is a convenience used by handlers that need to confirm auth status
// without going through the middleware (e.g. for custom status codes).
func (m *Manager) Verify(r *http.Request) error {
	if !m.IsAuthenticated(r) {
		return fmt.Errorf("%w", ErrNotAuthed)
	}
	return nil
}
