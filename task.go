package task

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-task/task/v3/internal/compiler"
	"github.com/go-task/task/v3/internal/execext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/output"
	"github.com/go-task/task/v3/internal/summary"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/taskfile"

	"github.com/sajari/fuzzy"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

const (
	// MaximumTaskCall is the max number of times a task can be called.
	// This exists to prevent infinite loops on cyclic dependencies
	MaximumTaskCall = 100
)

// Executor executes a Taskfile
type Executor struct {
	Taskfile *taskfile.Taskfile

	Dir         string
	TempDir     string
	Entrypoint  string
	Force       bool
	Watch       bool
	Verbose     bool
	Silent      bool
	Dry         bool
	Summary     bool
	Parallel    bool
	Color       bool
	Concurrency int
	Interval    string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Logger      *logger.Logger
	Compiler    compiler.Compiler
	Output      output.Output
	OutputStyle taskfile.Output

	taskvars   *taskfile.Vars
	fuzzyModel *fuzzy.Model

	concurrencySemaphore chan struct{}
	taskCallCount        map[string]*int32
	mkdirMutexMap        map[string]*sync.Mutex
	executionHashes      map[string]context.Context
	executionHashesMutex sync.Mutex
}

// Run runs Task
func (e *Executor) Run(ctx context.Context, calls ...taskfile.Call) error {
	// check if given tasks exist
	for _, call := range calls {
		task, err := e.GetTask(call)
		if err != nil {
			e.ListTasks(FilterOutInternal(), FilterOutNoDesc())
			return err
		}

		if task.Internal {
			e.ListTasks(FilterOutInternal(), FilterOutNoDesc())
			return &taskInternalError{taskName: call.Task}
		}
	}

	if e.Summary {
		for i, c := range calls {
			compiledTask, err := e.FastCompiledTask(c)
			if err != nil {
				return nil
			}
			summary.PrintSpaceBetweenSummaries(e.Logger, i)
			summary.PrintTask(e.Logger, compiledTask)
		}
		return nil
	}

	if e.Watch {
		return e.watchTasks(calls...)
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, c := range calls {
		c := c
		if e.Parallel {
			g.Go(func() error { return e.RunTask(ctx, c) })
		} else {
			if err := e.RunTask(ctx, c); err != nil {
				return err
			}
		}
	}
	return g.Wait()
}

// RunTask runs a task by its name
func (e *Executor) RunTask(ctx context.Context, call taskfile.Call) error {
	t, err := e.CompiledTask(call)
	if err != nil {
		return err
	}
	if !e.Watch && atomic.AddInt32(e.taskCallCount[t.Task], 1) >= MaximumTaskCall {
		return &MaximumTaskCallExceededError{task: t.Task}
	}

	release := e.acquireConcurrencyLimit()
	defer release()

	return e.startExecution(ctx, t, func(ctx context.Context) error {
		e.Logger.VerboseErrf(logger.Magenta, `task: "%s" started`, call.Task)
		if err := e.runDeps(ctx, t); err != nil {
			return err
		}

		if !e.Force {
			if err := ctx.Err(); err != nil {
				return err
			}

			preCondMet, err := e.areTaskPreconditionsMet(ctx, t)
			if err != nil {
				return err
			}

			upToDate, err := e.isTaskUpToDate(ctx, t)
			if err != nil {
				return err
			}

			if upToDate && preCondMet {
				if !e.Silent {
					e.Logger.Errf(logger.Magenta, `task: Task "%s" is up to date`, t.Name())
				}
				return nil
			}
		}

		if err := e.mkdir(t); err != nil {
			e.Logger.Errf(logger.Red, "task: cannot make directory %q: %v", t.Dir, err)
		}

		for i := range t.Cmds {
			if t.Cmds[i].Defer {
				defer e.runDeferred(t, call, i)
				continue
			}

			if err := e.runCommand(ctx, t, call, i); err != nil {
				if err2 := e.statusOnError(t); err2 != nil {
					e.Logger.VerboseErrf(logger.Yellow, "task: error cleaning status on error: %v", err2)
				}

				if execext.IsExitError(err) && t.IgnoreError {
					e.Logger.VerboseErrf(logger.Yellow, "task: task error ignored: %v", err)
					continue
				}

				return &TaskRunError{t.Task, err}
			}
		}
		e.Logger.VerboseErrf(logger.Magenta, `task: "%s" finished`, call.Task)
		return nil
	})
}

func (e *Executor) mkdir(t *taskfile.Task) error {
	if t.Dir == "" {
		return nil
	}

	mutex := e.mkdirMutexMap[t.Task]
	mutex.Lock()
	defer mutex.Unlock()

	if _, err := os.Stat(t.Dir); os.IsNotExist(err) {
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) runDeps(ctx context.Context, t *taskfile.Task) error {
	g, ctx := errgroup.WithContext(ctx)

	reacquire := e.releaseConcurrencyLimit()
	defer reacquire()

	for _, d := range t.Deps {
		d := d

		g.Go(func() error {
			err := e.RunTask(ctx, taskfile.Call{Task: d.Task, Vars: d.Vars})
			if err != nil {
				return err
			}
			return nil
		})
	}

	return g.Wait()
}

func (e *Executor) runDeferred(t *taskfile.Task, call taskfile.Call, i int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := e.runCommand(ctx, t, call, i); err != nil {
		e.Logger.VerboseErrf(logger.Yellow, `task: ignored error in deferred cmd: %s`, err.Error())
	}
}

func (e *Executor) runCommand(ctx context.Context, t *taskfile.Task, call taskfile.Call, i int) error {
	cmd := t.Cmds[i]

	switch {
	case len(cmd.Parallel) > 0:
		return e.runParallelCommands(ctx, t, call, cmd)
	case cmd.Task != "":
		reacquire := e.releaseConcurrencyLimit()
		defer reacquire()

		err := e.RunTask(ctx, taskfile.Call{Task: cmd.Task, Vars: cmd.Vars})
		if err != nil {
			return err
		}
		return nil
	case cmd.Cmd != "":
		if e.Verbose || (!cmd.Silent && !t.Silent && !e.Taskfile.Silent && !e.Silent) {
			e.Logger.Errf(logger.Green, "task: [%s] %s", t.Name(), cmd.Cmd)
		}

		if e.Dry {
			return nil
		}

		outputWrapper := e.Output
		if t.Interactive {
			outputWrapper = output.Interleaved{}
		}
		vars, err := e.Compiler.FastGetVariables(t, call)
		outputTemplater := &templater.Templater{Vars: vars, RemoveNoValue: true}
		if err != nil {
			return fmt.Errorf("task: failed to get variables: %w", err)
		}
		stdOut, stdErr, close := outputWrapper.WrapWriter(e.Stdout, e.Stderr, t.Prefix, outputTemplater)
		defer func() {
			if err := close(); err != nil {
				e.Logger.Errf(logger.Red, "task: unable to close writter: %v", err)
			}
		}()

		err = execext.RunCommand(ctx, &execext.RunCommandOptions{
			Command: cmd.Cmd,
			Dir:     t.Dir,
			Env:     getEnviron(t),
			Stdin:   e.Stdin,
			Stdout:  stdOut,
			Stderr:  stdErr,
		})
		if execext.IsExitError(err) && cmd.IgnoreError {
			e.Logger.VerboseErrf(logger.Yellow, "task: [%s] command error ignored: %v", t.Name(), err)
			return nil
		}
		return err
	default:
		return nil
	}
}

type cmdOutput struct {
	index  int
	stdout []byte
	stderr []byte
	err    error
}

func (e *Executor) runParallelCommands(ctx context.Context, t *taskfile.Task, call taskfile.Call, parentCmd *taskfile.Cmd) error {
	g, ctx := errgroup.WithContext(ctx)

	totalCmds := len(parentCmd.Parallel)
	completed := &atomic.Int32{}
	startTime := time.Now()

	// Save original stderr for progress display
	originalStderr := e.Stderr

	// Channel to capture outputs from each command
	outputsChan := make(chan cmdOutput, totalCmds)

	// Mutex to protect executor state during parallel task execution
	var executorMutex sync.Mutex

	// Start spinner in a separate goroutine using original stderr
	done := make(chan bool)
	go e.showParallelProgressWithWriter(totalCmds, completed, done, startTime, originalStderr)

	// Execute each command in parallel
	for idx, parallelCmd := range parentCmd.Parallel {
		idx := idx
		parallelCmd := parallelCmd

		g.Go(func() error {
			var stdoutBuf, stderrBuf []byte
			var err error

			switch {
			case parallelCmd.Task != "":
				// Run task call - capture output by replacing stdout/stderr
				stdoutWriter := &bufferWriter{buf: &stdoutBuf}
				stderrWriter := &bufferWriter{buf: &stderrBuf}

				// Lock while modifying shared executor state
				executorMutex.Lock()
				oldStdout := e.Stdout
				oldStderr := e.Stderr
				oldSilent := e.Silent

				e.Stdout = stdoutWriter
				e.Stderr = stderrWriter
				e.Silent = true // Suppress task logging messages
				executorMutex.Unlock()

				err = e.RunTask(ctx, taskfile.Call{Task: parallelCmd.Task, Vars: parallelCmd.Vars})

				// Lock while restoring shared executor state
				executorMutex.Lock()
				e.Stdout = oldStdout
				e.Stderr = oldStderr
				e.Silent = oldSilent
				executorMutex.Unlock()
			case parallelCmd.Cmd != "":
				// Run shell command - capture output
				stdoutBuf, stderrBuf, err = e.runCommandWithCapture(ctx, t, call, parallelCmd)
			}

			completed.Add(1)

			outputsChan <- cmdOutput{
				index:  idx,
				stdout: stdoutBuf,
				stderr: stderrBuf,
				err:    err,
			}

			return err
		})
	}

	// Wait for all commands to complete
	err := g.Wait()

	// Stop the spinner and clear the progress line immediately
	done <- true
	fmt.Fprintf(originalStderr, "\r\033[K") // Clear the line

	close(outputsChan)

	elapsed := time.Since(startTime)

	// Collect all outputs
	outputs := make([]cmdOutput, totalCmds)
	for output := range outputsChan {
		outputs[output.index] = output
	}

	// Print results
	e.printParallelResults(t, parentCmd, outputs, err, elapsed)

	return err
}

func (e *Executor) runCommandWithCapture(ctx context.Context, t *taskfile.Task, call taskfile.Call, cmd *taskfile.Cmd) ([]byte, []byte, error) {
	var stdoutBuf, stderrBuf []byte
	stdoutWriter := &bufferWriter{buf: &stdoutBuf}
	stderrWriter := &bufferWriter{buf: &stderrBuf}

	err := execext.RunCommand(ctx, &execext.RunCommandOptions{
		Command: cmd.Cmd,
		Dir:     t.Dir,
		Env:     getEnviron(t),
		Stdin:   e.Stdin,
		Stdout:  stdoutWriter,
		Stderr:  stderrWriter,
	})

	if execext.IsExitError(err) && cmd.IgnoreError {
		return stdoutBuf, stderrBuf, nil
	}

	return stdoutBuf, stderrBuf, err
}

type bufferWriter struct {
	buf *[]byte
	mu  sync.Mutex
}

func (w *bufferWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func (e *Executor) showParallelProgress(total int, completed *atomic.Int32, done chan bool, startTime time.Time) {
	e.showParallelProgressWithWriter(total, completed, done, startTime, e.Stderr)
}

func (e *Executor) showParallelProgressWithWriter(total int, completed *atomic.Int32, done chan bool, startTime time.Time, writer io.Writer) {
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerIdx := 0

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c := int(completed.Load())
			elapsed := time.Since(startTime)
			spinner := spinners[spinnerIdx%len(spinners)]
			spinnerIdx++

			// Clear line and print progress
			fmt.Fprintf(writer, "\r\033[K%s Running parallel commands: %d/%d (%.1fs)",
				spinner, c, total, elapsed.Seconds())
		}
	}
}

func (e *Executor) printParallelResults(t *taskfile.Task, parentCmd *taskfile.Cmd, outputs []cmdOutput, err error, elapsed time.Duration) {
	if err != nil {
		// Find which command failed and count completed
		completedCount := 0
		for i, output := range outputs {
			if output.err == nil {
				completedCount++
			} else {
				cmd := parentCmd.Parallel[i]
				cmdDesc := cmd.Cmd
				if cmdDesc == "" && cmd.Task != "" {
					cmdDesc = "task: " + cmd.Task
				}

				e.Logger.Errf(logger.Red, "Parallel commands failed: %d/%d completed (%.1fs)",
					completedCount, len(outputs), elapsed.Seconds())
				e.Logger.Errf(logger.Red, "Failed command: %s", cmdDesc)

				// Print output without labels
				if len(output.stdout) > 0 {
					fmt.Fprintf(e.Stdout, "\n%s", string(output.stdout))
				}
				if len(output.stderr) > 0 {
					fmt.Fprintf(e.Stderr, "%s", string(output.stderr))
				}
				break
			}
		}
	} else {
		// All succeeded
		e.Logger.Errf(logger.Green, "Parallel commands completed: %d/%d (%.1fs)",
			len(outputs), len(outputs), elapsed.Seconds())

		// Print outputs from successful commands only if log: true
		if parentCmd.Log {
			for i, output := range outputs {
				if len(output.stdout) > 0 || len(output.stderr) > 0 {
					cmd := parentCmd.Parallel[i]
					cmdDesc := cmd.Cmd
					if cmdDesc == "" && cmd.Task != "" {
						cmdDesc = "task: " + cmd.Task
					}

					if !cmd.Silent && !t.Silent && !e.Silent {
						e.Logger.Errf(logger.Default, "\n[%s]", cmdDesc)
						if len(output.stdout) > 0 {
							fmt.Fprintf(e.Stdout, "%s", string(output.stdout))
						}
						if len(output.stderr) > 0 {
							fmt.Fprintf(e.Stderr, "%s", string(output.stderr))
						}
					}
				}
			}
		}
	}
}

func getEnviron(t *taskfile.Task) []string {
	if t.Env == nil {
		return nil
	}

	environ := os.Environ()

	for k, v := range t.Env.ToCacheMap() {
		str, isString := v.(string)
		if !isString {
			continue
		}

		if _, alreadySet := os.LookupEnv(k); alreadySet {
			continue
		}

		environ = append(environ, fmt.Sprintf("%s=%s", k, str))
	}

	return environ
}

func (e *Executor) startExecution(ctx context.Context, t *taskfile.Task, execute func(ctx context.Context) error) error {
	h, err := e.GetHash(t)
	if err != nil {
		return err
	}

	if h == "" {
		return execute(ctx)
	}

	e.executionHashesMutex.Lock()
	otherExecutionCtx, ok := e.executionHashes[h]

	if ok {
		e.executionHashesMutex.Unlock()
		e.Logger.VerboseErrf(logger.Magenta, "task: skipping execution of task: %s", h)
		<-otherExecutionCtx.Done()
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	e.executionHashes[h] = ctx
	e.executionHashesMutex.Unlock()

	return execute(ctx)
}

// GetTask will return the task with the name matching the given call from the taskfile.
// If no task is found, it will search for tasks with a matching alias.
// If multiple tasks contain the same alias or no matches are found an error is returned.
func (e *Executor) GetTask(call taskfile.Call) (*taskfile.Task, error) {
	// Search for a matching task
	matchingTask, ok := e.Taskfile.Tasks[call.Task]
	if ok {
		return matchingTask, nil
	}

	// If didn't find one, search for a task with a matching alias
	var aliasedTasks []string
	for _, task := range e.Taskfile.Tasks {
		if slices.Contains(task.Aliases, call.Task) {
			aliasedTasks = append(aliasedTasks, task.Task)
			matchingTask = task
		}
	}
	// If we found multiple tasks
	if len(aliasedTasks) > 1 {
		return nil, &multipleTasksWithAliasError{
			aliasName: call.Task,
			taskNames: aliasedTasks,
		}
	}
	// If we found no tasks
	if len(aliasedTasks) == 0 {
		didYouMean := ""
		if e.fuzzyModel != nil {
			didYouMean = e.fuzzyModel.SpellCheck(call.Task)
		}
		return nil, &taskNotFoundError{
			taskName:   call.Task,
			didYouMean: didYouMean,
		}
	}

	return matchingTask, nil
}

type FilterFunc func(tasks []*taskfile.Task) []*taskfile.Task

func (e *Executor) GetTaskList(filters ...FilterFunc) []*taskfile.Task {
	tasks := make([]*taskfile.Task, 0, len(e.Taskfile.Tasks))

	// Fetch and compile the list of tasks
	for _, task := range e.Taskfile.Tasks {
		compiledTask, err := e.FastCompiledTask(taskfile.Call{Task: task.Task})
		if err == nil {
			task = compiledTask
		}
		tasks = append(tasks, task)

	}

	// Filter the tasks
	for _, filter := range filters {
		tasks = filter(tasks)
	}

	sortKey := func(t *taskfile.Task) (key string) {
		if t.Taskfile == path.Join(e.Dir, "Taskfile.yml") {
			key += "0"
		} else {
			key += "1"
		}

		return key + ":" + t.Task
	}

	// Sort the tasks
	// Tasks in the root taskfile go first
	// Then everything else
	sort.Slice(tasks, func(i, j int) bool {
		return sortKey(tasks[i]) < sortKey(tasks[j])
	})

	return tasks
}

// Filter is a generic task filtering function. It will remove each task in the
// slice where the result of the given function is true.
func Filter(f func(task *taskfile.Task) bool) FilterFunc {
	return func(tasks []*taskfile.Task) []*taskfile.Task {
		shift := 0
		for _, task := range tasks {
			if !f(task) {
				tasks[shift] = task
				shift++
			}
		}
		// This loop stops any memory leaks
		for j := shift; j < len(tasks); j++ {
			tasks[j] = nil
		}
		return slices.Clip(tasks[:shift])
	}
}

// FilterOutNoDesc removes all tasks that do not contain a description.
func FilterOutNoDesc() FilterFunc {
	return Filter(func(task *taskfile.Task) bool {
		return task.Desc == ""
	})
}

// FilterOutInternal removes all tasks that are marked as internal.
func FilterOutInternal() FilterFunc {
	return Filter(func(task *taskfile.Task) bool {
		return task.Internal
	})
}
