// Package store persists burn sessions in a SQLite database: which ISO files
// to burn, how many copies of each, which drives burn them and how far every
// disc has got, so an interrupted session can be resumed.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/sirily11/iso-burner/internal/drive"
)

// DiscStatus is where one disc is in its burn.
type DiscStatus string

const (
	// DiscPending discs have not been claimed by a drive yet.
	DiscPending DiscStatus = "pending"
	// DiscAssigned discs belong to a drive that waits for a blank disc.
	DiscAssigned  DiscStatus = "assigned"
	DiscBurning   DiscStatus = "burning"
	DiscVerifying DiscStatus = "verifying"
	// DiscDone discs are burned; Disc.VerifyNote says if they were not verified.
	DiscDone DiscStatus = "done"
)

// DriveState is what a drive is doing in a session.
type DriveState string

const (
	DriveIdle      DriveState = "idle"
	DriveWaiting   DriveState = "waiting" // for the user to insert a blank disc
	DriveBurning   DriveState = "burning"
	DriveVerifying DriveState = "verifying"
	DriveFinished  DriveState = "finished" // no discs left for this drive
)

// SessionStatus is the lifecycle state of a session.
type SessionStatus string

const (
	SessionActive    SessionStatus = "active"
	SessionDone      SessionStatus = "done"
	SessionDiscarded SessionStatus = "discarded"
)

// Job is one ISO file and how many discs to burn from it.
type Job struct {
	Path   string
	Size   int64
	Copies int
}

// Session is one run of burning a set of jobs.
type Session struct {
	ID        int64
	Status    SessionStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	// DriveIDs are the drives the session last burned with, in order.
	DriveIDs []string
	// Total and Done count the session's discs.
	Total, Done int
}

// Disc is one disc to burn: copy Copy of Copies from ISOPath.
type Disc struct {
	ID        int64
	SessionID int64
	ISOPath   string
	ISOSize   int64
	Copy      int
	Copies    int
	Status    DiscStatus
	DriveID   string
	// Progress and Total are the bytes handled by the current phase (burning
	// or verifying).
	Progress, Total int64
	Attempts        int
	// Error is why the last attempt failed.
	Error string
	// VerifyNote is set when a done disc could not be verified.
	VerifyNote string
	FinishedAt time.Time
}

// DriveRecord is a drive's persisted state in a session.
type DriveRecord struct {
	SessionID int64
	DriveID   string
	Name      string
	State     DriveState
	DiscID    int64 // the disc it is working on or waiting for, 0 if none
	Message   string
	Completed int // discs this drive finished in the session
}

// Store is a handle on the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS discs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	iso_path    TEXT NOT NULL,
	iso_size    INTEGER NOT NULL,
	copy        INTEGER NOT NULL,
	copies      INTEGER NOT NULL,
	status      TEXT NOT NULL,
	drive_id    TEXT NOT NULL DEFAULT '',
	progress    INTEGER NOT NULL DEFAULT 0,
	total       INTEGER NOT NULL DEFAULT 0,
	attempts    INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	verify_note TEXT NOT NULL DEFAULT '',
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS discs_session ON discs(session_id, status);
CREATE TABLE IF NOT EXISTS session_drives (
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	drive_id   TEXT NOT NULL,
	name       TEXT NOT NULL,
	position   INTEGER NOT NULL,
	state      TEXT NOT NULL,
	disc_id    INTEGER NOT NULL DEFAULT 0,
	message    TEXT NOT NULL DEFAULT '',
	completed  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (session_id, drive_id)
);
`

// DefaultPath is where the database lives unless told otherwise.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "iso-burner", "burns.db"), nil
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection serialises writes, so claiming a disc is race free.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func unix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// CreateSession records a new session with one pending disc per copy of each
// job, and the drives that will burn them.
func (s *Store) CreateSession(ctx context.Context, jobs []Job, drives []drive.Drive) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions(status, created_at, updated_at) VALUES (?, ?, ?)`, SessionActive, t, t)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		for c := 1; c <= j.Copies; c++ {
			if _, err := tx.ExecContext(ctx, `INSERT INTO discs(session_id, iso_path, iso_size, copy, copies, status) VALUES (?, ?, ?, ?, ?, ?)`,
				id, j.Path, j.Size, c, j.Copies, DiscPending); err != nil {
				return 0, err
			}
		}
	}
	if err := setDrives(ctx, tx, id, drives); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func setDrives(ctx context.Context, tx *sql.Tx, session int64, drives []drive.Drive) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_drives WHERE session_id = ?`, session); err != nil {
		return err
	}
	for i, d := range drives {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_drives(session_id, drive_id, name, position, state) VALUES (?, ?, ?, ?, ?)`,
			session, d.ID, d.Name(), i, DriveIdle); err != nil {
			return err
		}
	}
	return nil
}

// Resume prepares an interrupted session to continue with drives: discs that
// were being burned are put back in the queue, since a half-burned disc is
// unusable, and the drive list is replaced.
func (s *Store) Resume(ctx context.Context, session int64, drives []drive.Drive) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE discs SET status = ?, drive_id = '', progress = 0, total = 0,
		error = CASE WHEN status IN (?, ?) THEN 'interrupted' ELSE error END
		WHERE session_id = ? AND status IN (?, ?, ?)`,
		DiscPending, DiscBurning, DiscVerifying, session, DiscAssigned, DiscBurning, DiscVerifying); err != nil {
		return err
	}
	if err := setDrives(ctx, tx, session, drives); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, SessionActive, now(), session); err != nil {
		return err
	}
	return tx.Commit()
}

// Unfinished returns the most recent active session, or nil if there is none.
func (s *Store) Unfinished(ctx context.Context) (*Session, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE status = ? ORDER BY id DESC LIMIT 1`, SessionActive).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Session(ctx, id)
}

// Session loads one session with its disc counts and drives.
func (s *Store) Session(ctx context.Context, id int64) (*Session, error) {
	sess := Session{ID: id}
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT status, created_at, updated_at,
		(SELECT COUNT(*) FROM discs WHERE session_id = s.id),
		(SELECT COUNT(*) FROM discs WHERE session_id = s.id AND status = ?)
		FROM sessions s WHERE id = ?`, DiscDone, id).Scan(&sess.Status, &created, &updated, &sess.Total, &sess.Done)
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.UpdatedAt = unix(created), unix(updated)
	drives, err := s.Drives(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, d := range drives {
		sess.DriveIDs = append(sess.DriveIDs, d.DriveID)
	}
	return &sess, nil
}

// SetSessionStatus marks a session done or discarded.
func (s *Store) SetSessionStatus(ctx context.Context, id int64, status SessionStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

const discColumns = `id, session_id, iso_path, iso_size, copy, copies, status, drive_id, progress, total, attempts, error, verify_note, finished_at`

func scanDisc(row interface{ Scan(...any) error }) (Disc, error) {
	var d Disc
	var finished int64
	err := row.Scan(&d.ID, &d.SessionID, &d.ISOPath, &d.ISOSize, &d.Copy, &d.Copies, &d.Status, &d.DriveID,
		&d.Progress, &d.Total, &d.Attempts, &d.Error, &d.VerifyNote, &finished)
	d.FinishedAt = unix(finished)
	return d, err
}

// Discs lists a session's discs in burn order.
func (s *Store) Discs(ctx context.Context, session int64) ([]Disc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+discColumns+` FROM discs WHERE session_id = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Disc
	for rows.Next() {
		d, err := scanDisc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Disc loads one disc.
func (s *Store) Disc(ctx context.Context, id int64) (Disc, error) {
	return scanDisc(s.db.QueryRowContext(ctx, `SELECT `+discColumns+` FROM discs WHERE id = ?`, id))
}

// Claim returns the disc driveID should burn next: one already assigned to
// it, such as a disc whose burn failed, or else the next pending disc, which
// it assigns to the drive. It returns nil when no disc is left to burn.
func (s *Store) Claim(ctx context.Context, session int64, driveID string) (*Disc, error) {
	d, err := scanDisc(s.db.QueryRowContext(ctx, `UPDATE discs SET status = ?, drive_id = ?, progress = 0, total = 0
		WHERE id = (SELECT id FROM discs WHERE session_id = ?
			AND (status = ? OR (status = ? AND drive_id = ?))
			ORDER BY status = ? DESC, id LIMIT 1)
		RETURNING `+discColumns, DiscAssigned, driveID, session, DiscPending, DiscAssigned, driveID, DiscAssigned))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// StartPhase moves a disc into burning or verifying with total bytes to go.
// Starting to burn counts as a new attempt.
func (s *Store) StartPhase(ctx context.Context, disc int64, status DiscStatus, total int64) error {
	inc := 0
	if status == DiscBurning {
		inc = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE discs SET status = ?, progress = 0, total = ?, attempts = attempts + ? WHERE id = ?`,
		status, total, inc, disc)
	return err
}

// SetProgress records how many bytes of the current phase are done.
func (s *Store) SetProgress(ctx context.Context, disc, progress int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE discs SET progress = ? WHERE id = ?`, progress, disc)
	return err
}

// FinishDisc marks a disc burned. verifyNote explains why it was not
// verified, if it was not.
func (s *Store) FinishDisc(ctx context.Context, disc int64, verifyNote string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE discs SET status = ?, progress = total, error = '', verify_note = ?, finished_at = ? WHERE id = ?`,
		DiscDone, verifyNote, now(), disc)
	return err
}

// FailDisc records why a burn attempt failed and keeps the disc assigned to
// its drive so the same drive retries it with a new blank disc.
func (s *Store) FailDisc(ctx context.Context, disc int64, cause error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE discs SET status = ?, progress = 0, total = 0, error = ? WHERE id = ?`,
		DiscAssigned, cause.Error(), disc)
	return err
}

// ReleaseDisc puts a disc back in the queue, e.g. when burning is stopped.
func (s *Store) ReleaseDisc(ctx context.Context, disc int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE discs SET status = ?, drive_id = '', progress = 0, total = 0, error = ? WHERE id = ? AND status != ?`,
		DiscPending, reason, disc, DiscDone)
	return err
}

// SetDrive records a drive's state in a session.
func (s *Store) SetDrive(ctx context.Context, session int64, driveID string, state DriveState, disc int64, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_drives SET state = ?, disc_id = ?, message = ?,
		completed = (SELECT COUNT(*) FROM discs WHERE session_id = ? AND drive_id = ? AND status = ?)
		WHERE session_id = ? AND drive_id = ?`,
		state, disc, message, session, driveID, DiscDone, session, driveID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now(), session)
	return err
}

// Drives lists a session's drives in the order they were chosen.
func (s *Store) Drives(ctx context.Context, session int64) ([]DriveRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, drive_id, name, state, disc_id, message, completed
		FROM session_drives WHERE session_id = ? ORDER BY position`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriveRecord
	for rows.Next() {
		var d DriveRecord
		if err := rows.Scan(&d.SessionID, &d.DriveID, &d.Name, &d.State, &d.DiscID, &d.Message, &d.Completed); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
