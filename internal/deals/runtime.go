package deals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/core"
	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// InvocationInfo summarizes a real workflow instance persisted in workflows.sqlite (`instances` + `history`).
type InvocationInfo struct {
	ID             string `json:"id"`
	ExecutionID    string `json:"executionId,omitempty"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	RetryCount     int64  `json:"retryCount"`
	NextRetryAt    int64  `json:"nextRetryAt,omitempty"`
	ModifiedAt     int64  `json:"modifiedAt,omitempty"`
	LastFailureErr string `json:"lastFailureError,omitempty"`
}

// Runtime embeds pure-Go cschleiden/go-workflows (modernc.org/sqlite WAL) directly in-process,
// completely eliminating external Rust restate-server sidecars, WSL2 spawning, and h2c ports.
type Runtime struct {
	sqlDB   *sql.DB
	memConn *sql.Conn
	backend backend.Backend
	client  *client.Client
	worker  *worker.Worker

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
}

// NewRuntime opens <dataDir>/workflows.sqlite in WAL + immediate-txlock mode (or a named pure-Go
// in-memory SQLite database when dataDir == "") and runs embedded schema migrations quietly.
func NewRuntime(dataDir string) (*Runtime, error) {
	var dsn string
	inMem := strings.TrimSpace(dataDir) == "" || dataDir == ":memory:"
	if inMem {
		dsn = fmt.Sprintf("file:nektar_wf_%s?mode=memory&cache=shared&_txlock=immediate", strings.ReplaceAll(uuid.NewString(), "-", ""))
	} else {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("deals: mkdir %s: %w", dataDir, err)
		}
		dbPath := filepath.Join(dataDir, "workflows.sqlite")
		dsn = fmt.Sprintf("file:%s?_txlock=immediate", filepath.ToSlash(dbPath))
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("deals: open workflows.sqlite: %w", err)
	}
	var memConn *sql.Conn
	if inMem {
		sqlDB.SetConnMaxIdleTime(0)
		sqlDB.SetMaxIdleConns(2)
		sqlDB.SetMaxOpenConns(2)
		memConn, err = sqlDB.Conn(context.Background())
		if err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("deals: pin in-memory workflows conn: %w", err)
		}
	} else {
		if _, err := sqlDB.Exec(`
			PRAGMA journal_mode=WAL;
			PRAGMA synchronous=NORMAL;
			PRAGMA cache_size=-64000;
			PRAGMA temp_store=MEMORY;
			PRAGMA mmap_size=134217728;
			PRAGMA wal_autocheckpoint=4000;
			PRAGMA journal_size_limit=67108864;
		`); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("deals: configure WAL PRAGMAs on workflows.sqlite: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}
	if _, err := sqlDB.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		if memConn != nil {
			_ = memConn.Close()
		}
		_ = sqlDB.Close()
		return nil, fmt.Errorf("deals: set busy_timeout on workflows.sqlite: %w", err)
	}

	quietLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	sb := sqlite.NewSqliteBackendWithDB(
		sqlDB,
		sqlite.WithApplyMigrations(false),
		sqlite.WithBackendOptions(backend.WithLogger(quietLogger)),
	)
	if err := sb.Migrate(); err != nil {
		if memConn != nil {
			_ = memConn.Close()
		}
		_ = sqlDB.Close()
		return nil, fmt.Errorf("deals: migrate workflows.sqlite: %w", err)
	}

	opts := worker.DefaultOptions
	opts.WorkflowPollingInterval = 2 * time.Millisecond
	opts.ActivityPollingInterval = 2 * time.Millisecond
	opts.MaxParallelWorkflowTasks = 512
	opts.MaxParallelActivityTasks = 512

	w := worker.New(sb, &opts)
	c := client.New(sb)
	return &Runtime{
		sqlDB:   sqlDB,
		memConn: memConn,
		backend: sb,
		client:  c,
		worker:  w,
	}, nil
}

func (r *Runtime) Backend() backend.Backend { return r.backend }
func (r *Runtime) Client() *client.Client   { return r.client }

func (r *Runtime) RegisterWorkflow(wf workflow.Workflow, opts ...registry.RegisterOption) error {
	return r.worker.RegisterWorkflow(wf, opts...)
}

func (r *Runtime) RegisterActivity(act workflow.Activity, opts ...registry.RegisterOption) error {
	return r.worker.RegisterActivity(act, opts...)
}

// Start launches the embedded workflow & activity workers on a background context owned by Runtime.
func (r *Runtime) Start(parentCtx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("deals: runtime is closed")
	}
	if r.started {
		return nil
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	wCtx, cancel := context.WithCancel(parentCtx)
	if err := r.worker.Start(wCtx); err != nil {
		cancel()
		return fmt.Errorf("deals: start go-workflows worker: %w", err)
	}
	// Automatically prune completed workflow history older than 30 minutes so workflows.sqlite
	// stays bounded under 24/7 high-TPS deal execution.
	if r.client != nil {
		go func() {
			_ = r.client.StartAutoExpiration(wCtx, 30*time.Minute)
		}()
	}
	r.cancel = cancel
	r.started = true
	return nil
}

// IsRunning reports whether the embedded durable workflow worker is active.
func (r *Runtime) IsRunning() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && !r.closed
}

// Close stops the embedded worker, waits for in-flight tasks to drain cleanly (bounded to 10s), and closes workflows.sqlite.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	wasStarted := r.started
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if wasStarted && r.worker != nil {
		done := make(chan struct{})
		go func() {
			_ = r.worker.WaitForCompletion()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	if r.backend != nil {
		_ = r.backend.Close()
	}
	if r.memConn != nil {
		_ = r.memConn.Close()
	}
	if r.sqlDB != nil {
		return r.sqlDB.Close()
	}
	return nil
}

// CreateOrAttach starts a workflow keyed by instanceID, or returns the active running instance if already active.
func (r *Runtime) CreateOrAttach(ctx context.Context, instanceID string, wf workflow.Workflow, args ...any) (*workflow.Instance, error) {
	if r == nil || !r.IsRunning() {
		return nil, errors.New("deals: durable workflow runtime is not running")
	}
	inst, err := r.client.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
		InstanceID: instanceID,
	}, wf, args...)
	if err != nil {
		if existing, fErr := r.findLatestInstance(ctx, instanceID); fErr == nil && existing != nil {
			return existing, nil
		}
		return nil, err
	}
	return inst, nil
}

// Signal delivers a typed signal to instanceID's durable signal channel.
func (r *Runtime) Signal(ctx context.Context, instanceID, signalName string, arg any) error {
	if r == nil || !r.IsRunning() {
		return errors.New("deals: durable workflow runtime is not running")
	}
	return r.client.SignalWorkflow(ctx, instanceID, signalName, arg)
}

// Cancel cancels the latest execution of instanceID (or by executionID when instanceID matches).
func (r *Runtime) Cancel(ctx context.Context, idOrInstanceID string) error {
	if r == nil || !r.IsRunning() {
		return errors.New("deals: durable workflow runtime is not running")
	}
	inst, err := r.findInstanceByIDOrExec(ctx, idOrInstanceID)
	if err != nil || inst == nil {
		if err == nil {
			err = fmt.Errorf("workflow instance %q not found", idOrInstanceID)
		}
		return err
	}
	return r.client.CancelWorkflowInstance(ctx, inst)
}

func (r *Runtime) findLatestInstance(ctx context.Context, instanceID string) (*workflow.Instance, error) {
	var execID string
	err := r.sqlDB.QueryRowContext(ctx,
		`SELECT execution_id FROM instances WHERE id = ? ORDER BY created_at DESC LIMIT 1`,
		instanceID,
	).Scan(&execID)
	if err != nil {
		return nil, err
	}
	return core.NewWorkflowInstance(instanceID, execID), nil
}

func (r *Runtime) findInstanceByIDOrExec(ctx context.Context, id string) (*workflow.Instance, error) {
	var instID, execID string
	err := r.sqlDB.QueryRowContext(ctx,
		`SELECT id, execution_id FROM instances WHERE id = ? OR execution_id = ? ORDER BY created_at DESC LIMIT 1`,
		id, id,
	).Scan(&instID, &execID)
	if err != nil {
		return nil, err
	}
	return core.NewWorkflowInstance(instID, execID), nil
}

// QueryInvocations reads live workflow instances directly from workflows.sqlite (`instances` + `history`).
func (r *Runtime) QueryInvocations(ctx context.Context, limit int) ([]InvocationInfo, error) {
	if r == nil || r.sqlDB == nil {
		return nil, errors.New("deals: workflow database unavailable")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := r.sqlDB.QueryContext(ctx, `
		SELECT i.id, i.execution_id, i.state, i.created_at, i.completed_at,
		       COALESCE((SELECT a.data FROM history h JOIN attributes a ON a.id = h.id AND a.instance_id = h.instance_id AND a.execution_id = h.execution_id WHERE h.instance_id = i.id AND h.execution_id = i.execution_id AND h.sequence_id = 1 LIMIT 1), x''),
		       COALESCE((SELECT COUNT(*) FROM pending_events p WHERE p.instance_id = i.id AND p.execution_id = i.execution_id), 0)
		FROM instances i
		ORDER BY i.created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InvocationInfo
	for rows.Next() {
		var (
			id, execID  string
			stateInt    int
			createdAt   time.Time
			completedAt sql.NullTime
			startAttr   []byte
			pendingCnt  int64
		)
		if err := rows.Scan(&id, &execID, &stateInt, &createdAt, &completedAt, &startAttr, &pendingCnt); err != nil {
			continue
		}
		wfName := "Workflow"
		if len(startAttr) > 0 {
			var parsed struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(startAttr, &parsed) == nil && parsed.Name != "" {
				wfName = parsed.Name
			}
		}
		status := "running"
		switch core.WorkflowInstanceState(stateInt) {
		case core.WorkflowInstanceStateActive:
			if pendingCnt == 0 {
				status = "suspended"
			} else {
				status = "running"
			}
		case core.WorkflowInstanceStateContinuedAsNew:
			status = "continued"
		case core.WorkflowInstanceStateFinished:
			status = "completed"
		}
		modAt := createdAt.UnixMilli()
		if completedAt.Valid && !completedAt.Time.IsZero() {
			modAt = completedAt.Time.UnixMilli()
		}
		out = append(out, InvocationInfo{
			ID:          id,
			ExecutionID: execID,
			Target:      fmt.Sprintf("%s/%s", wfName, id),
			Status:      status,
			ModifiedAt:  modAt,
		})
	}
	return out, rows.Err()
}
