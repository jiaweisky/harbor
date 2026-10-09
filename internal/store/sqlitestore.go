package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/jiaweisky/harbor/internal/sqlstore"
)

// SQLiteStore is a taskstore.Store backed directly by a SQLite database: the
// database is the source of truth, so task history is not held in memory and the
// store's ownership, versioning, and list semantics are enforced in SQL.
type SQLiteStore struct {
	db   *sql.DB
	auth taskstore.Authenticator
	now  func() time.Time
}

// NewSQLiteStore opens (or creates) a SQLite-backed task store at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sqlstore.Open(path)
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{
		db:   db,
		auth: func(context.Context) (string, error) { return "", nil },
		now:  time.Now,
	}, nil
}

// Close closes the database.
func (s *SQLiteStore) Close() error { return s.db.Close() }

func initSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id         TEXT PRIMARY KEY,
			context_id TEXT NOT NULL,
			user       TEXT NOT NULL,
			status     TEXT NOT NULL,
			status_ts  INTEGER,
			updated_at INTEGER NOT NULL,
			version    INTEGER NOT NULL,
			data       BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS tasks_user_updated ON tasks(user, updated_at DESC, id DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// Create implements taskstore.Store.
func (s *SQLiteStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if err := validateTask(task); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	user, err := s.auth(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	data, err := json.Marshal(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	// ON CONFLICT keeps the check in one statement, so an existing id maps to
	// ErrTaskAlreadyExists without driver-specific error inspection.
	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id, context_id, user, status, status_ts, updated_at, version, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		string(task.ID), task.ContextID, user, string(task.Status.State),
		statusNanos(task), s.now().UnixNano(), int64(1), data)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	return taskstore.TaskVersion(1), nil
}

// Update implements taskstore.Store.
func (s *SQLiteStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if err := validateTask(req.Task); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	user, err := s.auth(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	data, err := json.Marshal(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	defer func() { _ = tx.Rollback() }()

	var storedUser string
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT user, version FROM tasks WHERE id = ?`, string(req.Task.ID)).
		Scan(&storedUser, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if storedUser != user {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && taskstore.TaskVersion(version) != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}

	next := version + 1
	if _, err := tx.ExecContext(ctx, `UPDATE tasks
		SET context_id = ?, status = ?, status_ts = ?, updated_at = ?, version = ?, data = ?
		WHERE id = ?`,
		req.Task.ContextID, string(req.Task.Status.State), statusNanos(req.Task),
		s.now().UnixNano(), next, data, string(req.Task.ID)); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if err := tx.Commit(); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(next), nil
}

// Get implements taskstore.Store.
func (s *SQLiteStore) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	user, err := s.auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskstore auth failed: %w", err)
	}
	var storedUser string
	var version int64
	var data []byte
	err = s.db.QueryRowContext(ctx, `SELECT user, version, data FROM tasks WHERE id = ?`, string(id)).
		Scan(&storedUser, &version, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if storedUser != user {
		// Mask an ownership mismatch as not-found, per the store contract.
		return nil, a2a.ErrTaskNotFound
	}
	task, err := decodeTask(data)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: storedUser}, nil
}

// List implements taskstore.Store.
func (s *SQLiteStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	user, err := s.auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskstore auth failed: %w", err)
	}
	if user == "" {
		return nil, a2a.ErrUnauthenticated
	}
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 1 || pageSize > maxPageSize:
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2a.ErrInvalidRequest)
	}

	where, args := listFilter(req, user)

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE "+strings.Join(where, " AND "), args...).
		Scan(&total); err != nil {
		return nil, err
	}

	pageWhere := append([]string{}, where...)
	pageArgs := append([]any{}, args...)
	if req.PageToken != "" {
		cursorTime, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		pageWhere = append(pageWhere, "(updated_at < ? OR (updated_at = ? AND id < ?))")
		pageArgs = append(pageArgs, cursorTime.UnixNano(), cursorTime.UnixNano(), string(cursorID))
	}
	// Fetch one extra row to know whether a further page exists.
	pageArgs = append(pageArgs, pageSize+1)

	query := "SELECT id, updated_at, data FROM tasks WHERE " + strings.Join(pageWhere, " AND ") +
		" ORDER BY updated_at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var got []taskRow
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.id, &r.updatedAt, &r.data); err != nil {
			return nil, err
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var nextToken string
	if len(got) > pageSize {
		got = got[:pageSize]
		last := got[pageSize-1]
		nextToken = encodePageToken(time.Unix(0, last.updatedAt), a2a.TaskID(last.id))
	}

	tasks, err := pageTasks(got, req)
	if err != nil {
		return nil, err
	}
	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     total,
		PageSize:      pageSize,
		NextPageToken: nextToken,
	}, nil
}

// taskRow is one listed task's raw row.
type taskRow struct {
	id        string
	updatedAt int64
	data      []byte
}

// listFilter returns the WHERE clauses (and their args) shared by the count and
// page queries. The caller is always scoped to its own user.
func listFilter(req *a2a.ListTasksRequest, user string) ([]string, []any) {
	where := []string{"user = ?"}
	args := []any{user}
	if req.ContextID != "" {
		where = append(where, "context_id = ?")
		args = append(args, req.ContextID)
	}
	if req.Status != a2a.TaskStateUnspecified {
		where = append(where, "status = ?")
		args = append(args, string(req.Status))
	}
	if req.StatusTimestampAfter != nil {
		// A task without a status timestamp is not filtered out.
		where = append(where, "(status_ts IS NULL OR status_ts >= ?)")
		args = append(args, req.StatusTimestampAfter.UnixNano())
	}
	return where, args
}

// pageTasks decodes the page rows and applies the HistoryLength and
// IncludeArtifacts options.
func pageTasks(rows []taskRow, req *a2a.ListTasksRequest) ([]*a2a.Task, error) {
	historyLength := defaultMaxHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	out := make([]*a2a.Task, 0, len(rows))
	for _, r := range rows {
		task, err := decodeTask(r.data)
		if err != nil {
			return nil, err
		}
		if historyLength <= 0 {
			task.History = []*a2a.Message{}
		} else if len(task.History) > historyLength {
			task.History = task.History[len(task.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		out = append(out, task)
	}
	return out, nil
}

// Defaults and limits from the A2A task-list contract.
const (
	defaultPageSize         = 50
	maxPageSize             = 100
	defaultMaxHistoryLength = 100
)

func decodeTask(data []byte) (*a2a.Task, error) {
	var task a2a.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("task store: %w", err)
	}
	return &task, nil
}

// statusNanos returns the task status timestamp in Unix nanoseconds, or nil.
func statusNanos(task *a2a.Task) any {
	if task.Status.Timestamp == nil {
		return nil
	}
	return task.Status.Timestamp.UnixNano()
}

func encodePageToken(updated time.Time, id a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString([]byte(updated.Format(time.RFC3339Nano) + "_" + string(id)))
}

func decodePageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	ts, id, ok := strings.Cut(string(decoded), "_")
	if !ok {
		return time.Time{}, "", a2a.ErrParseError
	}
	updated, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return updated, a2a.TaskID(id), nil
}
