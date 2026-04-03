package main

import (
	cryptoRand "crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

type userRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

type activationRecord struct {
	MachineID  string `json:"machine_id"`
	ActivatedAt string `json:"activated_at"`
	LastSeenAt  string `json:"last_seen_at"`
}

type licenseRecord struct {
	Key         string             `json:"key"`
	KeyLast4    string             `json:"key_last4"`
	UserID      string             `json:"user_id"`
	Status      string             `json:"status"`
	MaxDevices  int                `json:"max_devices"`
	CreatedAt   string             `json:"created_at"`
	ExpiresAt   string             `json:"expires_at,omitempty"`
	Activations []activationRecord `json:"activations"`
}

type dbData struct {
	Users    []userRecord    `json:"users"`
	Licenses []licenseRecord `json:"licenses"`
}

type app struct {
	mu       sync.Mutex
	dataFile string
	data     dbData
	tpl      *template.Template
}

func main() {
	addr := getenv("ADDR", ":8085")
	dataFile := getenv("DATA_FILE", "./data/license-db.json")

	tpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	a := &app{
		dataFile: dataFile,
		tpl:      tpl,
	}
	if err := a.load(); err != nil {
		log.Fatalf("load db: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/users", a.handleUsers)
	mux.HandleFunc("/api/keys", a.handleKeys)
	mux.HandleFunc("/api/keys/verify", a.handleVerifyKey)

	log.Printf("license backend listening on %s", addr)
	if err := http.ListenAndServe(addr, loggingMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.tpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (a *app) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		users := append([]userRecord(nil), a.data.Users...)
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": users})
	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := decodeJSON(r.Body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		if req.Name == "" || req.Email == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name_and_email_required"})
			return
		}

		a.mu.Lock()
		defer a.mu.Unlock()

		for _, u := range a.data.Users {
			if strings.EqualFold(u.Email, req.Email) {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "email_exists"})
				return
			}
		}

		user := userRecord{
			ID:        "usr_" + randomAlphaNum(12),
			Name:      req.Name,
			Email:     req.Email,
			CreatedAt: nowISO(),
		}
		a.data.Users = append(a.data.Users, user)
		if err := a.saveLocked(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
		a.mu.Lock()
		keys := append([]licenseRecord(nil), a.data.Licenses...)
		a.mu.Unlock()
		if userID != "" {
			filtered := make([]licenseRecord, 0, len(keys))
			for _, k := range keys {
				if k.UserID == userID {
					filtered = append(filtered, k)
				}
			}
			keys = filtered
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": keys})
	case http.MethodPost:
		var req struct {
			UserID     string `json:"user_id"`
			MaxDevices int    `json:"max_devices"`
			ExpiresAt  string `json:"expires_at"`
		}
		if err := decodeJSON(r.Body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
			return
		}
		req.UserID = strings.TrimSpace(req.UserID)
		if req.UserID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "user_id_required"})
			return
		}
		if req.MaxDevices <= 0 {
			req.MaxDevices = 2
		}
		if req.MaxDevices > 100 {
			req.MaxDevices = 100
		}

		if req.ExpiresAt != "" {
			if _, err := time.Parse(time.RFC3339, req.ExpiresAt); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "expires_at_must_be_rfc3339"})
				return
			}
		}

		a.mu.Lock()
		defer a.mu.Unlock()

		if !a.userExistsLocked(req.UserID) {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "user_not_found"})
			return
		}

		key := generateLicenseKey()
		rec := licenseRecord{
			Key:         key,
			KeyLast4:    key[len(key)-4:],
			UserID:      req.UserID,
			Status:      "active",
			MaxDevices:  req.MaxDevices,
			CreatedAt:   nowISO(),
			ExpiresAt:   req.ExpiresAt,
			Activations: []activationRecord{},
		}
		a.data.Licenses = append(a.data.Licenses, rec)
		if err := a.saveLocked(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": rec})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleVerifyKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key       string `json:"key"`
		MachineID string `json:"machine_id"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	req.Key = strings.ToUpper(strings.TrimSpace(req.Key))
	req.MachineID = strings.TrimSpace(req.MachineID)

	if req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "key_required"})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	idx := -1
	for i, k := range a.data.Licenses {
		if strings.EqualFold(k.Key, req.Key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "invalid_key"})
		return
	}

	rec := &a.data.Licenses[idx]
	if rec.Status != "active" {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "revoked"})
		return
	}
	if rec.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, rec.ExpiresAt)
		if err == nil && expiresAt.Before(time.Now()) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "expired"})
			return
		}
	}

	if req.MachineID != "" {
		found := false
		for i, a := range rec.Activations {
			if a.MachineID == req.MachineID {
				rec.Activations[i].LastSeenAt = nowISO()
				found = true
				break
			}
		}
		if !found {
			if len(rec.Activations) >= rec.MaxDevices {
				writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "device_limit"})
				return
			}
			now := nowISO()
			rec.Activations = append(rec.Activations, activationRecord{
				MachineID:   req.MachineID,
				ActivatedAt: now,
				LastSeenAt:  now,
			})
		}
		if err := a.saveLocked(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"license": map[string]any{
			"key_last4":     rec.KeyLast4,
			"user_id":       rec.UserID,
			"status":        rec.Status,
			"max_devices":   rec.MaxDevices,
			"active_devices": len(rec.Activations),
			"expires_at":    rec.ExpiresAt,
		},
		"validated_at": nowISO(),
	})
}

func (a *app) userExistsLocked(userID string) bool {
	for _, u := range a.data.Users {
		if u.ID == userID {
			return true
		}
	}
	return false
}

func (a *app) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.dataFile), 0o755); err != nil {
		return err
	}

	raw, err := os.ReadFile(a.dataFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.data = dbData{Users: []userRecord{}, Licenses: []licenseRecord{}}
			return a.saveLocked()
		}
		return err
	}

	if len(strings.TrimSpace(string(raw))) == 0 {
		a.data = dbData{Users: []userRecord{}, Licenses: []licenseRecord{}}
		return a.saveLocked()
	}

	if err := json.Unmarshal(raw, &a.data); err != nil {
		return err
	}
	if a.data.Users == nil {
		a.data.Users = []userRecord{}
	}
	if a.data.Licenses == nil {
		a.data.Licenses = []licenseRecord{}
	}
	return nil
}

func (a *app) saveLocked() error {
	raw, err := json.MarshalIndent(a.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.dataFile + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.dataFile)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func decodeJSON(body io.ReadCloser, dst any) error {
	defer body.Close()
	dec := json.NewDecoder(io.LimitReader(body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func getenv(name, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func generateLicenseKey() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	parts := make([]string, 4)
	for i := 0; i < 4; i++ {
		var b strings.Builder
		for j := 0; j < 4; j++ {
			idx := randomInt(len(alphabet))
			b.WriteByte(alphabet[idx])
		}
		parts[i] = b.String()
	}
	return "LF-" + strings.Join(parts, "-")
}

func randomAlphaNum(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[randomInt(len(alphabet))])
	}
	return b.String()
}

func randomInt(max int) int {
	if max <= 1 {
		return 0
	}
	v, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(int64(max)))
	if err != nil {
		// fallback should almost never happen; keep service running
		return int(time.Now().UnixNano() % int64(max))
	}
	return int(v.Int64())
}
