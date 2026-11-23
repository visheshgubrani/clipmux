// Package backup keeps encrypted copies of the database on a backup target
// and reads them back (D143, D146). A backup is a pg_dump in custom format,
// encrypted with age to an identity derived from the secrets key, written to
// an S3-compatible bucket or a directory beside a manifest that says what it
// is. The settings come from the environment, never the database, because a
// restore needs the target before any database exists.
package backup

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
)

// Defaults when a variable is unset.
const (
	// DefaultInterval is the time between backups to a target the owner chose.
	DefaultInterval = time.Hour
	// DefaultLocalInterval is the time between backups to the built-in local
	// directory, which isn't off-site: one a day.
	DefaultLocalInterval = 24 * time.Hour
	// MinInterval keeps a mistyped value from dumping the database in a loop.
	MinInterval = time.Minute
)

// Config is the backup configuration.
type Config struct {
	// URL is the target: s3://bucket/prefix?endpoint=…&region=…, or
	// file:///path (CLIPMUX_BACKUP_URL; default file://{data}/backups).
	URL string
	// Local reports that URL is the built-in default, which sits on the same
	// disk as the database.
	Local bool
	// AccessKeyID and SecretAccessKey are the target's credentials
	// (CLIPMUX_BACKUP_ACCESS_KEY_ID, CLIPMUX_BACKUP_SECRET_ACCESS_KEY). Never
	// log them.
	AccessKeyID     string
	SecretAccessKey string
	// Interval is the time between scheduled backups (CLIPMUX_BACKUP_INTERVAL);
	// 0 turns them off. The commands still work.
	Interval time.Duration
	// Recipients are age recipients added to every backup
	// (CLIPMUX_BACKUP_RECIPIENTS), such as the owner's offline key.
	Recipients []string
	// PingURL is fetched after each successful scheduled backup
	// (CLIPMUX_BACKUP_PING_URL): a dead man's switch, so a missing backup
	// raises an alert elsewhere. Never log it.
	PingURL string
	// BeforeMigrate backs the database up before pending migrations are applied
	// (CLIPMUX_BACKUP_BEFORE_MIGRATE; default true), and refuses to migrate if
	// that fails.
	BeforeMigrate bool
	// PGDump is the pg_dump binary (CLIPMUX_PG_DUMP; default found on PATH).
	PGDump string
}

// FromEnv reads the configuration; dataDir holds the default target.
func FromEnv(getenv func(string) string, dataDir string) (Config, error) {
	cfg := Config{
		URL:             getenv("CLIPMUX_BACKUP_URL"),
		AccessKeyID:     getenv("CLIPMUX_BACKUP_ACCESS_KEY_ID"),
		SecretAccessKey: getenv("CLIPMUX_BACKUP_SECRET_ACCESS_KEY"),
		PingURL:         getenv("CLIPMUX_BACKUP_PING_URL"),
		PGDump:          getenv("CLIPMUX_PG_DUMP"),
	}
	if cfg.PGDump == "" {
		cfg.PGDump = "pg_dump"
	}
	cfg.BeforeMigrate = true
	if v := getenv("CLIPMUX_BACKUP_BEFORE_MIGRATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("CLIPMUX_BACKUP_BEFORE_MIGRATE %q must be true or false", v)
		}
		cfg.BeforeMigrate = b
	}
	cfg.Interval = DefaultInterval
	if cfg.URL == "" {
		cfg.URL = "file://" + filepath.ToSlash(filepath.Join(absOr(dataDir), "backups"))
		cfg.Local = true
		cfg.Interval = DefaultLocalInterval
	}
	if v := getenv("CLIPMUX_BACKUP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			return Config{}, fmt.Errorf("CLIPMUX_BACKUP_INTERVAL %q must be a duration such as 1h or 15m, or 0 to turn backups off", v)
		case d != 0 && d < MinInterval:
			return Config{}, fmt.Errorf("CLIPMUX_BACKUP_INTERVAL %q is under %s", v, MinInterval)
		}
		cfg.Interval = d
	}
	for r := range strings.FieldsFuncSeq(getenv("CLIPMUX_BACKUP_RECIPIENTS"), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if _, err := age.ParseX25519Recipient(r); err != nil {
			return Config{}, fmt.Errorf("CLIPMUX_BACKUP_RECIPIENTS: %q is not an age recipient (age1…)", r)
		}
		cfg.Recipients = append(cfg.Recipients, r)
	}
	if cfg.PingURL != "" {
		if u, err := url.Parse(cfg.PingURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("CLIPMUX_BACKUP_PING_URL must be an http or https URL")
		}
	}
	if _, err := parseTarget(cfg.URL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func absOr(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// Policy is how long each tier of backups is kept, by age. A tier with a zero
// age isn't kept, and nothing is promoted to it. The newest backup is never
// pruned, whatever its age, so a target that stopped receiving backups isn't
// emptied.
type Policy struct {
	Base, Daily, Weekly, Monthly time.Duration
}

// Policy is the retention that applies to the target: the standard one, or
// seven days of the one backup a day for the built-in local directory, which
// shares the database's disk.
func (c Config) Policy() Policy {
	if c.Local {
		return Policy{Base: 7 * 24 * time.Hour}
	}
	return Policy{Base: 3 * 24 * time.Hour, Daily: 35 * 24 * time.Hour, Weekly: 90 * 24 * time.Hour, Monthly: 400 * 24 * time.Hour}
}
