package manage

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// Backups (T29.8, D143): what the dashboard shows of them, "back up now", and
// the owner's word that the recovery kit is kept somewhere else. The target and
// its schedule are environment settings, so they are shown and not edited here.

// BackupsView is GET /v1/settings/backups.
type BackupsView struct {
	// Enabled is false when CLIPMUX_BACKUP_INTERVAL is 0.
	Enabled bool `json:"enabled"`
	// Target names the target without its credentials.
	Target          string `json:"target,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	// OffSite is false for a directory on this server's own disk.
	OffSite bool `json:"off_site"`
	// Immutable is true when the target's key can't delete (the setup
	// ransomware can't empty), false when it can, and null until a backup has
	// found out since serve started.
	Immutable *bool `json:"immutable"`
	// Last is the newest backup, Verified the newest check of one (a failed
	// check included), Error why the newest attempt failed.
	Last     *BackupView       `json:"last"`
	Verified *VerificationView `json:"verified"`
	Error    *BackupErrorView  `json:"error"`
	// Requested is true while a "back up now" waits for serve's next pass.
	Requested      bool       `json:"requested"`
	SourcesPending int64      `json:"sources_pending"`
	Fence          *FenceView `json:"fence"`
	// RecoveryKitConfirmedAt is when the owner said the secrets key and
	// DATABASE_URL are stored off this server.
	RecoveryKitConfirmedAt *time.Time `json:"recovery_kit_confirmed_at"`
}

// BackupView is one backup.
type BackupView struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
}

// VerificationView is one check of a backup.
type VerificationView struct {
	BackupID string    `json:"backup_id"`
	At       time.Time `json:"at"`
	// Mode is "restore" (loaded into a scratch database) or "integrity" (the
	// file only: the role can't create databases).
	Mode  string `json:"mode"`
	Error string `json:"error,omitempty"`
}

// BackupErrorView is a failed backup attempt.
type BackupErrorView struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// FenceView is the restore fence while it is up (D144).
type FenceView struct {
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail"`
	DetectedAt time.Time `json:"detected_at"`
}

func (m *API) getBackups(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	ctx := r.Context()
	q := db.New(m.pool)
	v := BackupsView{Enabled: m.Backups != nil, IntervalSeconds: int64(m.BackupConfig.Interval / time.Second), OffSite: !m.BackupConfig.Local}
	if m.Backups != nil {
		v.Target = m.Backups.Target.Desc
		if immutable, known := m.Backups.Immutable(); known {
			v.Immutable = &immutable
		}
	}
	last, err := q.LastBackup(ctx, o.ProjectID)
	switch {
	case err == nil:
		v.Last = &BackupView{ID: last.ID, Kind: last.Kind, CreatedAt: last.CreatedAt.Time.UTC(), SizeBytes: last.SizeBytes}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	check, err := q.LastBackupVerification(ctx, o.ProjectID)
	switch {
	case err == nil:
		v.Verified = &VerificationView{BackupID: check.ID, At: check.VerifiedAt.Time.UTC(), Mode: check.VerifyMode.String, Error: check.VerifyError.String}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	state, err := q.GetBackupState(ctx, o.ProjectID)
	if err != nil {
		return err
	}
	if state.BackupError.Valid {
		v.Error = &BackupErrorView{At: state.BackupErrorAt.Time.UTC(), Message: state.BackupError.String}
	}
	v.Requested = state.BackupRequestedAt.Valid && (last.CreatedAt.Time.IsZero() || state.BackupRequestedAt.Time.After(last.CreatedAt.Time))
	v.RecoveryKitConfirmedAt = optTime(state.RecoveryKitConfirmedAt)
	if v.SourcesPending, err = q.CountSourcesToMirror(ctx); err != nil {
		return err
	}
	switch f, err := q.GetActiveFence(ctx, o.ProjectID); {
	case err == nil:
		v.Fence = &FenceView{Reason: f.Reason, Detail: f.Detail, DetectedAt: f.DetectedAt.Time.UTC()}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	httpjson.Write(w, http.StatusOK, v)
	return nil
}

// runBackup asks for a backup now. serve's leader takes it on its next pass,
// within a minute: only it runs pg_dump, and a dump can outlast a request.
func (m *API) runBackup(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Backups == nil {
		return httpjson.Fail(http.StatusConflict, "backups_off", "backups are off: CLIPMUX_BACKUP_INTERVAL is 0")
	}
	err := db.New(m.pool).RequestBackup(r.Context(), db.RequestBackupParams{ID: o.ProjectID, RequestedAt: pgtype.Timestamptz{Time: m.Now(), Valid: true}})
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"requested": true})
	return nil
}

func (m *API) setRecoveryKit(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	var body struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	q := db.New(m.pool)
	if err := q.SetRecoveryKitConfirmed(r.Context(), db.SetRecoveryKitConfirmedParams{ID: o.ProjectID, Confirmed: body.Confirmed}); err != nil {
		return err
	}
	state, err := q.GetBackupState(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"recovery_kit_confirmed_at": optTime(state.RecoveryKitConfirmedAt)})
	return nil
}
