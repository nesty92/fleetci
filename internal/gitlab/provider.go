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

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"
	fleetingprovider "gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/session/terminal"
	"gitlab.com/gitlab-org/gitlab-runner/steps"

	"github.com/nesty92/fleetci/internal/fleet"
)

// executorName is the executor fleetci's runners name.
const executorName = "fleetci"

// VMs is what the provider needs of fleet.Backend.
type VMs interface {
	Reserve(class string) (*fleet.Reservation, error)
	Delete(ctx context.Context, id string) error
}

type provider struct {
	common.ExecutorProvider
	vms VMs
	// classOf is the class of a runner's jobs.
	classOf func(*common.RunnerConfig) string
}

func newProvider(docker common.ExecutorProvider, vms VMs, classOf func(*common.RunnerConfig) string) *provider {
	return &provider{ExecutorProvider: docker, vms: vms, classOf: classOf}
}

func (p *provider) Acquire(config *common.RunnerConfig) (common.ExecutorData, error) {
	class := p.classOf(config)
	res, err := p.vms.Reserve(class)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRoom) {
			err = &common.NoFreeExecutorError{Message: fmt.Sprintf("reserving a %s vm: %v", class, err)}
		}

		return nil, err
	}

	logrus.WithField("class", class).Trace("Reserved capacity...")

	return &acquisitionRef{res: res, dial: connector.Dial}, nil
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

	return &executor{Executor: e}
}

var (
	_ terminal.InteractiveTerminal = (*executor)(nil)
	_ steps.Connector              = (*executor)(nil)
)

type executor struct {
	common.Executor
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

	// fleetci: the class's acquire_timeout bounds the wait, inside the
	// reservation.
	vm, err := acqRef.res.Acquire(options.Context)
	if err != nil {
		return fmt.Errorf("unable to acquire instance: %w", err)
	}

	options.Build.Log().WithField("vm", vm.ID).Trace("Acquired capacity...")

	acqRef.vm = &vm

	return e.Executor.Prepare(options)
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
	res *fleet.Reservation
	vm  *fleet.VM

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
	target, err := ref.vm.Connect(dialCtx)
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
