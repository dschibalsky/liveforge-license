package main

import (
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	MachineID   string `json:"machine_id"`
	ActivatedAt string `json:"activated_at"`
	LastSeenAt  string `json:"last_seen_at"`
}

type licenseRecord struct {
	Key         string `json:"key"`
	KeyLast4    string `json:"key_last4"`
	UserID      string `json:"user_id"`
	Status      string `json:"status"`
	MaxDevices  int    `json:"max_devices"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	BillingMode string `json:"billing_mode,omitempty"`
	// AllowedEdition: full (default) or cloud — must match app build when enforced client-side.
	AllowedEdition string `json:"allowed_edition,omitempty"`
	// WalletBalanceUSD used when BillingMode is hosted (prepaid inference balance).
	WalletBalanceUSD float64            `json:"wallet_balance_usd"`
	ModelTier        string             `json:"model_tier,omitempty"`
	Activations      []activationRecord `json:"activations"`
	// ConsumedTraceIDs deduplicates POST /api/keys/consume (idempotent wallet debits).
	ConsumedTraceIDs []string `json:"consumed_trace_ids,omitempty"`
}

// hostedTierTriple is OpenRouter model ids for main / compose / utility (hosted billing).
type hostedTierTriple struct {
	Model        string `json:"model"`
	ComposeModel string `json:"compose_model"`
	UtilityModel string `json:"utility_model"`
}

type dbData struct {
	Users             []userRecord               `json:"users"`
	Licenses          []licenseRecord            `json:"licenses"`
	HostedTierPresets map[string]hostedTierTriple `json:"hosted_tier_presets,omitempty"`
}

func defaultHostedTierPresets() map[string]hostedTierTriple {
	// Keep in sync with gui/app/lib/tier-presets.ts HOSTED_OPENROUTER_TIER_PRESETS.
	return map[string]hostedTierTriple{
		"t1": {
			Model:        "google/gemini-2.5-flash",
			ComposeModel: "google/gemini-2.5-flash",
			UtilityModel: "google/gemini-2.5-flash",
		},
		"t2": {
			Model:        "google/gemini-3-flash-preview",
			ComposeModel: "google/gemini-3-flash-preview",
			UtilityModel: "google/gemini-2.5-flash",
		},
		"t3": {
			Model:        "anthropic/claude-sonnet-4-6",
			ComposeModel: "anthropic/claude-sonnet-4-6",
			UtilityModel: "google/gemini-2.5-pro",
		},
	}
}

func tripleOK(t hostedTierTriple) bool {
	return strings.TrimSpace(t.Model) != "" &&
		strings.TrimSpace(t.ComposeModel) != "" &&
		strings.TrimSpace(t.UtilityModel) != ""
}

// ensureHostedTierPresets fills missing or invalid t1/t2/t3 entries; returns true if db should be saved.
func ensureHostedTierPresets(d *dbData) bool {
	changed := false
	if d.HostedTierPresets == nil {
		d.HostedTierPresets = make(map[string]hostedTierTriple)
		changed = true
	}
	for tier, def := range defaultHostedTierPresets() {
		cur, ok := d.HostedTierPresets[tier]
		if !ok || !tripleOK(cur) {
			d.HostedTierPresets[tier] = def
			changed = true
		}
	}
	return changed
}

func hostedTierPresetsJSON(p map[string]hostedTierTriple) map[string]any {
	if len(p) == 0 {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = map[string]any{
			"model":         v.Model,
			"compose_model": v.ComposeModel,
			"utility_model": v.UtilityModel,
		}
	}
	return out
}

type app struct {
	mu                 sync.Mutex
	dataFile           string
	updatesDir         string
	updatesUploadToken string
	consumeSecret      string
	maxUploadBytes     int64
	data               dbData
	tpl                *template.Template
}

func main() {
	addr := getenv("ADDR", ":8085")
	dataFile := getenv("DATA_FILE", "./data/license-db.json")
	updatesDir := getenv("UPDATES_DIR", "./updates")
	updatesUploadToken := getenv("UPDATES_UPLOAD_TOKEN", "")
	maxUploadMB := getenv("UPDATES_MAX_UPLOAD_MB", "2048")
	maxUploadBytes := int64(2 * 1024 * 1024 * 1024)
	if strings.TrimSpace(maxUploadMB) != "" {
		if mb, err := strconv.ParseInt(strings.TrimSpace(maxUploadMB), 10, 64); err == nil && mb > 0 {
			maxUploadBytes = mb * 1024 * 1024
		}
	}

	tpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	a := &app{
		dataFile:           dataFile,
		updatesDir:         updatesDir,
		updatesUploadToken: updatesUploadToken,
		consumeSecret:      strings.TrimSpace(os.Getenv("CONSUME_SECRET")),
		maxUploadBytes:     maxUploadBytes,
		tpl:                tpl,
	}
	if err := a.load(); err != nil {
		log.Fatalf("load db: %v", err)
	}
	if err := os.MkdirAll(a.updatesDir, 0o755); err != nil {
		log.Fatalf("prepare updates dir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/users", a.handleUsers)
	mux.HandleFunc("/api/keys", a.handleKeys)
	mux.HandleFunc("/api/keys/verify", a.handleVerifyKey)
	mux.HandleFunc("/api/keys/consume", a.handleConsumeWallet)
	mux.HandleFunc("/api/keys/revoke", a.handleKeyRevoke)
	mux.HandleFunc("/api/keys/delete", a.handleKeyDelete)
	mux.HandleFunc("/api/keys/reset-activations", a.handleKeyResetActivations)
	mux.HandleFunc("/api/keys/patch", a.handleKeyPatch)
	mux.HandleFunc("/api/users/delete", a.handleUserDelete)
	mux.HandleFunc("/api/updates/upload", a.handleUploadUpdateAsset)
	mux.HandleFunc("/api/updates/files", a.handleListUpdateAssets)
	mux.Handle("/updates/",
		http.StripPrefix(
			"/updates/",
			http.FileServer(http.Dir(a.updatesDir)),
		),
	)

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

func (a *app) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "id_required"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, lic := range a.data.Licenses {
		if lic.UserID == id {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "user_has_licenses"})
			return
		}
	}
	idx := -1
	for i, u := range a.data.Users {
		if u.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "user_not_found"})
		return
	}
	a.data.Users = append(a.data.Users[:idx], a.data.Users[idx+1:]...)
	if err := a.saveLocked(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
			UserID           string   `json:"user_id"`
			MaxDevices       int      `json:"max_devices"`
			ExpiresAt        string   `json:"expires_at"`
			BillingMode      string   `json:"billing_mode"`
			AllowedEdition   string   `json:"allowed_edition"`
			WalletBalanceUSD *float64 `json:"wallet_balance_usd"`
			ModelTier        string   `json:"model_tier"`
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

		bm := strings.ToLower(strings.TrimSpace(req.BillingMode))
		if bm == "" {
			bm = "byok"
		}
		if bm != "byok" && bm != "hosted" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_billing_mode"})
			return
		}
		ed := strings.ToLower(strings.TrimSpace(req.AllowedEdition))
		if ed == "" {
			ed = "full"
		}
		if ed != "full" && ed != "cloud" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_allowed_edition"})
			return
		}
		tier := strings.ToLower(strings.TrimSpace(req.ModelTier))
		if tier != "" && tier != "t1" && tier != "t2" && tier != "t3" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_model_tier"})
			return
		}
		wallet := 0.0
		if req.WalletBalanceUSD != nil {
			wallet = *req.WalletBalanceUSD
		}
		if wallet < 0 || math.IsNaN(wallet) || math.IsInf(wallet, 0) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_wallet_balance"})
			return
		}

		key := generateLicenseKey()
		rec := licenseRecord{
			Key:              key,
			KeyLast4:         key[len(key)-4:],
			UserID:           req.UserID,
			Status:           "active",
			MaxDevices:       req.MaxDevices,
			CreatedAt:        nowISO(),
			ExpiresAt:        req.ExpiresAt,
			BillingMode:      bm,
			AllowedEdition:   ed,
			WalletBalanceUSD: wallet,
			ModelTier:        tier,
			Activations:      []activationRecord{},
			ConsumedTraceIDs: []string{},
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
		"ok":           true,
		"license":      a.licenseVerifyJSON(rec),
		"validated_at": nowISO(),
	})
}

func (a *app) licenseVerifyJSON(rec *licenseRecord) map[string]any {
	out := map[string]any{
		"key_last4":       rec.KeyLast4,
		"user_id":         rec.UserID,
		"status":          rec.Status,
		"max_devices":     rec.MaxDevices,
		"active_devices":  len(rec.Activations),
		"expires_at":      rec.ExpiresAt,
		"billing_mode":    rec.BillingMode,
		"allowed_edition": rec.AllowedEdition,
	}
	if strings.EqualFold(rec.BillingMode, "hosted") {
		out["wallet_balance_usd"] = rec.WalletBalanceUSD
		if j := hostedTierPresetsJSON(a.data.HostedTierPresets); j != nil {
			out["hosted_tier_presets"] = j
		}
	}
	if rec.ModelTier != "" {
		out["model_tier"] = rec.ModelTier
	}
	return out
}

const maxConsumedTraceIDsPerLicense = 2500

func (a *app) consumeAuthOK(r *http.Request) bool {
	if strings.TrimSpace(a.consumeSecret) == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("x-liveforge-consume-secret"))
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.consumeSecret)) == 1
}

func (a *app) handleConsumeWallet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.consumeAuthOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized_consume"})
		return
	}
	var req struct {
		LicenseKey      string  `json:"license_key"`
		MachineID       string  `json:"machine_id"`
		TraceID         string  `json:"trace_id"`
		SessionID       string  `json:"session_id"` // accepted for logging compatibility; not persisted here
		ProviderCostUSD float64 `json:"provider_cost_usd"`
		WalletDebitUSD  float64 `json:"wallet_debit_usd"`
		Estimated       bool    `json:"estimated"`
		BillingMode     string  `json:"billing_mode"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	req.LicenseKey = strings.ToUpper(strings.TrimSpace(req.LicenseKey))
	req.MachineID = strings.TrimSpace(req.MachineID)
	req.TraceID = strings.TrimSpace(req.TraceID)
	if req.LicenseKey == "" || req.MachineID == "" || req.TraceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing_fields"})
		return
	}
	if req.WalletDebitUSD < 0 || math.IsNaN(req.WalletDebitUSD) || math.IsInf(req.WalletDebitUSD, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_wallet_debit"})
		return
	}
	if strings.ToLower(strings.TrimSpace(req.BillingMode)) != "hosted" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "billing_mode_must_be_hosted"})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	idx := -1
	for i := range a.data.Licenses {
		if strings.EqualFold(a.data.Licenses[i].Key, req.LicenseKey) {
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
	if !strings.EqualFold(rec.BillingMode, "hosted") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "license_not_hosted"})
		return
	}
	activated := false
	for _, act := range rec.Activations {
		if act.MachineID == req.MachineID {
			activated = true
			break
		}
	}
	if !activated {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "machine_not_activated"})
		return
	}
	for _, t := range rec.ConsumedTraceIDs {
		if t == req.TraceID {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "idempotent": true, "license": a.licenseVerifyJSON(rec)})
			return
		}
	}
	if req.WalletDebitUSD > rec.WalletBalanceUSD+1e-9 {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{"ok": false, "error": "insufficient_balance"})
		return
	}
	rec.WalletBalanceUSD = math.Round((rec.WalletBalanceUSD-req.WalletDebitUSD)*1e6) / 1e6
	rec.ConsumedTraceIDs = append(rec.ConsumedTraceIDs, req.TraceID)
	if len(rec.ConsumedTraceIDs) > maxConsumedTraceIDsPerLicense {
		rec.ConsumedTraceIDs = rec.ConsumedTraceIDs[len(rec.ConsumedTraceIDs)-maxConsumedTraceIDsPerLicense:]
	}
	if err := a.saveLocked(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "license": a.licenseVerifyJSON(rec)})
}

func (a *app) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.applyKeyMutation(w, r, func(rec *licenseRecord) {
		rec.Status = "revoked"
	})
}

func (a *app) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	key := strings.ToUpper(strings.TrimSpace(req.Key))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "key_required"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := -1
	for i := range a.data.Licenses {
		if strings.EqualFold(a.data.Licenses[i].Key, key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "key_not_found"})
		return
	}
	a.data.Licenses = append(a.data.Licenses[:idx], a.data.Licenses[idx+1:]...)
	if err := a.saveLocked(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleKeyResetActivations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.applyKeyMutation(w, r, func(rec *licenseRecord) {
		rec.Activations = []activationRecord{}
	})
}

func (a *app) handleKeyPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key              string   `json:"key"`
		Status           string   `json:"status"`
		MaxDevices       *int     `json:"max_devices"`
		WalletBalanceUSD *float64 `json:"wallet_balance_usd"`
		BillingMode      string   `json:"billing_mode"`
		AllowedEdition   string   `json:"allowed_edition"`
		ModelTier        string   `json:"model_tier"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	key := strings.ToUpper(strings.TrimSpace(req.Key))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "key_required"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := -1
	for i := range a.data.Licenses {
		if strings.EqualFold(a.data.Licenses[i].Key, key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "key_not_found"})
		return
	}
	rec := &a.data.Licenses[idx]
	if s := strings.TrimSpace(req.Status); s != "" {
		if s != "active" && s != "revoked" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_status"})
			return
		}
		rec.Status = s
	}
	if req.MaxDevices != nil {
		if *req.MaxDevices < 1 || *req.MaxDevices > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_max_devices"})
			return
		}
		rec.MaxDevices = *req.MaxDevices
	}
	if req.WalletBalanceUSD != nil {
		v := *req.WalletBalanceUSD
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_wallet_balance"})
			return
		}
		rec.WalletBalanceUSD = v
	}
	if bm := strings.ToLower(strings.TrimSpace(req.BillingMode)); bm != "" {
		if bm != "byok" && bm != "hosted" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_billing_mode"})
			return
		}
		rec.BillingMode = bm
	}
	if ed := strings.ToLower(strings.TrimSpace(req.AllowedEdition)); ed != "" {
		if ed != "full" && ed != "cloud" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_allowed_edition"})
			return
		}
		rec.AllowedEdition = ed
	}
	if tier := strings.ToLower(strings.TrimSpace(req.ModelTier)); tier != "" {
		if tier != "t1" && tier != "t2" && tier != "t3" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_model_tier"})
			return
		}
		rec.ModelTier = tier
	}
	if err := a.saveLocked(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": *rec})
}

func (a *app) applyKeyMutation(w http.ResponseWriter, r *http.Request, fn func(rec *licenseRecord)) {
	var req struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	key := strings.ToUpper(strings.TrimSpace(req.Key))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "key_required"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := -1
	for i := range a.data.Licenses {
		if strings.EqualFold(a.data.Licenses[i].Key, key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "key_not_found"})
		return
	}
	rec := &a.data.Licenses[idx]
	fn(rec)
	if err := a.saveLocked(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": *rec})
}

func (a *app) hasValidUpdateUploadToken(r *http.Request) bool {
	if strings.TrimSpace(a.updatesUploadToken) == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("x-updates-upload-token"))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.updatesUploadToken)) == 1
}

func sanitizeUploadName(raw string) string {
	name := strings.TrimSpace(filepath.Base(raw))
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return ""
	}
	return name
}

func (a *app) handleUploadUpdateAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.hasValidUpdateUploadToken(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "unauthorized_upload_token",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.maxUploadBytes)
	if err := r.ParseMultipartForm(a.maxUploadBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "invalid_multipart_or_file_too_large",
		})
		return
	}

	src, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "missing_file",
		})
		return
	}
	defer src.Close()

	name := sanitizeUploadName(hdr.Filename)
	if custom := sanitizeUploadName(r.FormValue("name")); custom != "" {
		name = custom
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "invalid_filename",
		})
		return
	}

	target := filepath.Join(a.updatesDir, name)
	tmp := target + ".uploading"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "open_target_failed",
		})
		return
	}
	n, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "write_failed",
		})
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "finalize_failed",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"file":     name,
		"bytes":    n,
		"url_path": "/updates/" + name,
	})
}

func (a *app) handleListUpdateAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.hasValidUpdateUploadToken(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "unauthorized_upload_token",
		})
		return
	}
	entries, err := os.ReadDir(a.updatesDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "list_failed",
		})
		return
	}
	files := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]any{
			"name":        e.Name(),
			"bytes":       info.Size(),
			"modified_at": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"files": files,
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
	migrateLicenses(&a.data.Licenses)
	if ensureHostedTierPresets(&a.data) {
		if err := a.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

func migrateLicenses(lics *[]licenseRecord) {
	for i := range *lics {
		rec := &(*lics)[i]
		if rec.BillingMode == "" {
			rec.BillingMode = "byok"
		}
		if rec.AllowedEdition == "" {
			rec.AllowedEdition = "full"
		}
		if rec.ConsumedTraceIDs == nil {
			rec.ConsumedTraceIDs = []string{}
		}
	}
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
