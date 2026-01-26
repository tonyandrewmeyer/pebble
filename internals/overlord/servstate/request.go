package servstate

import (
	"fmt"
	"time"

	"github.com/canonical/pebble/internals/overlord/state"
	"github.com/canonical/pebble/internals/plan"
)

// ServiceRequest holds the details required to perform service tasks.
type ServiceRequest struct {
	Name string
}

// Stop creates and returns a task set for stopping the given services.
func Stop(s *state.State, lanes [][]string) (*state.TaskSet, error) {
	var tasks []*state.Task
	for _, services := range lanes {
		lane := s.NewLane()
		for i, name := range services {
			task := s.NewTask("stop", fmt.Sprintf("Stop service %q", name))
			req := ServiceRequest{
				Name: name,
			}
			task.Set("service-request", &req)
			task.JoinLane(lane)
			// Wait for the previous task in the same lane.
			if i > 0 {
				task.WaitFor(tasks[len(tasks)-1])
			}
			tasks = append(tasks, task)
		}
	}
	return state.NewTaskSet(tasks...), nil
}

// StopRunning creates and returns a task set for stopping all running
// services. It returns a nil *TaskSet if there are no services to stop.
func StopRunning(s *state.State, m *ServiceManager) (*state.TaskSet, error) {
	lanes, err := servicesToStop(m)
	if err != nil {
		return nil, err
	}
	if len(lanes) == 0 {
		return nil, nil
	}

	// One change to stop them all.
	s.Lock()
	defer s.Unlock()
	taskSet, err := Stop(s, lanes)
	if err != nil {
		return nil, err
	}
	return taskSet, nil
}

// Change and task kind constants for new service lifecycle management
const (
	runServiceKind     = "run-service"
	startServiceKind   = "start-service"
	monitorServiceKind = "monitor-service"
	restartServiceKind = "restart-service"

	serviceNoPruneAttr = "service-no-prune"
)

// Task data structures

// StartServiceDetails holds data for start-service task
type StartServiceDetails struct {
	ServiceName string `json:"service-name"`
}

// monitorServiceDetails holds data for monitor-service task
type monitorServiceDetails struct {
	ServiceName string    `json:"service-name"`
	StartTime   time.Time `json:"start-time"`
}

// restartServiceDetails holds data for restart-service task
type restartServiceDetails struct {
	ServiceName     string `json:"service-name"`
	InitialExitCode int    `json:"initial-exit-code"`
	Attempts        int    `json:"attempts"`
}

// runServiceDetails holds data for run-service change
type runServiceDetails struct {
	ServiceName string `json:"service-name"`
}

// Exit information passed from serviceData to monitor task
type exitInfo struct {
	Code   int
	Action string
}

// Cache key types for storing service config in state
type runServiceConfigKey struct {
	changeID string
}

type restartServiceConfigKey struct {
	changeID string
}

// createRunServiceChange creates a run-service change with a monitor task.
// This is called after the service has been started (either by doStartService or
// doRestartService) to provide lifecycle visibility - the change stays in "Doing"
// status while the service runs and moves to "Done" or "Error" when it exits.
func createRunServiceChange(st *state.State, serviceName string, config *plan.Service) (changeID string) {
	summary := fmt.Sprintf("Run service %q", serviceName)

	// Create monitor-service task (service is already running)
	monitorTask := st.NewTask(monitorServiceKind, fmt.Sprintf("Monitor service %q", serviceName))
	monitorTask.Set("monitor-details", &monitorServiceDetails{
		ServiceName: serviceName,
		StartTime:   time.Now(),
	})

	// Create the run-service change
	change := st.NewChangeWithNoticeData(runServiceKind, summary, map[string]string{
		"service-name": serviceName,
	})
	change.Set(serviceNoPruneAttr, true) // Don't prune long-running changes
	change.Set("run-service-details", &runServiceDetails{
		ServiceName: serviceName,
	})
	change.AddTask(monitorTask)

	// Cache the service config for use by handleRunServiceComplete
	st.Cache(runServiceConfigKey{change.ID()}, config)

	return change.ID()
}

// createRestartServiceChange creates a change to handle service restart with exponential backoff.
func createRestartServiceChange(st *state.State, serviceName string, config *plan.Service, exitCode int, initialAttempts int) (changeID string) {
	summary := fmt.Sprintf("Restart service %q", serviceName)

	task := st.NewTask(restartServiceKind, summary)
	task.Set("restart-details", &restartServiceDetails{
		ServiceName:     serviceName,
		InitialExitCode: exitCode,
		Attempts:        initialAttempts,
	})

	change := st.NewChangeWithNoticeData(restartServiceKind, task.Summary(), map[string]string{
		"service-name": serviceName,
	})
	change.Set(serviceNoPruneAttr, true)
	change.AddTask(task)

	// Cache the service config
	st.Cache(restartServiceConfigKey{change.ID()}, config)

	return change.ID()
}
