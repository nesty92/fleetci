// The provider below is built as gitlab-runner v19.5.2's docker-autoscaler
// is, from executors/internal/autoscaler (provider.go, executor.go and
// acquisition.go; MIT, see LICENSE.gitlab-runner), with fleetci's host queue
// in place of taskscaler and without nesting, suspend and resume:
//
//   - provider.Acquire reserves room for one VM of the runner's class before
//     the job is requested, or fails with NoFreeExecutorError;
//   - executor.Prepare, once a job has arrived, takes the VM;
//   - acquisitionRef.Prepare is the executors.Environment the Docker
//     executor runs the job on: it dials the VM with fleeting's connector.
//
// A runner of gitlab-runner's config.toml has no class of its own: the
// job's FLEETCI_CLASS names it, as with fleetci's plugin. Its Acquire
// reserves room for a VM of the first class that has some, the default
// class first, so it asks for no job while the host is full; Prepare uses
// the reservation when the job's class is the reserved one, and otherwise
// gives it back and waits for room in the job's class.

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"
	fleetingprovider "gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/session/terminal"
	"gitlab.com/gitlab-org/gitlab-runner/steps"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// executorName is the executor fleetci's runners name.
const executorName = "fleetci"

// classVariable is the job variable that names a job's class on runners
// without a class of their own.
const classVariable = "FLEETCI_CLASS"

// reservation is room for one VM, held until Acquire or Release.
type reservation interface {
	Class() string
	Acquire(ctx context.Context) (source.VM, error)
	Release()
}

// vms is what the provider needs of fleetci's core: source.Capacity, with
// reservations that tests can fake.
type vms interface {
	Reserve(class string) (reservation, error)
	Create(ctx context.Context, class string) (source.VM, error)
	Connect(ctx context.Context, vm string) (proxy.Target, error)
	Delete(ctx context.Context, vm string) error
}

// capacityVMs is vms over source.Capacity.
type capacityVMs struct{ source.Capacity }

func (c capacityVMs) Reserve(class string) (reservation, error) {
	r, err := c.Capacity.Reserve(context.Background(), class)
	if err != nil {
		return nil, err
	}
	return &capacityReservation{c: c.Capacity, r: r}, nil
}

// capacityReservation is a source.Reservation, used once by Acquire or
// Release.
type capacityReservation struct {
	c    source.Capacity
	r    source.Reservation
	once sync.Once
}

func (r *capacityReservation) Class() string { return r.r.Class }

func (r *capacityReservation) Acquire(ctx context.Context) (source.VM, error) {
	vm, err := source.VM{}, errors.New("reservation already used")
	r.once.Do(func() { vm, err = r.c.Acquire(ctx, r.r) })
	return vm, err
}

func (r *capacityReservation) Release() {
	r.once.Do(func() { _ = r.c.Release(context.Background(), r.r) })
}

type provider struct {
	common.ExecutorProvider
	vms vms
	// classOf is the class of a runner's jobs, or "" when the job's
	// FLEETCI_CLASS names it.
	classOf func(*common.RunnerConfig) string
	// classes and defaultClass resolve FLEETCI_CLASS.
	classes      []string
	defaultClass string
}

func newProvider(docker common.ExecutorProvider, vms vms, classOf func(*common.RunnerConfig) string, classes []string, defaultClass string) *provider {
	return &provider{ExecutorProvider: docker, vms: vms, classOf: classOf, classes: classes, defaultClass: defaultClass}
}

// anyRoom reserves room for a VM of the first class that has some: the
// default class, then the others in order.
func (p *provider) anyRoom() (reservation, error) {
	order := append([]string{p.defaultClass}, slices.DeleteFunc(slices.Clone(p.classes), func(c string) bool { return c == p.defaultClass })...)
	for _, class := range order {
		res, err := p.vms.Reserve(class)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, source.ErrNoRoom) {
			return nil, err
		}
	}
	return nil, &common.NoFreeExecutorError{Message: "no class has room for a vm"}
}

// jobClass is the class a job's FLEETCI_CLASS names, the default when it
// is unset.
func (p *provider) jobClass(build *common.Build) (string, error) {
	class := build.GetAllVariables().Value(classVariable)
	if class == "" {
		class = p.defaultClass
	}
	if slices.Contains(p.classes, class) {
		return class, nil
	}
	list := make([]string, len(p.classes))
	for i, c := range p.classes {
		list[i] = c
		if c == p.defaultClass {
			list[i] += " (default)"
		}
	}
	return "", &common.BuildError{
		Inner:         fmt.Errorf("unknown class %q; this runner has %s", class, strings.Join(list, ", ")),
		FailureReason: common.ConfigurationError,
	}
}

func (p *provider) Acquire(config *common.RunnerConfig) (common.ExecutorData, error) {
	class := p.classOf(config)
	if class == "" {
		// fleetci: the class is known once the job is. Room in some class
		// is held until then, so no job is taken while the host is full.
		res, err := p.anyRoom()
		if err != nil {
			return nil, err
		}
		return &acquisitionRef{res: res, byJob: true, connect: p.vms.Connect, dial: connector.Dial}, nil
	}
	res, err := p.vms.Reserve(class)
	if err != nil {
		if errors.Is(err, source.ErrNoRoom) {
			err = &common.NoFreeExecutorError{Message: fmt.Sprintf("reserving a %s vm: %v", class, err)}
		}

		return nil, err
	}

	logrus.WithField("class", class).Trace("Reserved capacity...")

	return &acquisitionRef{res: res, connect: p.vms.Connect, dial: connector.Dial}, nil
}

func (p *provider) Release(_ *common.RunnerConfig, data common.ExecutorData) {
	acqRef, ok := data.(*acquisitionRef)
	if !ok {
		return
	}

	if acqRef.vm != nil {
		_ = p.vms.Delete(context.Background(), acqRef.vm.ID)
		logrus.WithField("vm", acqRef.vm.ID).Trace("Released capacity...")

		acqRef.vm = nil
		return
	}

	acqRef.res.Release()
	logrus.Trace("Unreserved capacity...")
}

func (p *provider) Create() common.Executor {
	e := p.ExecutorProvider.Create()
	if e == nil {
		return nil
	}

	return &executor{Executor: e, p: p}
}

var (
	_ terminal.InteractiveTerminal = (*executor)(nil)
	_ steps.Connector              = (*executor)(nil)
)

type executor struct {
	common.Executor
	p *provider
}

func (e *executor) Prepare(options common.ExecutorPrepareOptions) (err error) {
	options.Build.Log().Infoln("Preparing instance...")

	acqRef, ok := options.Build.ExecutorData.(*acquisitionRef)
	if !ok {
		return errors.New("no acquisition ref data")
	}

	// If we already have an acquisition just retry preparing it
	if acqRef.vm != nil {
		return e.Executor.Prepare(options)
	}

	// fleetci: the class's acquire_timeout bounds the wait.
	// gitlab-runner tries Prepare again after an error that isn't a
	// BuildError, with the same reservation, which is spent by then.
	var vm source.VM
	switch tried := acqRef.tried; {
	case tried:
		vm, err = e.retry(options, acqRef)
	case acqRef.byJob:
		acqRef.tried = true
		vm, err = e.prepareByJob(options, acqRef.res)
	default:
		acqRef.tried = true
		vm, err = acqRef.res.Acquire(options.Context)
	}
	if err != nil {
		return fmt.Errorf("unable to acquire instance: %w", err)
	}

	options.Build.Log().WithField("vm", vm.ID).Trace("Acquired capacity...")

	acqRef.vm = &vm

	return e.Executor.Prepare(options)
}

// prepareByJob takes a VM of the class the job's FLEETCI_CLASS names: in
// the reserved room when it is that class's, else after the room is given
// back, waiting for the job's class.
func (e *executor) prepareByJob(options common.ExecutorPrepareOptions, res reservation) (source.VM, error) {
	class, err := e.p.jobClass(options.Build)
	if err != nil {
		res.Release()
		return source.VM{}, err
	}
	if class == res.Class() {
		return res.Acquire(options.Context)
	}
	res.Release()
	// fleetci: in the job's log, where the wait shows.
	options.BuildLogger.Println("Waiting for a " + class + " VM...")
	return e.p.vms.Create(options.Context, class)
}

// retry waits for room in the job's class, its reservation spent.
func (e *executor) retry(options common.ExecutorPrepareOptions, acqRef *acquisitionRef) (source.VM, error) {
	// The build's runner config is a copy, unknown to classOf; the
	// reservation knows the runner's class.
	class := acqRef.res.Class()
	if acqRef.byJob {
		var err error
		if class, err = e.p.jobClass(options.Build); err != nil {
			return source.VM{}, err
		}
	}
	// fleetci: in the job's log, where the wait shows.
	options.BuildLogger.Println("Waiting for a " + class + " VM...")
	return e.p.vms.Create(options.Context, class)
}

func (e *executor) Connect(ctx context.Context) (func() (io.ReadWriteCloser, error), error) {
	if connector, ok := e.Executor.(steps.Connector); ok {
		return connector.Connect(ctx)
	}

	return nil, common.ErrExecutorStepRunnerConnectNotSupported
}

func (e *executor) TerminalConnect() (terminal.Conn, error) {
	if connector, ok := e.Executor.(terminal.InteractiveTerminal); ok {
		return connector.TerminalConnect()
	}

	return nil, errors.New("executor does not have terminal")
}

var _ executors.Environment = (*acquisitionRef)(nil)

var errRefAcqNotSet = errors.New("ref.vm is not set")

type acquisitionRef struct {
	res reservation
	// byJob: the job's FLEETCI_CLASS names its class, and res is room in
	// whichever class had some.
	byJob bool
	// tried: Prepare has used res, or given it back.
	tried bool
	vm    *source.VM

	// connect returns a VM's connection details.
	connect func(ctx context.Context, vm string) (proxy.Target, error)
	// test hook
	dial connector.DialFn
}

// AcquisitionKey labels the job's containers with its VM.
func (ref *acquisitionRef) AcquisitionKey() string {
	if ref.vm == nil {
		return ""
	}
	return ref.vm.ID
}

func (ref *acquisitionRef) Prepare(
	ctx context.Context,
	logger buildlogger.Logger,
	options common.ExecutorPrepareOptions,
) (executors.Client, error) {
	if ref.vm == nil {
		return nil, errRefAcqNotSet
	}

	dialCtx, cancel := ref.WithContext(ctx)
	defer cancel()

	// fleetci: the VM's connect details, with the external address
	// already chosen when the class's connector_config asks for it.
	target, err := ref.connect(dialCtx, ref.vm.ID)
	if cause := context.Cause(dialCtx); cause != nil {
		return nil, buildErrorFromContextCause(cause)
	}
	if err != nil {
		return nil, fmt.Errorf("getting instance connect info: %w", err)
	}
	info := target.Info

	options.Build.Log().WithFields(logrus.Fields{
		"address":     info.InternalAddr,
		"instance-id": info.ID,
		"vm":          ref.vm.ID,
	}).Info("Dialing instance")

	logger.Println(fmt.Sprintf("Dialing instance %s...", info.ID))
	fleetingDialer, err := dialWithCancelCheck(dialCtx, ref.dial, info, connector.DialOptions{})
	if err != nil {
		return nil, err
	}
	logger.Println(fmt.Sprintf("Instance %s connected", info.ID))

	return &client{client: fleetingDialer, cleanup: nil}, nil
}

func (ref *acquisitionRef) WithContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func dialWithCancelCheck(
	ctx context.Context,
	dial connector.DialFn,
	info fleetingprovider.ConnectInfo,
	options connector.DialOptions,
) (connector.Client, error) {
	client, err := dial(ctx, info, options)
	if cause := context.Cause(ctx); cause != nil {
		if client != nil {
			client.Close()
		}
		return nil, buildErrorFromContextCause(cause)
	}

	return client, err
}

// buildErrorFromContextCause maps a context cancellation cause to the
// appropriate BuildError failure reason. User-initiated cancels and job
// timeouts are distinguished from autoscaler-internal failures so that
// Rails receives an accurate failure attribution.
func buildErrorFromContextCause(cause error) *common.BuildError {
	switch {
	case errors.Is(cause, context.Canceled):
		return &common.BuildError{Inner: cause, FailureReason: common.JobCanceled}
	case errors.Is(cause, context.DeadlineExceeded):
		return &common.BuildError{Inner: cause, FailureReason: common.JobExecutionTimeout}
	default:
		return &common.BuildError{Inner: cause, FailureReason: common.RunnerSystemFailure}
	}
}

type client struct {
	client  connector.Client
	cleanup func() error
}

func (c *client) Dial(n string, addr string) (net.Conn, error) {
	return c.client.Dial(n, addr)
}

func (c *client) DialRun(ctx context.Context, command string) (net.Conn, error) {
	return c.client.DialRun(ctx, command)
}

func (c *client) Run(ctx context.Context, opts executors.RunOptions) error {
	err := c.client.Run(ctx, connector.RunOptions(opts))

	var exitErr *connector.ExitError
	if errors.As(err, &exitErr) {
		return &common.BuildError{
			Inner:    err,
			ExitCode: common.NormalizeExitCode(exitErr.ExitCode()),
		}
	}

	return err
}

func (c *client) Close() error {
	var err error
	if c.cleanup != nil {
		err = c.cleanup()
	}

	if cerr := c.client.Close(); cerr != nil {
		return cerr
	}
	return err
}
