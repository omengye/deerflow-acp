package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const schema = `CREATE TABLE IF NOT EXISTS harness_secrets(
	name TEXT PRIMARY KEY,
	description TEXT NOT NULL DEFAULT '',
	current_version INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	updated_by TEXT NOT NULL DEFAULT '',
	last_used_at TEXT NOT NULL DEFAULT '',
	exposed_at TEXT NOT NULL DEFAULT '',
	deleted_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS harness_secret_versions(
	name TEXT NOT NULL,
	version INTEGER NOT NULL,
	kind TEXT NOT NULL,
	file_name TEXT NOT NULL DEFAULT '',
	key_id TEXT NOT NULL,
	ciphertext BLOB NOT NULL,
	size INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	source TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(name, version)
);`

type VersionInfo struct {
	Version     int64     `json:"version"`
	Kind        Kind      `json:"kind"`
	FileName    string    `json:"file_name,omitempty"`
	Size        int       `json:"size"`
	Fingerprint string    `json:"fingerprint"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	Current     bool      `json:"current,omitempty"`
	Unavailable bool      `json:"unavailable,omitempty"`
	Marker      string    `json:"marker"`
}

// Metadata never contains a value. Fingerprints are keyed HMACs, so they only
// compare values inside this vault and cannot be brute-forced offline.
type Metadata struct {
	Name        string        `json:"name"`
	Kind        Kind          `json:"kind"`
	Description string        `json:"description,omitempty"`
	FileName    string        `json:"file_name,omitempty"`
	Size        int           `json:"size"`
	Fingerprint string        `json:"fingerprint"`
	Version     int64         `json:"version"`
	Source      string        `json:"source"`
	Marker      string        `json:"marker"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	UpdatedBy   string        `json:"updated_by,omitempty"`
	LastUsedAt  *time.Time    `json:"last_used_at,omitempty"`
	ExposedAt   *time.Time    `json:"exposed_at,omitempty"`
	DeletedAt   *time.Time    `json:"deleted_at,omitempty"`
	PurgeAfter  *time.Time    `json:"purge_after,omitempty"`
	Unavailable bool          `json:"unavailable,omitempty"`
	Versions    []VersionInfo `json:"versions,omitempty"`
}

type SetRequest struct {
	Name        string
	Kind        Kind
	Value       []byte
	Description *string // nil keeps the existing description
	FileName    string
	Source      string
	Actor       string
}

// Secret is a resolved value for one injection. Callers must not log it.
type Secret struct {
	Name     string
	Kind     Kind
	Version  int64
	FileName string
	Value    []byte
}

type Vault struct {
	db             *sql.DB
	dir            string
	keyID          string
	encKey         []byte
	macKey         []byte
	sealKey        []byte
	credentialGate chan struct{}
	writes         sync.Mutex // serializes mutations through publication of external configuration references
	mu             sync.RWMutex
	masker         atomic.Pointer[Masker]
	closed         atomic.Bool
	now            func() time.Time
}

func derive(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// Open loads or creates the vault in dir (normally <data>/secrets) and its
// tables in db. If an existing master key cannot be unlocked, Open fails with
// ErrUnavailable instead of silently moving it aside.
func Open(ctx context.Context, db *sql.DB, dir string) (*Vault, error) {
	if db == nil || dir == "" {
		return nil, fmt.Errorf("%w: secret vault requires a database and directory", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	master, err := loadKey(dir)
	if err != nil {
		return nil, err
	}
	if _, err = db.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	v := keyed(db, dir, master)
	if err = v.purge(ctx); err != nil {
		return nil, err
	}
	// No command survives a daemon restart as an owner of injected material.
	_ = os.RemoveAll(filepath.Join(dir, "run"))
	if err = v.rebuild(ctx); err != nil {
		return nil, err
	}
	register(v)
	return v, nil
}

// keyed returns a vault whose keys derive from master, and clears master.
func keyed(db *sql.DB, dir string, master []byte) *Vault {
	v := &Vault{credentialGate: make(chan struct{}, 1), db: db, dir: dir, encKey: derive(master, "deerflow-secret-encrypt-v1"), macKey: derive(master, "deerflow-secret-fingerprint-v1"), sealKey: derive(master, "deerflow-secret-seal-v1"), now: time.Now}
	v.keyID = hex.EncodeToString(derive(master, "deerflow-secret-key-id-v1"))[:16]
	clear(master)
	return v
}

func loadKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, "master.key")
	if err := checkKeyFile(path); err == nil {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		key, openErr := openKey(raw)
		if openErr == nil && len(key) == 32 {
			return key, nil
		}
		clear(key)
		return nil, fmt.Errorf("%w: existing master.key cannot be unlocked (kept untouched for recovery; remove it only if intentionally resetting the vault)", ErrUnavailable)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	sealed, err := sealKey(key)
	if err != nil {
		clear(key)
		return nil, err
	}
	temporary := path + ".tmp-" + rand.Text()
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		clear(key)
		return nil, err
	}
	_, err = file.Write(sealed)
	err = errors.Join(err, file.Sync(), file.Close())
	if err == nil {
		err = os.Rename(temporary, path)
	}
	if err != nil {
		clear(key)
		_ = os.Remove(temporary)
		return nil, err
	}
	return key, nil
}

// Close unregisters the vault's masker. It does not close the shared database.
func (v *Vault) Close() error {
	if v == nil || v.closed.Swap(true) {
		return nil
	}
	unregister(v)
	return nil
}

// Dir is the private vault directory; command material lives under Dir/run.
func (v *Vault) Dir() string { return v.dir }

// Masker returns the current immutable masker of every retained value.
func (v *Vault) Masker() *Masker {
	if v == nil {
		return nil
	}
	return v.masker.Load()
}

// Fixed-width UTC timestamps keep lexical SQL comparisons chronological.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func (v *Vault) timestamp() string { return v.now().UTC().Format(timeLayout) }

func parseTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &t
}

func (v *Vault) aad(name string, version int64) []byte {
	return []byte("deerflow-secret-v1\x00" + name + "\x00" + strconv.FormatInt(version, 10))
}

func aead(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (v *Vault) encrypt(name string, version int64, plaintext []byte) ([]byte, error) {
	gcm, err := aead(v.encKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, v.aad(name, version)), nil
}

func (v *Vault) decrypt(name string, version int64, keyID string, data []byte) ([]byte, error) {
	if keyID != v.keyID {
		return nil, ErrUnavailable
	}
	gcm, err := aead(v.encKey)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, ErrUnavailable
	}
	plaintext, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], v.aad(name, version))
	if err != nil {
		return nil, ErrUnavailable
	}
	return plaintext, nil
}

// Fingerprint is a keyed, truncated HMAC usable to compare values.
func (v *Vault) Fingerprint(value []byte) string {
	mac := hmac.New(sha256.New, v.macKey)
	mac.Write(value)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))[:16]
}

// Seal encrypts a value staged in tool arguments. It is deterministic for the
// same binding and value, so a retried preparation produces identical durable
// arguments; the binding (tool call and name) prevents replay elsewhere.
func (v *Vault) Seal(binding string, value []byte) (string, error) {
	gcm, err := aead(derive(v.sealKey, "encrypt"))
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, derive(v.sealKey, "nonce"))
	mac.Write([]byte(binding))
	mac.Write([]byte{0})
	mac.Write(value)
	nonce := mac.Sum(nil)[:gcm.NonceSize()]
	sealed := gcm.Seal(append([]byte(nil), nonce...), nonce, value, []byte("deerflow-secret-seal-v1\x00"+binding))
	return "v1." + v.keyID + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (v *Vault) Unseal(binding, sealed string) ([]byte, error) {
	parts := strings.Split(sealed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, fmt.Errorf("%w: invalid sealed value", ErrInvalid)
	}
	if parts[1] != v.keyID {
		// Seals live for one tool call, so another key means another host.
		return nil, fmt.Errorf("%w: sealed value was not written by this vault", ErrInvalid)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: invalid sealed value", ErrInvalid)
	}
	gcm, err := aead(derive(v.sealKey, "encrypt"))
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: invalid sealed value", ErrInvalid)
	}
	value, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte("deerflow-secret-seal-v1\x00"+binding))
	if err != nil {
		return nil, fmt.Errorf("%w: sealed value does not belong to this tool call", ErrInvalid)
	}
	return value, nil
}

// ValidFileName reports whether name can be used as a file secret's file name.
func ValidFileName(name string) error { return validateFileName(name) }

func validateFileName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 128 || name == "." || name == ".." || strings.ContainsAny(name, `/\:*?"<>|`) || strings.HasPrefix(name, ".") && len(strings.Trim(name, ".")) == 0 {
		return fmt.Errorf("%w: file_name must be a plain file name", ErrInvalid)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: file_name contains control characters", ErrInvalid)
		}
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: file_name cannot end with a space or dot", ErrInvalid)
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch {
	case stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL",
		len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9':
		return fmt.Errorf("%w: file_name %q is a reserved device name on Windows", ErrInvalid, name)
	}
	return nil
}

func validateDescription(text string) error {
	if len(text) > MaxDescriptionBytes || !utf8.ValidString(text) || strings.ContainsAny(text, "\x00\r\n") {
		return fmt.Errorf("%w: description must be one line of at most %d bytes", ErrInvalid, MaxDescriptionBytes)
	}
	return nil
}

func validateValue(kind Kind, value []byte) error {
	switch kind {
	case KindString:
		if len(value) == 0 || len(value) > MaxStringBytes {
			return fmt.Errorf("%w: string secrets must be 1..%d bytes", ErrInvalid, MaxStringBytes)
		}
		if !utf8.Valid(value) || strings.ContainsRune(string(value), 0) {
			return fmt.Errorf("%w: string secrets must be UTF-8 text without NUL; store binary data as kind=file", ErrInvalid)
		}
	case KindFile:
		if len(value) == 0 || len(value) > MaxFileBytes {
			return fmt.Errorf("%w: file secrets must be 1..%d bytes", ErrInvalid, MaxFileBytes)
		}
	default:
		return fmt.Errorf("%w: kind must be string or file", ErrInvalid)
	}
	return nil
}

func (v *Vault) usable() error {
	if v == nil {
		return ErrClosed
	}
	if v.closed.Load() {
		return ErrClosed
	}
	return nil
}

// Set stores a new current version, reviving a soft-deleted secret.
func (v *Vault) Set(ctx context.Context, req SetRequest) (Metadata, error) {
	if err := v.usable(); err != nil {
		return Metadata{}, err
	}
	w, release := v.BeginImport()
	defer release()
	return w.Set(ctx, req)
}

func (w *ImportWriter) Set(ctx context.Context, req SetRequest) (Metadata, error) {
	v := w.v
	if IsCredentialName(req.Name) {
		return Metadata{}, ErrInvalid
	}
	if err := validateSetRequest(req); err != nil {
		return Metadata{}, err
	}
	if req.Source == "" {
		req.Source = "value"
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.setLocked(ctx, req)
}

func validateSetRequest(req SetRequest) error {
	if err := validateName(req.Name); err != nil {
		return err
	}
	if err := validateValue(req.Kind, req.Value); err != nil {
		return err
	}
	if err := validateFileName(req.FileName); err != nil {
		return err
	}
	if req.Kind == KindString && req.FileName != "" {
		return fmt.Errorf("%w: file_name applies only to kind=file", ErrInvalid)
	}
	if req.Description != nil {
		if err := validateDescription(*req.Description); err != nil {
			return err
		}
	}
	return nil
}

// writeVersion writes all rows for one version in the caller's transaction.
func (v *Vault) writeVersion(ctx context.Context, tx *sql.Tx, req SetRequest, createOnly bool) error {
	now := v.timestamp()
	var exists bool
	var description string
	err := tx.QueryRowContext(ctx, `SELECT description FROM harness_secrets WHERE name=?`, req.Name).Scan(&description)
	switch {
	case err == nil:
		if createOnly {
			return ErrImportConflict
		}
		exists = true
	case errors.Is(err, sql.ErrNoRows):
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_secrets WHERE deleted_at=''`).Scan(&count); err != nil {
			return err
		}
		if count >= MaxSecrets {
			return fmt.Errorf("%w: the vault already holds %d secrets; delete unused ones first", ErrInvalid, MaxSecrets)
		}
	default:
		return err
	}
	if req.Description != nil {
		description = *req.Description
	}
	var latest int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM harness_secret_versions WHERE name=?`, req.Name).Scan(&latest); err != nil {
		return err
	}
	version := latest + 1
	ciphertext, err := v.encrypt(req.Name, version, req.Value)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_secret_versions(name,version,kind,file_name,key_id,ciphertext,size,fingerprint,source,created_at,created_by) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		req.Name, version, string(req.Kind), req.FileName, v.keyID, ciphertext, len(req.Value), v.Fingerprint(req.Value), req.Source, now, req.Actor); err != nil {
		return err
	}
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE harness_secrets SET description=?,current_version=?,updated_at=?,updated_by=?,exposed_at='',deleted_at='' WHERE name=?`, description, version, now, req.Actor, req.Name)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_secrets(name,description,current_version,created_at,updated_at,updated_by) VALUES(?,?,?,?,?,?)`, req.Name, description, version, now, now, req.Actor)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_secret_versions WHERE name=? AND version<=?`, req.Name, version-RetainedVersions); err != nil {
		return err
	}
	return nil
}

func (v *Vault) setLocked(ctx context.Context, req SetRequest) (Metadata, error) {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return Metadata{}, err
	}
	defer tx.Rollback()
	if err = v.writeVersion(ctx, tx, req, false); err != nil {
		return Metadata{}, err
	}
	if err = tx.Commit(); err != nil {
		return Metadata{}, err
	}
	if err = v.rebuild(context.WithoutCancel(ctx)); err != nil {
		return Metadata{}, err
	}
	return v.metadata(ctx, req.Name, false)
}

// Delete soft-deletes a secret. Its values stay masked and restorable until
// the retention period ends.
func (v *Vault) Delete(ctx context.Context, name, actor string) (Metadata, error) {
	if err := v.usable(); err != nil {
		return Metadata{}, err
	}
	v.writes.Lock()
	defer v.writes.Unlock()
	if err := validateName(name); err != nil {
		return Metadata{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	result, err := v.db.ExecContext(ctx, `UPDATE harness_secrets SET deleted_at=?,updated_at=?,updated_by=? WHERE name=? AND deleted_at=''`, v.timestamp(), v.timestamp(), actor, name)
	if err != nil {
		return Metadata{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Metadata{}, ErrNotFound
	}
	if err = v.rebuild(context.WithoutCancel(ctx)); err != nil {
		return Metadata{}, err
	}
	return v.metadata(ctx, name, true)
}

// Restore undeletes a secret and, with version > 0, makes that retained
// version current again as a new version (history stays linear).
func (v *Vault) Restore(ctx context.Context, name string, version int64, actor string) (Metadata, error) {
	if err := v.usable(); err != nil {
		return Metadata{}, err
	}
	v.writes.Lock()
	defer v.writes.Unlock()
	if err := validateName(name); err != nil {
		return Metadata{}, err
	}
	if IsCredentialName(name) {
		return Metadata{}, fmt.Errorf("%w: enroll scoped credentials together in the credential form", ErrInvalid)
	}
	if version < 0 {
		return Metadata{}, fmt.Errorf("%w: version must be positive", ErrInvalid)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var current int64
	var deleted string
	err := v.db.QueryRowContext(ctx, `SELECT current_version,deleted_at FROM harness_secrets WHERE name=?`, name).Scan(&current, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, err
	}
	if version == 0 || version == current {
		if deleted == "" {
			return Metadata{}, fmt.Errorf("%w: secret is not deleted; pass an older version to roll back", ErrInvalid)
		}
		_, err = v.db.ExecContext(ctx, `UPDATE harness_secrets SET deleted_at='',updated_at=?,updated_by=? WHERE name=?`, v.timestamp(), actor, name)
		if err == nil {
			err = v.rebuild(context.WithoutCancel(ctx))
		}
		if err != nil {
			return Metadata{}, err
		}
		return v.metadata(ctx, name, false)
	}
	var kind, fileName, keyID string
	var ciphertext []byte
	err = v.db.QueryRowContext(ctx, `SELECT kind,file_name,key_id,ciphertext FROM harness_secret_versions WHERE name=? AND version=?`, name, version).Scan(&kind, &fileName, &keyID, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return Metadata{}, fmt.Errorf("%w: version %d is not retained", ErrNotFound, version)
	}
	if err != nil {
		return Metadata{}, err
	}
	value, err := v.decrypt(name, version, keyID, ciphertext)
	if err != nil {
		return Metadata{}, err
	}
	defer clear(value)
	return v.setLocked(ctx, SetRequest{Name: name, Kind: Kind(kind), Value: value, FileName: fileName, Source: "restore:v" + strconv.FormatInt(version, 10), Actor: actor})
}

// Reveal returns the current plaintext and records that it left the vault.
// Intended for host-owned UI or CLI inspection only; never expose to models.
func (v *Vault) Reveal(ctx context.Context, name, actor string) (Metadata, []byte, error) {
	if err := v.usable(); err != nil {
		return Metadata{}, nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	secrets, err := v.resolve(ctx, []string{name}, false)
	if err != nil {
		return Metadata{}, nil, err
	}
	secret := secrets[name]
	now := v.timestamp()
	if _, err = v.db.ExecContext(ctx, `UPDATE harness_secrets SET exposed_at=?,last_used_at=? WHERE name=?`, now, now, name); err != nil {
		clear(secret.Value)
		return Metadata{}, nil, err
	}
	meta, err := v.metadata(ctx, name, false)
	if err != nil {
		clear(secret.Value)
		return Metadata{}, nil, err
	}
	return meta, secret.Value, nil
}

// Resolve decrypts the current versions of names for one injection and
// records their use.
func (v *Vault) Resolve(ctx context.Context, names []string) (map[string]Secret, error) {
	if err := v.usable(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.resolve(ctx, names, true)
}

func (v *Vault) resolve(ctx context.Context, names []string, touch bool) (map[string]Secret, error) {
	out := make(map[string]Secret, len(names))
	clearOut := func() {
		for _, secret := range out {
			clear(secret.Value)
		}
	}
	var missing []string
	for _, name := range names {
		if !permittedCredential(ctx, name) {
			clearOut()
			return nil, harness.ErrPermissionDenied
		}
		if _, done := out[name]; done {
			continue
		}
		if err := validateName(name); err != nil {
			clearOut()
			return nil, err
		}
		var version int64
		var kind, fileName, keyID string
		var ciphertext []byte
		err := v.db.QueryRowContext(ctx, `SELECT s.current_version,v.kind,v.file_name,v.key_id,v.ciphertext FROM harness_secrets s JOIN harness_secret_versions v ON v.name=s.name AND v.version=s.current_version WHERE s.name=? AND s.deleted_at=''`, name).Scan(&version, &kind, &fileName, &keyID, &ciphertext)
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			clearOut()
			return nil, err
		}
		value, err := v.decrypt(name, version, keyID, ciphertext)
		if err != nil {
			clearOut()
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		out[name] = Secret{Name: name, Kind: Kind(kind), Version: version, FileName: fileName, Value: value}
	}
	if len(missing) > 0 {
		clearOut()
		sort.Strings(missing)
		return nil, &MissingError{Names: missing}
	}
	if touch && len(out) > 0 {
		now := v.timestamp()
		for name := range out {
			if _, err := v.db.ExecContext(ctx, `UPDATE harness_secrets SET last_used_at=? WHERE name=?`, now, name); err != nil {
				clearOut()
				return nil, err
			}
		}
	}
	return out, nil
}

// Missing returns the names that cannot be injected now: absent, deleted or
// encrypted under a vault key that is no longer available.
func (v *Vault) Missing(ctx context.Context, names []string) ([]string, error) {
	if err := v.usable(); err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range names {
		meta, err := v.Get(ctx, name, false)
		switch {
		case errors.Is(err, ErrNotFound), err == nil && (meta.DeletedAt != nil || meta.Unavailable):
			missing = append(missing, name)
		case err != nil:
			return nil, err
		}
	}
	return missing, nil
}

// MissingError lists names that do not exist or are deleted.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return "secrets not found: " + strings.Join(e.Names, ", ") + "; for a username or key use the credential form by specifying the tool's credential binding"
}
func (e *MissingError) Unwrap() error { return ErrNotFound }

func (v *Vault) List(ctx context.Context, includeDeleted bool) ([]Metadata, error) {
	if err := v.usable(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	query := `SELECT s.name FROM harness_secrets s`
	if !includeDeleted {
		query += ` WHERE s.deleted_at=''`
	}
	rows, err := v.db.QueryContext(ctx, query+` ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]Metadata, 0, len(names))
	for _, name := range names {
		meta, err := v.metadata(ctx, name, false)
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, nil
}

// Get returns metadata, and with history the retained versions.
func (v *Vault) Get(ctx context.Context, name string, history bool) (Metadata, error) {
	if err := v.usable(); err != nil {
		return Metadata{}, err
	}
	if err := validateName(name); err != nil {
		return Metadata{}, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.metadata(ctx, name, history)
}

func (v *Vault) metadata(ctx context.Context, name string, history bool) (Metadata, error) {
	var m Metadata
	var created, updated, used, exposed, deleted string
	err := v.db.QueryRowContext(ctx, `SELECT name,description,current_version,created_at,updated_at,updated_by,last_used_at,exposed_at,deleted_at FROM harness_secrets WHERE name=?`, name).
		Scan(&m.Name, &m.Description, &m.Version, &created, &updated, &m.UpdatedBy, &used, &exposed, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, err
	}
	if t := parseTime(created); t != nil {
		m.CreatedAt = *t
	}
	if t := parseTime(updated); t != nil {
		m.UpdatedAt = *t
	}
	m.LastUsedAt, m.ExposedAt, m.DeletedAt = parseTime(used), parseTime(exposed), parseTime(deleted)
	if m.DeletedAt != nil {
		purge := m.DeletedAt.Add(DeletedRetentionDays * 24 * time.Hour)
		m.PurgeAfter = &purge
	}
	rows, err := v.db.QueryContext(ctx, `SELECT version,kind,file_name,key_id,size,fingerprint,source,created_at,created_by FROM harness_secret_versions WHERE name=? ORDER BY version DESC`, name)
	if err != nil {
		return Metadata{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var info VersionInfo
		var kind, keyID, at string
		if err = rows.Scan(&info.Version, &kind, &info.FileName, &keyID, &info.Size, &info.Fingerprint, &info.Source, &at, &info.CreatedBy); err != nil {
			return Metadata{}, err
		}
		info.Kind, info.Unavailable = Kind(kind), keyID != v.keyID
		if t := parseTime(at); t != nil {
			info.CreatedAt = *t
		}
		info.Current = info.Version == m.Version
		info.Marker = markerBase(name, info.Version, info.Current && m.DeletedAt == nil) + "]"
		if info.Current {
			m.Kind, m.FileName, m.Size, m.Fingerprint, m.Source, m.Unavailable, m.Marker = info.Kind, info.FileName, info.Size, info.Fingerprint, info.Source, info.Unavailable, info.Marker
		}
		if history {
			m.Versions = append(m.Versions, info)
		}
	}
	return m, rows.Err()
}

func (v *Vault) purge(ctx context.Context) error {
	cutoff := v.now().UTC().Add(-DeletedRetentionDays * 24 * time.Hour).Format(timeLayout)
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_secret_versions WHERE name IN (SELECT name FROM harness_secrets WHERE deleted_at<>'' AND deleted_at<?)`, cutoff); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_secrets WHERE deleted_at<>'' AND deleted_at<?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// rebuild decrypts every retained version into a fresh masker. Current values
// of live secrets claim their plain marker first.
func (v *Vault) rebuild(ctx context.Context) error {
	rows, err := v.db.QueryContext(ctx, `SELECT v.name,v.version,v.key_id,v.ciphertext,s.current_version,s.deleted_at FROM harness_secret_versions v JOIN harness_secrets s ON s.name=v.name ORDER BY (v.version=s.current_version AND s.deleted_at='') DESC,v.name,v.version DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	builder := newMaskBuilder()
	for rows.Next() {
		var entry maskEntry
		var keyID, deleted string
		var ciphertext []byte
		var current int64
		if err = rows.Scan(&entry.name, &entry.version, &keyID, &ciphertext, &current, &deleted); err != nil {
			return err
		}
		value, decryptErr := v.decrypt(entry.name, entry.version, keyID, ciphertext)
		if decryptErr != nil {
			continue
		}
		entry.value, entry.current = value, entry.version == current && deleted == ""
		builder.add(entry)
		clear(value)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	v.masker.Store(builder.finish())
	return nil
}
