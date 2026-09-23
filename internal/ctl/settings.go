package ctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
)

// Environment variables read by `savior ctl`.
const (
	EnvHive        = "SAVIOR_HIVE"
	EnvFingerprint = "SAVIOR_FINGERPRINT"
	EnvAdminToken  = "SAVIOR_ADMIN_TOKEN"
	EnvConfig      = "SAVIOR_CTL_CONFIG" // alternative ctl.json path
)

const (
	configDirName  = "savior"
	configFileName = "ctl.json"
	maxConfigFile  = 64 << 10
)

// fileConfig is ctl.json: the remembered hive, its verified fingerprint and
// the current admin session. The admin token is never stored.
type fileConfig struct {
	Hive           string    `json:"hive,omitempty"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
	Session        string    `json:"session,omitempty"`
	SessionExpires time.Time `json:"session_expires,omitzero"`
}

// defaultConfigPath returns <UserConfigDir>/savior/ctl.json.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user config directory: %w", err)
	}
	return filepath.Join(dir, configDirName, configFileName), nil
}

// loadFileConfig reads ctl.json. A missing file is an empty config. Invalid
// fields are dropped with a warning rather than failing every command.
func loadFileConfig(path string) (fileConfig, []string, error) {
	var fc fileConfig
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fc, nil, nil
	}
	if err != nil {
		return fc, nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	var warnings []string
	if st, err := f.Stat(); err == nil && runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err == nil {
			warnings = append(warnings, fmt.Sprintf("%s was readable by other users; its mode is now 0600", path))
		} else {
			warnings = append(warnings, fmt.Sprintf("%s is readable by other users; make it private (chmod 600)", path))
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxConfigFile+1))
	if err != nil {
		return fc, warnings, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxConfigFile {
		return fileConfig{}, append(warnings, fmt.Sprintf("%s is too large; ignoring it", path)), nil
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		return fileConfig{}, append(warnings, fmt.Sprintf("%s is not valid JSON (%v); ignoring it", path, err)), nil
	}
	if fc.Hive != "" {
		if u, err := config.HiveURL(fc.Hive); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: ignoring invalid hive %q", path, sanitizeCell(fc.Hive)))
			fc = fileConfig{}
		} else {
			fc.Hive = u
		}
	}
	if fc.Fingerprint != "" {
		if fp, err := NormalizeFingerprint(fc.Fingerprint); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: ignoring invalid fingerprint", path))
			fc.Fingerprint, fc.Session, fc.SessionExpires = "", "", time.Time{}
		} else {
			fc.Fingerprint = fp
		}
	}
	if fc.Session != "" && (!validSession(fc.Session) || fc.Fingerprint == "") {
		fc.Session, fc.SessionExpires = "", time.Time{}
	}
	return fc, warnings, nil
}

// saveFileConfig writes ctl.json atomically with mode 0600 in a 0700
// directory.
func saveFileConfig(path string, fc fileConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if filepath.Base(dir) == configDirName {
		// Our own directory: keep it private even if it existed before.
		_ = os.Chmod(dir, 0o700)
	}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, "."+configFileName+".*")
	if err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("save %s: %w", path, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("save %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}

// settings are the effective connection parameters of one invocation.
type settings struct {
	hive        string // normalized base URL, "" = none
	hiveSource  string // flag, env or file
	fingerprint string // normalized, "" = none
	fpSource    string
	token       string // admin token from flag or env
	tokenSource string
	session     string // stored session usable for this hive and fingerprint
	expires     time.Time
	sameAsFile  bool // the effective hive is the one in ctl.json
}

// resolveSettings applies the precedence flags > environment > ctl.json.
// The stored fingerprint and session only apply to the stored hive, and
// the session only while the effective fingerprint is the one it was
// issued under, so a credential never goes to a different hive identity.
func resolveSettings(flagHive, flagFP, flagToken string, getenv func(string) string, fc fileConfig) (settings, error) {
	var s settings
	pick := func(flag, env, file string) (string, string) {
		switch {
		case flag != "":
			return flag, "flag"
		case env != "":
			return env, "env"
		case file != "":
			return file, "file"
		}
		return "", ""
	}
	raw, src := pick(flagHive, getenv(EnvHive), fc.Hive)
	if raw != "" {
		u, err := config.HiveURL(raw)
		if err != nil {
			return s, fmt.Errorf("invalid hive address %q (from %s): %w", truncate(sanitizeCell(raw), 80), src, err)
		}
		s.hive, s.hiveSource = u, src
	}
	s.sameAsFile = s.hive != "" && s.hive == fc.Hive
	fileFP := ""
	if s.sameAsFile {
		fileFP = fc.Fingerprint
	}
	raw, src = pick(flagFP, getenv(EnvFingerprint), fileFP)
	if raw != "" {
		fp, err := NormalizeFingerprint(raw)
		if err != nil {
			return s, fmt.Errorf("%w (from %s)", err, src)
		}
		s.fingerprint, s.fpSource = fp, src
	}
	s.token, s.tokenSource = pick(flagToken, getenv(EnvAdminToken), "")
	if s.sameAsFile && fc.Session != "" && fc.Fingerprint != "" && fc.Fingerprint == s.fingerprint {
		s.session, s.expires = fc.Session, fc.SessionExpires
	}
	return s, nil
}
