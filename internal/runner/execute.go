package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

type stopReason int

const (
	stopNone stopReason = iota
	stopTimeout
	stopCanceled
)

type outcome struct {
	err       error
	outputErr error
	status    string
	exitCode  int
	duration  time.Duration
}

type executor struct {
	plan     *Plan
	stdout   io.Writer
	stderr   io.Writer
	spoolErr error
	spoolDir string
	outcomes []outcome
	timeout  time.Duration
	grace    time.Duration
	jobs     int
	failFast bool
}

// spool holds one consumer's output until it is replayed. The files are named
// in a private directory rather than unlinked while open, so they can be
// closed as soon as the consumer exits and reopened for replay: every task
// that finishes behind a slow one would otherwise hold its descriptors until
// that one ends. The cost is that a killed Schepherd leaves the directory
// behind.
type spool struct {
	stdout     *os.File
	stderr     *os.File
	stdoutPath string
	stderrPath string
}

type watcher struct {
	quit   chan struct{}
	done   chan struct{}
	reason stopReason
}

// Execute runs the plan and returns the report together with the error that
// decides the exit status: the first task that did not succeed, in task
// order rather than completion order. With Spec.Jobs = 1 (or a single task)
// consumers write straight to opts.Stdout and opts.Stderr; with more jobs
// each consumer's output is spooled to temporary files and replayed in task
// order once it finishes. Every consumer runs in its own process group (Unix) or job object
// (Windows) that is killed on timeout, on cancellation of ctx and after the
// consumer exits.
func Execute(ctx context.Context, plan *Plan, opts Options) (*Report, error) {
	if plan == nil {
		return nil, fault.New(fault.Internal, "runner: no plan to execute")
	}

	x := newExecutor(plan, opts)

	var replayErr error
	if x.jobs == 1 {
		x.runSequential(ctx)
	} else {
		replayErr = x.runParallel(ctx)
	}

	return x.report(ctx, replayErr)
}

func newExecutor(plan *Plan, opts Options) *executor {
	x := &executor{
		plan:     plan,
		stdout:   opts.Stdout,
		stderr:   opts.Stderr,
		outcomes: make([]outcome, len(plan.Tasks)),
		timeout:  opts.Spec.Timeout,
		grace:    opts.KillGrace,
		jobs:     min(max(opts.Spec.Jobs, 1), MaxJobs, max(len(plan.Tasks), 1)),
		failFast: opts.Spec.FailFast,
	}

	if x.timeout <= 0 {
		x.timeout = DefaultTimeout
	}

	if x.grace <= 0 {
		x.grace = DefaultKillGrace
	}

	for i := range x.outcomes {
		x.outcomes[i] = outcome{status: StatusNotStarted, exitCode: -1}
	}

	return x
}

func (x *executor) runSequential(ctx context.Context) {
	failed := false

	for i := range x.plan.Tasks {
		if ctx.Err() != nil || (x.failFast && failed) {
			return
		}

		x.outcomes[i] = x.run(ctx, &x.plan.Tasks[i], x.stdout, x.stderr)
		failed = failed || x.outcomes[i].status != StatusOK
	}
}

// runParallel starts tasks strictly in task order, so a task that was never
// started always comes after every task that was.
func (x *executor) runParallel(ctx context.Context) error {
	if x.stdout != nil || x.stderr != nil {
		x.spoolDir, x.spoolErr = os.MkdirTemp("", "schepherd-output-*")
		if x.spoolErr == nil {
			defer func() { _ = os.RemoveAll(x.spoolDir) }()
		}
	}

	n := len(x.plan.Tasks)
	spools := make([]*spool, n)
	finished := make(chan int, n)

	var (
		mu     sync.Mutex
		next   int
		failed bool
		wg     sync.WaitGroup
	)

	take := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()

		if next >= n || ctx.Err() != nil || (x.failFast && failed) {
			return 0, false
		}

		next++

		return next - 1, true
	}

	for range x.jobs {
		wg.Go(func() {
			for i, ok := take(); ok; i, ok = take() {
				o, sp := x.runSpooled(ctx, &x.plan.Tasks[i])

				mu.Lock()
				x.outcomes[i], spools[i] = o, sp
				failed = failed || o.status != StatusOK
				mu.Unlock()

				finished <- i
			}
		})
	}

	go func() {
		wg.Wait()
		close(finished)
	}()

	return x.replayInOrder(finished, spools)
}

func (x *executor) replayInOrder(finished <-chan int, spools []*spool) error {
	done := make([]bool, len(spools))
	next := 0

	var firstErr error

	for i := range finished {
		done[i] = true

		for ; next < len(spools) && done[next]; next++ {
			if err := x.replay(spools[next]); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	for _, sp := range spools[next:] {
		sp.discard()
	}

	return firstErr
}

func (x *executor) runSpooled(ctx context.Context, task *Task) (outcome, *spool) {
	if x.spoolErr != nil {
		return startFailure(fault.Wrap(fault.Internal, x.spoolErr, "spool consumer output")), nil
	}

	sp, err := newSpool(x.spoolDir, x.stdout != nil, x.stderr != nil)
	if err != nil {
		return startFailure(fault.Wrap(fault.Internal, err, "spool consumer output")), nil
	}

	var stdout, stderr io.Writer
	if sp.stdout != nil {
		stdout = sp.stdout
	}

	if sp.stderr != nil {
		stderr = sp.stderr
	}

	o := x.run(ctx, task, stdout, stderr)
	sp.park()

	return o, sp
}

func (x *executor) replay(sp *spool) error {
	if sp == nil {
		return nil
	}

	defer sp.discard()

	return errors.Join(copyBack(x.stdout, sp.stdoutPath), copyBack(x.stderr, sp.stderrPath))
}

func copyBack(w io.Writer, path string) error {
	if path == "" {
		return nil
	}

	// bearer:disable go_gosec_filesystem_filereadtaint
	// The path is a spool file this run created in its private directory.
	f, err := os.Open(path) //nolint:gosec // G304: a spool file this run created in its private directory
	if err != nil {
		return fmt.Errorf("reopen consumer output: %w", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("replay consumer output: %w", err)
	}

	return nil
}

func newSpool(dir string, stdout, stderr bool) (*spool, error) {
	sp := &spool{}

	for _, target := range []struct {
		file   **os.File
		path   *string
		wanted bool
	}{{&sp.stdout, &sp.stdoutPath, stdout}, {&sp.stderr, &sp.stderrPath, stderr}} {
		if !target.wanted {
			continue
		}

		f, err := os.CreateTemp(dir, "spool-*")
		if err != nil {
			sp.discard()

			return nil, fmt.Errorf("create spool file: %w", err)
		}

		*target.file, *target.path = f, f.Name()
	}

	return sp, nil
}

func (sp *spool) park() {
	for _, f := range []*os.File{sp.stdout, sp.stderr} {
		if f != nil {
			_ = f.Close()
		}
	}

	sp.stdout, sp.stderr = nil, nil
}

func (sp *spool) discard() {
	if sp == nil {
		return
	}

	sp.park()

	for _, path := range []string{sp.stdoutPath, sp.stderrPath} {
		if path != "" {
			_ = os.Remove(path)
		}
	}

	sp.stdoutPath, sp.stderrPath = "", ""
}

// run executes one task. Output goes to stdout and stderr unchanged; exec
// copies through a pipe only when they are not *os.File.
func (x *executor) run(ctx context.Context, task *Task, stdout, stderr io.Writer) outcome {
	if ctx.Err() != nil {
		return interrupted(task, 0)
	}

	cmd := &exec.Cmd{
		Path:      task.Path,
		Args:      task.Args,
		Dir:       task.Dir,
		Env:       task.Env,
		Stdout:    stdout,
		Stderr:    stderr,
		WaitDelay: x.grace,
	}

	if cmd.Env == nil {
		cmd.Env = []string{}
	}

	if task.StdinFile != "" {
		f, err := os.Open(task.StdinFile)
		if err != nil {
			return startFailure(fault.Reclassify(fault.ConsumerStart, err, "open %s as consumer input", task.StdinFile))
		}

		defer func() { _ = f.Close() }()

		cmd.Stdin = f
	}

	group := newProcessGroup()
	defer group.release()

	group.configure(cmd)

	began := time.Now()

	if err := cmd.Start(); err != nil {
		return startFailure(fault.Reclassify(fault.ConsumerStart, err, "start consumer %s", task.Path))
	}

	if err := group.attach(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return startFailure(fault.Reclassify(fault.ConsumerStart, err, "contain consumer %s", task.Path))
	}

	w := watch(ctx, group, x.timeout, x.grace)

	var (
		reason  stopReason
		waitErr error
	)

	if awaitExit(cmd.Process) {
		reason = w.stop()
		group.kill()
		waitErr = cmd.Wait()
	} else {
		waitErr = cmd.Wait()
		reason = w.stop()
		group.kill()
	}

	return x.classify(task, cmd.ProcessState, waitErr, reason, time.Since(began))
}

func (x *executor) classify(task *Task, state *os.ProcessState, waitErr error, reason stopReason, elapsed time.Duration) outcome {
	switch reason {
	case stopTimeout:
		err := fault.New(fault.ConsumerTimeout, "consumer %s exceeded runner.timeout of %s", describe(task), x.timeout)

		return outcome{status: StatusTimeout, err: err, exitCode: fault.ExitCodeOf(err), duration: elapsed}
	case stopCanceled:
		return interrupted(task, elapsed)
	case stopNone:
	}

	if state == nil {
		err := fault.Wrap(fault.Internal, waitErr, "wait for consumer %s", describe(task))

		return outcome{status: StatusFailed, err: err, exitCode: fault.ExitCodeOf(err), duration: elapsed}
	}

	o := outcome{status: StatusOK, duration: elapsed}

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		o.outputErr = waitErr
	}

	code := state.ExitCode()
	if signalCode, ok := signalExitCode(state); ok {
		code = signalCode
	}

	if code != 0 {
		o.status = StatusFailed
		o.exitCode = code
		o.err = fmt.Errorf("consumer %s failed: %w", describe(task), &fault.ConsumerExitError{Code: code})
	}

	return o
}

func (x *executor) report(ctx context.Context, replayErr error) (*Report, error) {
	r := &Report{
		ReportVersion: ReportVersion,
		Mode:          x.plan.Mode,
		Tasks:         make([]TaskReport, 0, len(x.plan.Tasks)),
		Skipped:       []Skipped{},
	}

	var (
		result    error
		outputErr = replayErr
	)

	for i, task := range x.plan.Tasks {
		o := x.outcomes[i]
		r.Tasks = append(r.Tasks, TaskReport{
			SchemaID:   task.SchemaID,
			SchemaRef:  task.SchemaRef,
			Origin:     task.SchemaOrigin,
			Files:      append([]string{}, task.Files...),
			Status:     o.status,
			ExitCode:   o.exitCode,
			DurationMs: o.duration.Milliseconds(),
		})

		if outputErr == nil {
			outputErr = o.outputErr
		}

		if result == nil && o.status != StatusOK {
			result = x.failure(ctx, &x.plan.Tasks[i], o)
		}
	}

	if result == nil && outputErr != nil {
		result = fault.Wrap(fault.Internal, outputErr, "forward consumer output")
	}

	r.ExitCode = fault.ExitCodeOf(result)

	return r, result
}

func (x *executor) failure(ctx context.Context, task *Task, o outcome) error {
	if o.status != StatusNotStarted {
		return o.err
	}

	if ctx.Err() != nil {
		return fault.Reclassify(fault.Canceled, ctx.Err(), "interrupted before consumer %s started", describe(task))
	}

	return fault.New(fault.Internal, "runner: consumer %s was never started", describe(task))
}

func startFailure(err error) outcome {
	return outcome{status: StatusStartError, err: err, exitCode: fault.ExitCodeOf(err)}
}

func interrupted(task *Task, elapsed time.Duration) outcome {
	err := fault.New(fault.Canceled, "consumer %s was interrupted", describe(task))

	return outcome{status: StatusCanceled, err: err, exitCode: fault.ExitCodeOf(err), duration: elapsed}
}

func describe(task *Task) string {
	if len(task.Files) == 1 {
		return fmt.Sprintf("for schema %q on %s", task.SchemaID, task.Files[0])
	}

	return fmt.Sprintf("for schema %q on %d files", task.SchemaID, len(task.Files))
}

// watch stops the consumer group when the task times out or ctx is
// canceled: SIGTERM (or job termination) first, SIGKILL after the grace
// period if the consumer is still running.
func watch(ctx context.Context, group *processGroup, timeout, grace time.Duration) *watcher {
	w := &watcher{quit: make(chan struct{}), done: make(chan struct{})}

	go func() {
		defer close(w.done)

		deadline := time.NewTimer(timeout)
		defer deadline.Stop()

		select {
		case <-w.quit:
			return
		case <-deadline.C:
			w.reason = stopTimeout
		case <-ctx.Done():
			w.reason = stopCanceled
		}

		group.terminate()

		escalate := time.NewTimer(grace)
		defer escalate.Stop()

		select {
		case <-w.quit:
		case <-escalate.C:
			group.kill()
		}
	}()

	return w
}

func (w *watcher) stop() stopReason {
	close(w.quit)
	<-w.done

	return w.reason
}
