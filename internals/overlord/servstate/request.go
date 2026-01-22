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

// Start creates and returns a task set for starting the given services.
// After successfully starting each service, a run-service change is created
// to provide visibility into the service lifecycle.
func Start(s *state.State, lanes [][]string) (*state.TaskSet, error) {
	var tasks []*state.Task
	for _, services := range lanes {
		lane := s.NewLane()
		for i, name := range services {
			task := s.NewTask("start", fmt.Sprintf("Start service %q", name))
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

// startServiceDetails holds data for start-service task
type startServiceDetails struct {
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

// createMonitorOnlyRunServiceChange creates a run-service change with just a monitor task.
// This is used when the service has already been started via the old "start" task flow,
// and we just need to monitor it for lifecycle visibility.
func createMonitorOnlyRunServiceChange(st *state.State, serviceName string, config *plan.Service) (changeID string) {
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

// createRunServiceChange creates a long-running change to represent a service being run.
// The change contains start-service and monitor-service tasks.
// The service config will be cached by the start-service handler.
func createRunServiceChange(st *state.State, serviceName string, config *plan.Service) (changeID string) {
	summary := fmt.Sprintf("Run service %q", serviceName)

	// Create start-service task
	startTask := st.NewTask(startServiceKind, fmt.Sprintf("Start service %q", serviceName))
	startTask.Set("start-details", &startServiceDetails{
		ServiceName: serviceName,
	})

	// Create monitor-service task (depends on start completing)
	monitorTask := st.NewTask(monitorServiceKind, fmt.Sprintf("Monitor service %q", serviceName))
	monitorTask.Set("monitor-details", &monitorServiceDetails{
		ServiceName: serviceName,
		StartTime:   time.Now(),
	})
	monitorTask.WaitFor(startTask)

	// Create the run-service change
	change := st.NewChangeWithNoticeData(runServiceKind, summary, map[string]string{
		"service-name": serviceName,
	})
	change.Set(serviceNoPruneAttr, true) // Don't prune long-running changes
	change.Set("run-service-details", &runServiceDetails{
		ServiceName: serviceName,
	})
	change.AddTask(startTask)
	change.AddTask(monitorTask)

	// Cache the service config for use by handleRunServiceComplete
	st.Cache(runServiceConfigKey{change.ID()}, config)

	return change.ID()
}

// StartServices creates run-service changes for the given services. Each service
// gets its own long-running change that tracks its lifecycle. The services are
// started in dependency order (lanes), with services in the same lane started
// concurrently and later lanes waiting for earlier lanes to complete.
// This function is intended for starting services with startup: enabled.
func StartServices(st *state.State, p *plan.Plan, lanes [][]string) (changeIDs []string, err error) {
	var prevLaneChanges []*state.Change
	for _, services := range lanes {
		var currentLaneChanges []*state.Change
		for _, name := range services {
			config, ok := p.Services[name]
			if !ok {
				return nil, fmt.Errorf("cannot find service %q in plan", name)
			}

			changeID := createRunServiceChange(st, name, config)
			changeIDs = append(changeIDs, changeID)

			change := st.Change(changeID)
			currentLaneChanges = append(currentLaneChanges, change)

			// Make tasks in this change wait for all tasks in the previous lane
			if len(prevLaneChanges) > 0 {
				for _, task := range change.Tasks() {
					for _, prevChange := range prevLaneChanges {
						for _, prevTask := range prevChange.Tasks() {
							task.WaitFor(prevTask)
						}
					}
				}
			}
		}
		prevLaneChanges = currentLaneChanges
	}
	return changeIDs, nil
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
