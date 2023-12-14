package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/medik8s/fence-agents-remediation/api/v1alpha1"
	"github.com/medik8s/fence-agents-remediation/pkg/utils"
)

type routine struct {
	cancel context.CancelFunc
}

type Executer struct {
	client.Client
	log          logr.Logger
	routines     map[types.UID]*routine
	routinesLock sync.Mutex
	runner       runnerFunc
}

// runnerFunc is a function that runs the command and returns the stdout, stderr and error
// it is configurable in Executer for testing purposes
type runnerFunc func(ctx context.Context, command []string) (string, string, error)

// NewExecuter builds the Executer
func NewExecuter(client client.Client) (*Executer, error) {
	logger := ctrl.Log.WithName("executer")

	return &Executer{
		Client:   client,
		log:      logger,
		routines: make(map[types.UID]*routine),
		runner:   run,
	}, nil
}

// NewExecuter builds an Executer with configurable runnerFunc for testing
func NewFakeExecuter(client client.Client, fn runnerFunc) (*Executer, error) {
	logger := ctrl.Log.WithName("fakeExecuter")

	return &Executer{
		Client:   client,
		log:      logger,
		routines: make(map[types.UID]*routine),
		runner:   fn,
	}, nil
}

// AsyncExecute runs the command in a goroutine mapped to the UID
func (e *Executer) AsyncExecute(ctx context.Context, uid types.UID, command []string, retryCount int, retryDuration, timeout time.Duration) {
	e.routinesLock.Lock()
	defer e.routinesLock.Unlock()
	if _, exist := e.routines[uid]; exist {
		return
	}

	// create a context for the fence agent command that the controller can cancel
	cancellableCtx, cancel := context.WithCancel(ctx)
	routine := routine{
		cancel: cancel,
	}
	e.routines[uid] = &routine

	go e.fenceAgentRoutine(cancellableCtx, uid, command, retryCount, retryDuration, timeout)
}

func (e *Executer) fenceAgentRoutine(ctx context.Context, uid types.UID, command []string, retryCount int, retryDuration, timeout time.Duration) {
	// run the command and update the status
	retryErr, cmdErr := e.runWithRetry(ctx, uid, command, retryCount, retryDuration, timeout)
	if retryErr != nil {
		switch {
		case errors.Is(retryErr, context.Canceled):
			e.log.Info("fence agent context canceled. Nothing to do")
			return
		case wait.Interrupted(retryErr):
			e.log.Info("fence agent context timed out")
		default:
			e.log.Error(retryErr, "fence agent retry error")
		}
	}

	retryErr = e.updateStatusWithRetry(ctx, uid, cmdErr)
	if retryErr != nil {
		switch {
		case wait.Interrupted(retryErr):
			e.log.Info("status context timed out")
		default:
			e.log.Error(retryErr, "status retry error")
		}
	}
}

func (e *Executer) runWithRetry(ctx context.Context, uid types.UID, command []string, retryCount int, retryDuration, timeout time.Duration) (retryErr, faErr error) {
	// Run the command with an exponantial backoff retry to handle the following cases:
	// - the command fails: the command is retried until the retryCount is reached
	// - the command times out: the command is retried until the retryCount is reached
	// - the FA context times out: the command is cancelled and the status is updated
	// - the FA context is cancelled: the command is cancelled and the status is not updated
	// - the command succeeds: the command is not retried and the status is updated

	// Linear backoff
	backoff := wait.Backoff{
		Steps:    retryCount,
		Duration: retryDuration,
		Factor:   1.0,
	}

	e.log.Info("fence agent start", "uid", uid, "command", command, "retryCount", retryCount, "retryDuration", retryDuration, "timeout", timeout)

	var stdout, stderr string
	retryErr = wait.ExponentialBackoffWithContext(ctx,
		backoff,
		func(ctx context.Context) (bool, error) {
			ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			stdout, stderr, faErr = e.runner(ctxWithTimeout, command)
			if faErr == nil {
				e.log.Info("command completed", "uid", uid, "response", stdout, "errMessage", stderr, "err", faErr)
				return true, nil
			}

			if wait.Interrupted(faErr) {
				e.log.Error(faErr, "fence agent timeout", "uid", uid)
				return false, faErr
			}

			e.log.Info("command failed", "uid", uid, "response", stdout, "errMessage", stderr, "err", faErr)
			return false, nil
		})

	e.log.Info("fence agent done", "uid", uid, "command", command, "stdout", stdout, "stderr", stderr, "err", faErr)
	return retryErr, faErr
}

func (e *Executer) updateStatusWithRetry(ctx context.Context, uid types.UID, fenceAgentErr error) error {
	// Update FAR status with an exponantial backoff retry to handle only the updateStatus error cases where:
	// - FAR cannot be found, but it does exist
	// - the status update fails for conflicts

	e.log.Info("updating status", "FAR uid", uid)

	err := wait.ExponentialBackoffWithContext(ctx,
		retry.DefaultBackoff,
		func(ctx context.Context) (bool, error) {
			far, err := e.getFenceAgentsRemediationByUID(ctx, uid)
			if err != nil {
				if apiErrors.IsNotFound(err) {
					e.log.Info("could not find FAR by UID", "FAR uid", uid)
					return false, err
				}

				if wait.Interrupted(err) {
					e.log.Info("could not update status", "FAR uid", uid, "reason", err)
					return false, err
				}

				e.log.Error(err, "could not update status", "FAR uid", uid)
				return false, err
			}

			if err := e.updateStatus(ctx, far, fenceAgentErr); err != nil {
				if wait.Interrupted(err) {
					e.log.Info("context cancelled while updating the status", "FAR uid", uid)
					return true, err
				}
				if apiErrors.IsConflict(err) {
					e.log.Error(err, "conflict while updating the status", "FAR uid", uid)
					return true, nil
				}
				e.log.Error(err, "failed to update status", "FAR uid", uid)
				return false, err
			}

			e.log.Info("status updated", "FAR uid", uid)
			return true, nil
		})
	return err
}

// Exists checks if there is already a running Fence Agent command mapped to the UID
func (e *Executer) Exists(uid types.UID) bool {
	e.routinesLock.Lock()
	defer e.routinesLock.Unlock()
	_, exist := e.routines[uid]
	return exist
}

func (e *Executer) Remove(uid types.UID) {
	e.routinesLock.Lock()
	defer e.routinesLock.Unlock()
	if routine, exist := e.routines[uid]; exist {
		e.log.Info("cancelling fence agent routine", "uid", uid)
		routine.cancel()
		delete(e.routines, uid)
	}
}

// run runs the command in the container and updates the status of the FAR instance maching the UID
func run(ctx context.Context, command []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)

	var outBuilder, errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder

	err = cmd.Run()

	return outBuilder.String(), errBuilder.String(), err
}

func (e *Executer) getFenceAgentsRemediationByUID(ctx context.Context, uid types.UID) (*v1alpha1.FenceAgentsRemediation, error) {
	farList := &v1alpha1.FenceAgentsRemediationList{}
	if err := e.List(ctx, farList, &client.ListOptions{}); err != nil || len(farList.Items) == 0 {
		e.log.Error(err, "failed to list FAR", "FAR uid", uid)
		return nil, err
	}

	for _, far := range farList.Items {
		if far.UID == uid {
			return &far, nil
		}
	}

	err := fmt.Errorf("could not find any FAR matching the UID")
	e.log.Error(err, "failed to get far", "uid", uid)

	return nil, err
}

func (e *Executer) updateStatus(ctx context.Context, far *v1alpha1.FenceAgentsRemediation, err error) error {
	var reason utils.ConditionsChangeReason

	if err == nil {
		reason = utils.FenceAgentSucceeded
	} else if wait.Interrupted(err) {
		reason = utils.FenceAgentTimedOut
	} else {
		reason = utils.FenceAgentFailed
	}

	err = utils.UpdateConditions(reason, far, e.log)
	if err != nil {
		e.log.Error(err, "failed to update conditions", "FAR uid", far.UID)
		return err
	}
	return e.Status().Update(ctx, far)
}
