package cli

import (
	"os/exec"
	"strings"

	"github.com/go-logr/logr"

	ctrl "sigs.k8s.io/controller-runtime"
)

type Executer interface {
	Execute(command []string) (string, string, error)
}

type executer struct {
	log logr.Logger
}

var _ Executer = executer{}

// NewExecuter builds the executer
func NewExecuter() Executer {
	logger := ctrl.Log.WithName("executer")

	return &executer{
		log: logger,
	}
}

// Execute run the command in the Pod
func (e executer) Execute(command []string) (string, string, error) {
	e.log.Info("Executing command", "command", command)
	var stdoutBldr, stderrBldr strings.Builder

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout = &stdoutBldr
	cmd.Stderr = &stderrBldr
	err := cmd.Run()
	e.log.Info("Done with command", "command", command, "stdout", stdoutBldr.String(), "stderr", stderrBldr.String(), "err", err)
	return stdoutBldr.String(), stderrBldr.String(), err
}
