package gitlab

// A fleet's GitLab jobs: the leader takes each job from GitLab with the
// runner token and places it on a worker, which runs it with only the
// job's own token. gitlab-runner's network client sends the runner token
// only to request jobs (network/gitlab.go: RequestJob); UpdateJob,
// PatchTrace and the artifact calls send JobCredentials.Token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker"
	"gitlab.com/gitlab-org/gitlab-runner/network"
	"gitlab.com/gitlab-org/moa/value"
	"gitlab.com/gitlab-org/step-runner/pkg/api/expression"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// Remote runs a job on the worker that owns vm and returns when the job
// ends there. job is the job as GitLab gave it; runner the [[runners]]
// table without its token.
type Remote func(ctx context.Context, vm source.VM, job, runner []byte) error

// NewDispatch is New for a fleet's leader: each job taken goes to a VM of
// its class from capacity, and remote runs it there.
func NewDispatch(ctx context.Context, c Config, capacity source.Capacity, remote Remote, log hclog.Logger) (*Loop, error) {
	l, err := New(ctx, c, capacity, log)
	if err != nil {
		return nil, err
	}
	classes, defaultClass, err := capacity.Classes(ctx)
	if err != nil {
		return nil, err
	}
	p := l.mr.executorProviders.GetByName(executorName).(*provider)
	l.mr.dispatch = &dispatcher{ctx: ctx, capacity: capacity, remote: remote, log: log,
		classes: source.Names(classes), defaultClass: defaultClass, classOf: p.classOf,
		held: held(c.HeldDir), resume: c.Held, exiting: c.Exiting}
	if c.Concurrent == 0 {
		// The fleet grows as workers join; room, not this, holds jobs back.
		l.mr.config.Concurrent = 256
	}
	return l, nil
}

type dispatcher struct {
	ctx          context.Context
	capacity     source.Capacity
	remote       Remote
	log          hclog.Logger
	classes      []string
	defaultClass string
	classOf      func(*common.RunnerConfig) string
	held         held
	resume       *Held       // the jobs the last process left waiting
	exiting      func() bool // whether the controller exits, not only stops leading
}

// dispatchBuild takes one job from GitLab and runs it on a worker. It
// returns when the job ends, so the loop's concurrent holds across the
// fleet.
func (mr *RunCommand) dispatchBuild(runner *common.RunnerConfig, runners chan *common.RunnerConfig) error {
	d := mr.dispatch
	reqCtx, retriedFlag := network.WithRetryTracker(context.Background())
	job, healthy := mr.doJobRequest(reqCtx, runner, nil)
	mr.healthHelper.markHealth(runner, healthy)
	mr.buildsHelper.releaseRequest(runner, job != nil, retriedFlag.Load())
	if job == nil {
		return nil
	}
	if !runner.GetStrictCheckInterval() {
		mr.requeueRunner(runner, runners)
	}
	creds := &common.JobCredentials{ID: job.ID, Token: job.Token}
	log := d.log.With("job", job.ID, "project", job.JobInfo.ProjectID)
	rc := *runner
	rc.Token = runner.ShortDescription() // the worker never sees the runner token

	class := d.classOf(runner) // a runner of a class takes that class's jobs
	for _, v := range job.Variables {
		if class != "" {
			break
		}
		if v.Key == classVariable {
			class = v.Value
		}
	}
	if class == "" {
		class = d.defaultClass
	}
	if !slices.Contains(d.classes, class) {
		return mr.failJob(rc, creds, fmt.Errorf("unknown class %q; the fleet has %v (default %s)", class, d.classes, d.defaultClass), log)
	}
	jobJSON, err := marshalJob(job)
	if err != nil {
		return mr.failJob(rc, creds, err, log)
	}
	runnerJSON, err := json.Marshal(&rc)
	if err != nil {
		return mr.failJob(rc, creds, err, log)
	}
	n := heldJob{ID: job.ID, Token: job.Token, URL: runner.URL, Class: class, Job: jobJSON, Runner: runnerJSON}
	d.held.put(n, log)
	return mr.place(rc, n, log)
}

// place runs a job taken, and noted, on a VM of its class, and returns when
// it ends there. With no VM for it, it fails the job; but a controller
// that exits leaves it noted, for its next start to place.
func (mr *RunCommand) place(rc common.RunnerConfig, n heldJob, log hclog.Logger) error {
	d := mr.dispatch
	creds := &common.JobCredentials{ID: n.ID, Token: n.Token}
	ctx, cancel := context.WithTimeout(d.ctx, defaultAcquireWait)
	vm, err := d.capacity.Create(ctx, n.Class)
	cancel()
	if err != nil {
		if d.ctx.Err() != nil {
			if d.held != "" && d.exiting != nil && d.exiting() {
				log.Info("the controller stops before the job had a VM: its next start places it")
				return nil
			}
			err = errors.New("the controller stopped leading before the job had a VM")
		}
		d.held.drop(n.ID)
		return mr.failJob(rc, creds, fmt.Errorf("no %s VM: %w", n.Class, err), log)
	}
	log.Info("job placed", "class", n.Class, "vm", vm.ID)
	d.held.drop(n.ID) // the worker has it now, and ends it
	if err := d.remote(d.ctx, vm, n.Job, n.Runner); err != nil {
		if d.ctx.Err() != nil {
			return nil // the leader stops; the job goes on on its worker
		}
		log.Warn("job ended on its worker with an error", "vm", vm.ID, "err", err)
		return nil
	}
	log.Info("job done", "vm", vm.ID)
	return nil
}

// resumeHeld places the jobs the controller's last process took and left
// waiting for a VM, each as it would have.
func (mr *RunCommand) resumeHeld() {
	d := mr.dispatch
	if d == nil {
		return
	}
	for _, n := range d.resume.take() {
		log := d.log.With("job", n.ID)
		var rc common.RunnerConfig
		err := json.Unmarshal(n.Runner, &rc)
		switch {
		case err != nil || len(n.Job) == 0:
			// A note without the job, from before notes held it, can
			// only be failed.
			rc.URL = n.URL
			d.held.drop(n.ID)
			_ = mr.failJob(rc, &common.JobCredentials{ID: n.ID, Token: n.Token}, errors.New("the controller restarted before the job had a VM"), log)
		case !slices.Contains(d.classes, n.Class):
			d.held.drop(n.ID)
			_ = mr.failJob(rc, &common.JobCredentials{ID: n.ID, Token: n.Token}, fmt.Errorf("class %q is gone from the fleet", n.Class), log)
		default:
			log.Info("placing a job taken before a restart", "class", n.Class)
			go func() { _ = mr.place(rc, n, log) }()
		}
	}
}

// failJob fails a job taken that can't be placed, as a runner system
// failure, which a job's retry can take, saying why in its log.
func (mr *RunCommand) failJob(rc common.RunnerConfig, creds *common.JobCredentials, err error, log hclog.Logger) error {
	log.Error("job not placed; failing it", "err", err)
	trace, terr := mr.network.ProcessJob(rc, creds)
	if terr != nil {
		mr.network.UpdateJob(rc, creds, common.UpdateJobInfo{ID: creds.ID, State: common.Failed, FailureReason: common.RunnerSystemFailure})
		return err
	}
	fmt.Fprintln(trace, "fleetci: "+err.Error())
	logTerminationError(mr.log(), "Fail", trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure}))
	return err
}

// marshalJob is the job as GitLab sent it. spec.Job round-trips through
// JSON but for its inputs, which keep only their values: they go back in
// GitLab's form, {key, value: {type, content, sensitive}}.
func marshalJob(job *spec.Job) ([]byte, error) {
	b, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	inputs := []spec.JobInput{}
	for k, v := range job.Inputs.All() {
		t, err := inputType(v.Kind())
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", k.String(), err)
		}
		inputs = append(inputs, spec.JobInput{Key: k.String(), Value: spec.JobInputValue{
			Type: t, Content: v, Sensitive: v.HasMarks(expression.Sensitive)}})
	}
	if m["inputs"], err = json.Marshal(inputs); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func inputType(k value.Kind) (spec.JobInputContentTypeName, error) {
	switch k {
	case value.StringKind:
		return spec.JobInputContentTypeNameString, nil
	case value.NumberKind:
		return spec.JobInputContentTypeNameNumber, nil
	case value.BoolKind:
		return spec.JobInputContentTypeNameBoolean, nil
	case value.ArrayKind:
		return spec.JobInputContentTypeNameArray, nil
	case value.ObjectKind:
		return spec.JobInputContentTypeNameStruct, nil
	}
	return "", fmt.Errorf("kind %v", k)
}

// defaultAcquireWait is how long a job waits for room in the fleet, under
// GitLab's default job timeout (1h).
const defaultAcquireWait = 50 * time.Minute

// RunJob runs a job the leader placed on this worker's VM, with the job's
// token, to its end: gitlab-runner's requestJob after the request, then
// the build with the Docker executor on the VM.
func RunJob(ctx context.Context, vm source.VM, conn *proxy.Conn, jobJSON, runnerJSON []byte, log hclog.Logger) error {
	rc := &common.RunnerConfig{}
	if err := json.Unmarshal(runnerJSON, rc); err != nil {
		return fmt.Errorf("the runner: %w", err)
	}
	var job spec.Job
	if err := json.Unmarshal(jobJSON, &job); err != nil {
		// The job is GitLab's still: fail it there, with its own token.
		var id struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		}
		if json.Unmarshal(jobJSON, &id) == nil && id.Token != "" {
			netw := network.NewGitLabClient()
			netw.UpdateJob(*rc, &common.JobCredentials{ID: id.ID, Token: id.Token},
				common.UpdateJobInfo{ID: id.ID, State: common.Failed, FailureReason: common.RunnerSystemFailure})
		}
		return fmt.Errorf("the job: %w", err)
	}
	rc.Executor = executorName
	if rc.Docker == nil {
		rc.Docker = &common.DockerConfig{Image: "ubuntu:24.04"}
	}
	common.AppVersion.Version = RunnerVersion
	common.AppVersion.Revision = runnerRevision

	single := &oneVM{vm: vm, conn: conn}
	p := newProvider(docker.NewProvider(), single, func(*common.RunnerConfig) string { return vm.Class }, []string{vm.Class}, vm.Class)
	netw := network.NewGitLabClient(network.WithExecutorProviderFunc(func(string) common.ExecutorProvider { return p }))

	creds := &common.JobCredentials{ID: job.ID, Token: job.Token}
	trace, err := netw.ProcessJob(*rc, creds)
	if err != nil {
		netw.UpdateJob(*rc, creds, common.UpdateJobInfo{ID: job.ID, State: common.Failed, FailureReason: common.RunnerSystemFailure})
		return err
	}
	if err := errors.Join(job.UnsupportedOptions(), job.ValidateStepsJobRequest(netw.ExecutorSupportsNativeSteps(*rc))); err != nil {
		_, _ = trace.Write([]byte(err.Error() + "\n"))
		return trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure, ExitCode: common.ExitCodeUnsupportedOptions})
	}
	upd := netw.UpdateJob(*rc, creds, common.UpdateJobInfo{ID: job.ID, State: common.Running})
	if upd.State == common.UpdateAbort {
		trace.Finish()
		return nil
	}
	if upd.CancelRequested {
		return trace.Fail(common.ErrJobCanceled, common.JobFailureData{Reason: common.JobCanceled})
	}

	data, err := p.Acquire(rc)
	if err != nil {
		fmt.Fprintln(trace, err.Error())
		return trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure})
	}
	defer p.Release(rc, data)
	abort := make(chan os.Signal, 1)
	build, err := common.NewBuild(job, rc, abort, data, p)
	if err != nil {
		fmt.Fprintln(trace, err.Error())
		return trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure})
	}
	build.ArtifactUploader = netw.UploadRawArtifacts
	trace.SetDebugModeEnabled(build.IsDebugModeEnabled())
	stop := context.AfterFunc(ctx, func() {
		select {
		case abort <- os.Interrupt:
		default:
		}
	})
	defer stop()
	log.Info("gitlab job running", "job", job.ID, "vm", vm.ID)
	config := common.NewConfig()
	err = build.Run(context.Background(), config, trace)
	if err != nil {
		fmt.Fprintln(trace, err.Error())
		logTerminationError(logrus.StandardLogger(), "Fail", trace.Fail(err, common.JobFailureData{Reason: common.RunnerSystemFailure}))
		return err
	}
	logTerminationError(logrus.StandardLogger(), "Success", trace.Success())
	return nil
}

// oneVM is vms with the one VM the leader placed the job on.
type oneVM struct {
	vm   source.VM
	conn *proxy.Conn
	used bool
}

func (o *oneVM) Reserve(class string) (reservation, error) {
	if o.used || class != o.vm.Class {
		return nil, source.ErrNoRoom
	}
	o.used = true
	return oneReservation{o.vm}, nil
}

func (o *oneVM) Create(context.Context, string) (source.VM, error) {
	return source.VM{}, errors.New("the job's VM is gone")
}

func (o *oneVM) Conn(string) (*proxy.Conn, error) { return o.conn, nil }

// Delete leaves the VM to the worker, which removes it after the job.
func (o *oneVM) Delete(context.Context, string) error { return nil }

type oneReservation struct{ vm source.VM }

func (r oneReservation) Class() string                              { return r.vm.Class }
func (r oneReservation) Acquire(context.Context) (source.VM, error) { return r.vm, nil }
func (r oneReservation) Release()                                   {}
