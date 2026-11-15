// The job loop below is gitlab-runner v19.5.2's, from commands/multi.go
// (MIT; see LICENSE.gitlab-runner): feedRunners → processRunners →
// processRunner → requestJob → processBuildOnRunner. Functions keep their
// names and order, so each release can be compared with this copy. What is
// left out: config reload, the session server, usage logs, the boot-verify
// canary, the runtime environment sweep, token reset and the metrics and
// debug server. What differs is marked "fleetci:".

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/network"
)

const (
	workerProcessingFailureOther          = "other"
	workerProcessingFailureNoFreeExecutor = "no_free_executor"
	workerProcessingFailureJobFailure     = "job_failure"
)

// fleetci: errInterrupted stands for the signal gitlab-runner broadcasts on
// runInterruptSignal and abortBuilds.
var errInterrupted = errors.New("fleetci is stopping")

type interruptSignal struct{}

func (interruptSignal) String() string { return errInterrupted.Error() }
func (interruptSignal) Signal()        {}

type RunCommand struct {
	network           common.Network
	executorProviders executors.Providers

	healthHelper healthHelper
	buildsHelper buildsHelper

	// fleetci: the config is built once from fleetci.toml, not reloaded.
	config *common.Config

	inputsMetricsCollector *spec.JobInputsMetricsCollector

	// abortBuilds is used to abort running builds
	abortBuilds chan os.Signal

	// runInterruptSignal is used to abort current operation (scaling workers, waiting for config)
	//
	// fleetci: closed rather than pumped forever, so no goroutine outlives
	// the loop. A receive from a closed channel is the same broadcast.
	runInterruptSignal chan os.Signal

	// stopSignal is used to preserve the signal that was used to stop the
	// process.
	//
	// fleetci: atomic, as several goroutines read it while Stop sets it.
	stopSignal atomic.Pointer[os.Signal]

	// runFinished is used to notify that run() did finish
	runFinished chan bool

	currentWorkers int

	interruptOnce sync.Once
	abortOnce     sync.Once

	// fleetci: dispatch, on a fleet's leader, sends each job taken to a
	// worker instead of running it here.
	dispatch *dispatcher
}

func (mr *RunCommand) log() *logrus.Entry {
	return logrus.WithFields(logrus.Fields{
		"builds":     mr.buildsHelper.buildsCount(),
		"max_builds": mr.config.Concurrent,
	})
}

func (mr *RunCommand) stopping() bool { return mr.stopSignal.Load() != nil }

func (mr *RunCommand) run() {
	mr.initUsedExecutorProviders()

	runners := make(chan *common.RunnerConfig)
	go mr.feedRunners(runners)

	startWorker := make(chan int)
	stopWorker := make(chan bool)
	go mr.startWorkers(startWorker, stopWorker, runners)

	workerIndex := 0

	// Update number of workers.
	// Exits when mr.runInterruptSignal receives a signal.
	for !mr.stopping() {
		signaled := mr.updateWorkers(&workerIndex, startWorker, stopWorker)
		if signaled != nil {
			break
		}

		// fleetci: in place of updateConfig, which also waits for the
		// interrupt.
		select {
		case <-mr.runInterruptSignal:
		case <-time.After(common.ReloadConfigInterval):
		}
	}

	// Workers exit when their current job is done; providers need not wait for that.
	mr.beginShutdownOfExecutorProviders()

	// Wait for workers to shut down
	mr.stopWorkers(stopWorker)
	mr.log().Info("All workers stopped.")

	mr.shutdownUsedExecutorProviders()
	mr.log().Info("All executor providers shut down.")

	close(mr.runFinished)

	mr.log().Info("Can exit now!")
}

func (mr *RunCommand) initUsedExecutorProviders() {
	mr.log().Info("Initializing executor providers")

	for _, provider := range mr.executorProviders.All() {
		managedProvider, ok := provider.(common.ManagedExecutorProvider)
		if ok {
			managedProvider.Init(mr.config)
		}
	}
}

func (mr *RunCommand) beginShutdownOfExecutorProviders() {
	for _, provider := range mr.executorProviders.All() {
		if p, ok := provider.(common.ShutdownAwareExecutorProvider); ok {
			p.BeginShutdown(mr.config)
		}
	}
}

func (mr *RunCommand) shutdownUsedExecutorProviders() {
	shutdownTimeout := mr.config.GetShutdownTimeout()

	logger := mr.log().WithField("shutdown-timeout", shutdownTimeout)
	logger.Info("Shutting down executor providers")

	ctx, cancelFn := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelFn()

	wg := new(sync.WaitGroup)
	for _, provider := range mr.executorProviders.All() {
		managedProvider, ok := provider.(common.ManagedExecutorProvider)
		if ok {
			wg.Add(1)
			go func(p common.ManagedExecutorProvider) {
				defer wg.Done()
				p.Shutdown(ctx, mr.config)
			}(managedProvider)
		}
	}

	wg.Wait()

	if ctx.Err() != nil {
		logger.Warn("Executor providers shutdown timeout exceeded")
	}
}

func (mr *RunCommand) feedRunners(runners chan *common.RunnerConfig) {
	for !mr.stopping() {
		mr.log().Debugln("Feeding runners to channel")
		config := mr.config

		// If no runners wait full interval to test again
		if len(config.Runners) == 0 {
			mr.sleep(config.GetCheckInterval())
			continue
		}

		interval := config.GetCheckInterval() / time.Duration(len(config.Runners))

		// Feed runner with waiting exact amount of time
		for _, runner := range config.Runners {
			mr.feedRunner(runner, runners)
			mr.sleep(interval)
		}
	}

	mr.log().
		WithField("StopSignal", mr.stopSignal.Load()).
		Debug("Stopping feeding runners to channel")
}

// fleetci: sleep is time.Sleep that ends early when the loop stops, so
// feedRunners doesn't outlive it.
func (mr *RunCommand) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-mr.runInterruptSignal:
	}
}

func (mr *RunCommand) feedRunner(runner *common.RunnerConfig, runners chan *common.RunnerConfig) {
	if !runner.HasToken() {
		return
	}

	if !mr.healthHelper.isHealthy(runner) {
		return
	}

	mr.log().WithField("runner", runner.ShortDescription()).Debugln("Feeding runner to channel")
	// fleetci: gives up when the loop stops, instead of blocking on
	// workers that have exited.
	select {
	case runners <- runner:
	case <-mr.runInterruptSignal:
	}
}

// startWorkers is responsible for starting the workers (up to the number
// defined by `concurrent`) and assigning a runner processing method to them.
func (mr *RunCommand) startWorkers(startWorker chan int, stopWorker chan bool, runners chan *common.RunnerConfig) {
	for !mr.stopping() {
		// fleetci: also returns when the loop stops.
		select {
		case id := <-startWorker:
			go mr.processRunners(id, stopWorker, runners)
		case <-mr.runInterruptSignal:
			return
		}
	}
}

// processRunners is responsible for processing a Runner on a worker (when received
// a runner information sent to the channel by feedRunners) and for terminating the worker
// (when received an information on stoWorker chan - provided by updateWorkers)
func (mr *RunCommand) processRunners(id int, stopWorker chan bool, runners chan *common.RunnerConfig) {
	mr.log().
		WithField("worker", id).
		Debugln("Starting worker")

	for !mr.stopping() {
		select {
		case runner := <-runners:
			err := mr.processRunner(id, runner, runners)
			if err != nil {
				logger := mr.log().
					WithFields(logrus.Fields{
						"runner":      runner.ShortDescription(),
						"runner_name": runner.Name,
						"executor":    runner.Executor,
					}).WithError(err)

				l, _ := loggerAndFailureTypeFromError(logger, err)
				l("Failed to process runner")
			}

		case <-stopWorker:
			mr.log().
				WithField("worker", id).
				Debugln("Stopping worker")

			return
		}
	}
	<-stopWorker
}

func loggerAndFailureTypeFromError(logger logrus.FieldLogger, err error) (func(args ...any), string) {
	var NoFreeExecutorError *common.NoFreeExecutorError
	if errors.As(err, &NoFreeExecutorError) {
		return logger.Debug, workerProcessingFailureNoFreeExecutor
	}

	var BuildError *common.BuildError
	if errors.As(err, &BuildError) {
		return logger.Debug, workerProcessingFailureJobFailure
	}

	return logger.Warn, workerProcessingFailureOther
}

// processRunner is responsible for handling one job on a specified runner.
// First it acquires the Build to check if `limit` was met. If it's still in the capacity
// it creates the debug session (for debug terminal), triggers a job request to configured
// GitLab instance and finally creates and finishes the job.
// To speed-up jobs handling before starting the job this method "requeues" the runner to another
// worker (by feeding the channel normally handled by feedRunners).
func (mr *RunCommand) processRunner(id int, runner *common.RunnerConfig, runners chan *common.RunnerConfig) error {
	runnerFields := logrus.Fields{
		"runner":      runner.ShortDescription(),
		"runner_name": runner.Name,
	}

	mr.log().WithFields(runnerFields).Debugln("Processing runner")

	provider := mr.executorProviders.GetByName(runner.Executor)
	if provider == nil {
		mr.log().
			WithFields(runnerFields).
			Errorf("Executor %q is not known; marking Runner as unhealthy", runner.Executor)
		mr.healthHelper.markHealth(runner, false)

		return nil
	}

	mr.log().WithField("runner", runner.ShortDescription()).Debug("Acquiring job slot")
	if !mr.buildsHelper.acquireBuild(runner) {
		logrus.WithFields(runnerFields).WithField("worker", id).Debug("Failed to request job, runner limit met")

		return nil
	}
	defer mr.buildsHelper.releaseBuild(runner)

	// Acquire request for job
	// We must ensure that this is released after the job request, or earlier if there's an
	// error before the job request is made.
	mr.log().WithFields(runnerFields).Debug("Acquiring request slot")
	if !mr.buildsHelper.acquireRequest(runner) {
		mr.log().WithFields(runnerFields).
			Debugln("Failed to request job: 'request_concurrency' already reached, see https://docs.gitlab.com/runner/configuration/advanced-configuration/#the-runners-section")
		return nil
	}

	// fleetci: this is where fleetci's provider checks for room for one
	// VM of the runner's class, or fails with NoFreeExecutorError, so no
	// job leaves GitLab's queue while the host is full.
	mr.log().WithFields(runnerFields).Debug("Acquiring executor from provider")
	executorData, err := provider.Acquire(runner)
	if err != nil {
		// Release job request
		mr.buildsHelper.releaseRequest(runner, false, false)

		return fmt.Errorf("failed to update executor: %w", err)
	}
	defer provider.Release(runner, executorData)

	// fleetci: a fleet's leader runs no build itself.
	if mr.dispatch != nil {
		return mr.dispatchBuild(runner, runners)
	}

	return mr.processBuildOnRunner(runner, runners, provider, executorData)
}

func (mr *RunCommand) processBuildOnRunner(
	runner *common.RunnerConfig,
	runners chan *common.RunnerConfig,
	provider common.ExecutorProvider,
	executorData common.ExecutorData,
) (err error) {
	// fleetci: no session server, so no debug terminal session
	// (createSession).

	// Receive a new build
	trace, jobData, retried, err := mr.requestJob(runner, nil)
	// Release job request
	mr.buildsHelper.releaseRequest(runner, jobData != nil, retried)
	if err != nil || jobData == nil {
		return err
	}
	defer func() { mr.traceOutcome(trace, err) }()

	// Create a new build
	build, err := common.NewBuild(*jobData, runner, mr.abortBuilds, executorData, provider)
	if err != nil {
		return err
	}
	build.ArtifactUploader = mr.network.UploadRawArtifacts

	trace.SetDebugModeEnabled(build.IsDebugModeEnabled())

	// fleetci: no tracing spans.
	ctx := context.Background()

	// Add build to list of builds to assign numbers
	mr.buildsHelper.addBuild(build)

	fields := logrus.Fields{
		"runner_uuid":           build.RunnerInfo.UUID,
		"runner":                runner.ShortDescription(),
		"runner_name":           runner.Name,
		"job":                   build.ID,
		"pipeline_id":           build.JobInfo.PipelineID,
		"project":               build.JobInfo.ProjectID,
		"project_full_path":     build.JobInfo.ProjectFullPath,
		"namespace_id":          build.JobInfo.NamespaceID,
		"root_namespace_id":     build.JobInfo.RootNamespaceID,
		"gitlab_user_id":        build.JobInfo.UserID,
		"repo_url":              build.RepoCleanURL(),
		"time_in_queue_seconds": build.JobInfo.TimeInQueueSeconds,
		"queue_size":            build.JobInfo.QueueSize,
		"queue_depth":           build.JobInfo.QueueDepth,
	}

	if build.JobInfo.ScopedUserID != nil {
		fields["gitlab_scoped_user_id"] = *build.JobInfo.ScopedUserID
	}

	mr.log().WithFields(fields).Infoln("Added job to processing list")
	defer mr.finishBuild(runner, build, fields)
	if !runner.GetStrictCheckInterval() {
		// Process the same runner by different worker again
		// to speed up taking the builds
		mr.requeueRunner(runner, runners)
	}
	// Process a build
	return build.Run(ctx, mr.config, trace)
}

func (mr *RunCommand) traceOutcome(trace common.JobTrace, err error) {
	if err != nil {
		fmt.Fprintln(trace, err.Error())

		logTerminationError(
			mr.log(),
			"Fail",
			trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure}),
		)

		return
	}

	logTerminationError(mr.log(), "Success", trace.Success())
}

func logTerminationError(logger logrus.FieldLogger, name string, err error) {
	if err != nil {
		logger.WithError(err).Errorf("Job trace termination %q failed", name)
	}
}

// finishBuild is invoked via defer from processBuildOnRunner when a build
// finishes. It removes the build from the active list.
//
// fleetci: no usage log record.
func (mr *RunCommand) finishBuild(_ *common.RunnerConfig, build *common.Build, fields logrus.Fields) {
	if !mr.buildsHelper.removeBuild(build) {
		return
	}

	mr.log().WithFields(fields).Infoln("Removed job from processing list")
}

// requestJob will check if the runner can send another concurrent request to
// GitLab, if not the return value is nil.
//
// The returned `retried` flag covers only the initial job pickup: it is
// snapshotted before any post-pickup network calls so ProcessJob/UpdateJob
// retries cannot pollute the adaptive-concurrency signal.
func (mr *RunCommand) requestJob(
	runner *common.RunnerConfig,
	sessionInfo *common.SessionInfo,
) (common.JobTrace, *spec.Job, bool, error) {
	reqCtx, retriedFlag := network.WithRetryTracker(context.Background())

	jobData, healthy := mr.doJobRequest(reqCtx, runner, sessionInfo)
	retried := retriedFlag.Load()
	mr.healthHelper.markHealth(runner, healthy)

	if jobData == nil {
		return nil, nil, retried, nil
	}

	// Inject metrics collector into JobInputs
	jobData.Inputs.SetMetricsCollector(mr.inputsMetricsCollector)

	// Make sure to always close output
	jobCredentials := &common.JobCredentials{
		ID:    jobData.ID,
		Token: jobData.Token,
	}

	trace, err := mr.network.ProcessJob(*runner, jobCredentials)
	if err != nil {
		jobInfo := common.UpdateJobInfo{
			ID:            jobCredentials.ID,
			State:         common.Failed,
			FailureReason: common.RunnerSystemFailure,
		}

		// send failure once
		mr.network.UpdateJob(*runner, jobCredentials, jobInfo)
		return nil, nil, retried, err
	}

	if err := errors.Join(jobData.UnsupportedOptions(),
		jobData.ValidateStepsJobRequest(mr.executorSupportsNativeSteps(runner))); err != nil {
		_, _ = trace.Write([]byte(err.Error() + "\n"))

		err = trace.Fail(err, common.JobFailureData{
			Reason:   common.RunnerSystemFailure,
			ExitCode: common.ExitCodeUnsupportedOptions,
		})
		logTerminationError(mr.log(), "Fail", err)

		return nil, nil, retried, err
	}

	updateResult := mr.network.UpdateJob(*runner, jobCredentials, common.UpdateJobInfo{
		ID:    jobCredentials.ID,
		State: common.Running,
	})

	if updateResult.State == common.UpdateAbort {
		trace.Finish()
		return nil, nil, retried, nil
	}

	if updateResult.CancelRequested {
		err := trace.Fail(common.ErrJobCanceled, common.JobFailureData{Reason: common.JobCanceled})
		logTerminationError(mr.log(), "Fail", err)
		return nil, nil, retried, nil
	}

	return trace, jobData, retried, nil
}

func (mr *RunCommand) executorSupportsNativeSteps(runnerConfig *common.RunnerConfig) bool {
	netCli, ok := mr.network.(*network.GitLabClient)
	return ok && netCli.ExecutorSupportsNativeSteps(*runnerConfig)
}

// doJobRequest will execute the request for a new job, respecting an interruption
// caused by interrupt signals or process execution finalization
func (mr *RunCommand) doJobRequest(
	ctx context.Context,
	runner *common.RunnerConfig,
	sessionInfo *common.SessionInfo,
) (*spec.Job, bool) {
	// Terminate opened requests to GitLab when interrupt signal
	// is broadcast.
	ctx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	go func() {
		select {
		case <-mr.runInterruptSignal:
			cancelFn()
		case <-mr.runFinished:
			cancelFn()
		case <-ctx.Done():
		}
	}()

	return mr.network.RequestJob(ctx, *runner, sessionInfo)
}

// requeueRunner feeds the runners channel in a non-blocking way. This replicates the
// behavior of feedRunners and speeds-up jobs handling. But if the channel is full, the
// method just exits without blocking.
func (mr *RunCommand) requeueRunner(runner *common.RunnerConfig, runners chan *common.RunnerConfig) {
	runnerLog := mr.log().WithField("runner", runner.ShortDescription()).WithField("runner_name", runner.Name)

	select {
	case runners <- runner:
		runnerLog.Debugln("Requeued the runner")

	default:
		runnerLog.Debugln("Failed to requeue the runner")
	}
}

// updateWorkers, called periodically from run() is responsible for scaling the pool
// of workers. By worker we don't understand a `[[runners]]` entry, but a "slot" that will
// use one of the runners to request and handle a job.
// The size of the workers pool is controlled by `concurrent` setting. This method is responsible
// for the fact that `concurrent` defines the upper number of jobs that can be concurrently handled
// by GitLab Runner process.
func (mr *RunCommand) updateWorkers(workerIndex *int, startWorker chan int, stopWorker chan bool) os.Signal {
	concurrentLimit := mr.config.Concurrent

	for mr.currentWorkers > concurrentLimit {
		// Too many workers. Trigger stop on one of them
		// or exit if termination signal was broadcasted.
		select {
		case stopWorker <- true:
		case <-mr.runInterruptSignal:
			return interruptSignal{}
		}
		mr.currentWorkers--
	}

	for mr.currentWorkers < concurrentLimit {
		// Too few workers. Trigger a creation of a new one
		// or exit if termination signal was broadcasted.
		select {
		case startWorker <- *workerIndex:
		case <-mr.runInterruptSignal:
			return interruptSignal{}
		}
		mr.currentWorkers++

		*workerIndex++
	}

	return nil
}

func (mr *RunCommand) stopWorkers(stopWorker chan bool) {
	for mr.currentWorkers > 0 {
		stopWorker <- true
		mr.currentWorkers--
	}
}

// Stop triggers the process stop: no new jobs are requested, running jobs
// are aborted, and it waits for run() up to shutdown_timeout.
//
// fleetci: one forceful strategy, gitlab-runner's for SIGTERM and SIGINT
// (interruptRun, then handleForcefulShutdown). SIGQUIT's graceful one,
// which waits for jobs to finish, comes with the cluster.
func (mr *RunCommand) Stop() error {
	sig := os.Signal(interruptSignal{})
	mr.stopSignal.CompareAndSwap(nil, &sig)

	mr.interruptRun()

	return mr.handleForcefulShutdown()
}

// interruptRun broadcasts interrupt signal, which exits the workers
// scaling goroutine.
func (mr *RunCommand) interruptRun() {
	mr.log().Debug("Broadcasting interrupt signal")
	mr.interruptOnce.Do(func() { close(mr.runInterruptSignal) })
}

func (mr *RunCommand) handleForcefulShutdown() error {
	mr.log().
		WithField("shutdown-timeout", mr.config.GetShutdownTimeout()).
		Warning("Starting forceful shutdown")

	mr.abortAllBuilds()

	// Wait for graceful shutdown or abort after timeout
	select {
	case <-time.After(mr.config.GetShutdownTimeout()):
		return errors.New("shutdown timed out")

	case <-mr.runFinished:
		// Everything finished we can exit now
		return nil
	}
}

// abortAllBuilds broadcasts abort signal, which ends with all currently executed
// jobs being interrupted and terminated.
func (mr *RunCommand) abortAllBuilds() {
	mr.log().Debug("Broadcasting job abort signal")
	mr.abortOnce.Do(func() { close(mr.abortBuilds) })
}
