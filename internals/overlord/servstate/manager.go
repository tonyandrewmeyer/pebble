package servstate

import (
	"fmt"
	"io"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/canonical/pebble/internals/logger"
	"github.com/canonical/pebble/internals/metrics"
	"github.com/canonical/pebble/internals/overlord/restart"
	"github.com/canonical/pebble/internals/overlord/state"
	"github.com/canonical/pebble/internals/plan"
	"github.com/canonical/pebble/internals/servicelog"
	"github.com/canonical/pebble/internals/workloads"
)

// timeNow can be faked during testing.
var timeNow = time.Now

type ServiceManager struct {
	state  *state.State
	runner *state.TaskRunner

	planLock sync.Mutex
	plan     *plan.Plan

	servicesLock sync.Mutex
	services     map[string]*serviceData

	serviceOutput io.Writer
	restarter     Restarter

	randLock sync.Mutex
	rand     *rand.Rand

	logMgr LogManager
}

type LogManager interface {
	ServiceStarted(service *plan.Service, logs *servicelog.RingBuffer)
}

type Restarter interface {
	HandleRestart(t restart.RestartType)
}

func NewManager(s *state.State, runner *state.TaskRunner, serviceOutput io.Writer, restarter Restarter, logMgr LogManager) (*ServiceManager, error) {
	manager := &ServiceManager{
		state:         s,
		runner:        runner,
		services:      make(map[string]*serviceData),
		serviceOutput: serviceOutput,
		restarter:     restarter,
		rand:          rand.New(rand.NewSource(time.Now().UnixNano())),
		logMgr:        logMgr,
	}

	// Old-style handlers (still needed for backward compatibility)
	runner.AddHandler("start", manager.doStart, nil)
	runner.AddHandler("stop", manager.doStop, nil)

	// New change-based handlers
	runner.AddHandler(startServiceKind, manager.doStartService, nil)
	runner.AddHandler(monitorServiceKind, manager.doMonitorService, nil)
	runner.AddHandler(restartServiceKind, manager.doRestartService, nil)

	// Register change status change callback
	s.Lock()
	s.AddChangeStatusChangedHandler(manager.changeStatusChanged)
	s.Unlock()

	return manager, nil
}

// PlanChanged informs the service manager that the plan has been updated.
func (m *ServiceManager) PlanChanged(plan *plan.Plan) {
	m.planLock.Lock()
	defer m.planLock.Unlock()
	m.plan = plan
}

// getPlan returns the current plan pointer in a concurrency-safe way. The
// service manager must not mutate the result.
func (m *ServiceManager) getPlan() *plan.Plan {
	m.planLock.Lock()
	defer m.planLock.Unlock()
	// This should never be possible, but lets make the requirements clear to
	// catch misuse during development. Managers using the plan must receive
	// a PlanChanged update before the plan is used. The first update will be
	// received during stateengine StartUp, after the plan manager loads the
	// plan layers from storage.
	if m.plan == nil {
		panic("service manager with invalid plan state")
	}
	return m.plan
}

// Ensure implements StateManager.Ensure.
func (m *ServiceManager) Ensure() error {
	return nil
}

type ServiceInfo struct {
	Name         string
	Startup      ServiceStartup
	Current      ServiceStatus
	CurrentSince time.Time
}

type ServiceStartup string

const (
	StartupEnabled  = "enabled"
	StartupDisabled = "disabled"
)

type ServiceStatus string

const (
	StatusActive   ServiceStatus = "active"
	StatusBackoff  ServiceStatus = "backoff"
	StatusError    ServiceStatus = "error"
	StatusInactive ServiceStatus = "inactive"
)

// Services returns the list of configured services and their status, sorted
// by service name. Filter by the specified service names if provided.
func (m *ServiceManager) Services(names []string) ([]*ServiceInfo, error) {
	currentPlan := m.getPlan()
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	requested := make(map[string]bool, len(names))
	for _, name := range names {
		requested[name] = true
	}

	var services []*ServiceInfo
	matchNames := len(names) > 0
	for name, config := range currentPlan.Services {
		if matchNames && !requested[name] {
			continue
		}
		info := &ServiceInfo{
			Name:    name,
			Startup: StartupDisabled,
			Current: StatusInactive,
		}
		if config.Startup == plan.StartupEnabled {
			info.Startup = StartupEnabled
		}
		if s, ok := m.services[name]; ok {
			info.Current = stateToStatus(s.state)
			info.CurrentSince = s.currentSince
		}
		services = append(services, info)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})
	return services, nil
}

// StopTimeout returns the worst case duration that will have to be waited for
// to have all services in this manager stopped.
func (m *ServiceManager) StopTimeout() time.Duration {
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	maxDuration := killDelayDefault
	for _, service := range m.services {
		if service == nil {
			continue
		}
		switch service.state {
		case stateStarting, stateRunning, stateTerminating:
			if service.killDelay() > maxDuration {
				maxDuration = service.killDelay()
			}
		}
	}

	// We add a little extra time here to allow for signals to be sent and
	// processed.
	return maxDuration + failDelay + 100*time.Millisecond
}

func stateToStatus(state serviceState) ServiceStatus {
	switch state {
	case stateStarting, stateRunning:
		return StatusActive
	case stateTerminating, stateKilling, stateStopped:
		return StatusInactive
	case stateBackoff:
		return StatusBackoff
	default: // stateInitial (should never happen) and stateExited
		return StatusError
	}
}

// DefaultServiceNames returns the name of the services set to start
// by default.
func (m *ServiceManager) DefaultServiceNames() ([]string, error) {
	currentPlan := m.getPlan()
	var names []string
	for name, service := range currentPlan.Services {
		if service.Startup == plan.StartupEnabled {
			names = append(names, name)
		}
	}

	lanes, err := currentPlan.StartOrder(names)
	if err != nil {
		return nil, err
	}

	var result []string
	for _, lane := range lanes {
		result = append(result, lane...)
	}
	return result, err
}

// StartOrder returns the provided services, together with any required
// dependencies, in the proper order, put in lanes, for starting them all up.
func (m *ServiceManager) StartOrder(services []string) ([][]string, error) {
	currentPlan := m.getPlan()
	return currentPlan.StartOrder(services)
}

// StopOrder returns the provided services, together with any dependants,
// in the proper order, put in lanes, for stopping them all.
func (m *ServiceManager) StopOrder(services []string) ([][]string, error) {
	currentPlan := m.getPlan()
	return currentPlan.StopOrder(services)
}

// ServiceLogs returns iterators to the provided services. If last is negative,
// return tail iterators; if last is zero or positive, return head iterators
// going back last elements. Each iterator must be closed via the Close method.
func (m *ServiceManager) ServiceLogs(services []string, last int) (map[string]servicelog.Iterator, error) {
	requested := make(map[string]bool, len(services))
	for _, name := range services {
		requested[name] = true
	}

	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	iterators := make(map[string]servicelog.Iterator)
	for name, service := range m.services {
		if !requested[name] {
			continue
		}
		if service == nil || service.logs == nil {
			continue
		}
		if last >= 0 {
			iterators[name] = service.logs.HeadIterator(last)
		} else {
			iterators[name] = service.logs.TailIterator()
		}
	}

	return iterators, nil
}

// Replan returns a list of services in lanes to stop and services to start
// because their plans had changed between when they started and this call.
func (m *ServiceManager) Replan() ([][]string, [][]string, error) {
	currentPlan := m.getPlan()
	ws, _ := currentPlan.Sections[workloads.WorkloadsField].(*workloads.WorkloadsSection)
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	needsRestart := make(map[string]bool)
	var stop []string
	for name, s := range m.services {
		if config, ok := currentPlan.Services[name]; ok {
			// Don't restart the service unless the service configuration or its
			// workload definition (if any) have changed
			var workload *workloads.Workload
			if ws != nil {
				workload = ws.Entries[s.config.Workload]
			}
			if config.Equal(s.config) && (workload == nil || workload.Equal(s.workload)) {
				continue
			}
			// Update service config and workload from plan
			s.config = config.Copy()
			if workload != nil {
				s.workload = workload
			}
		}
		needsRestart[name] = true
		stop = append(stop, name)
	}

	var start []string
	for name, config := range currentPlan.Services {
		if needsRestart[name] || config.Startup == plan.StartupEnabled {
			start = append(start, name)
		}
	}

	stopLanes, err := currentPlan.StopOrder(stop)
	if err != nil {
		return nil, nil, err
	}
	for i, name := range stop {
		if !needsRestart[name] {
			stop = append(stop[:i], stop[i+1:]...)
		}
	}

	startLanes, err := currentPlan.StartOrder(start)
	if err != nil {
		return nil, nil, err
	}

	return stopLanes, startLanes, nil
}

func (m *ServiceManager) SendSignal(services []string, signal string) error {
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	var errors []string
	for _, name := range services {
		s := m.services[name]
		if s == nil {
			errors = append(errors, fmt.Sprintf("cannot send signal to %q: service is not running", name))
			continue
		}
		err := s.sendSignal(signal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("cannot send signal to %q: %v", name, err))
			continue
		}
	}
	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}

// CheckFailed response to a health check failure. If the given check name is
// in the on-check-failure map for a service, tell the service to perform the
// configured action (for example, "restart").
func (m *ServiceManager) CheckFailed(name string) {
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	for _, service := range m.services {
		for checkName, action := range service.config.OnCheckFailure {
			if checkName == name {
				service.checkFailed(action)
			}
		}
	}
}

// WriteMetrics collects and writes metrics for all services to the provided writer.
func (m *ServiceManager) WriteMetrics(writer metrics.Writer) error {
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		service := m.services[name]
		err := service.writeMetric(writer)
		if err != nil {
			return err
		}
	}
	return nil
}

// Prune cleans up the in-memory serviceData:
//   - It removes the serviceData if a service is inactive and its currentSince is older than pruneWait.
//   - If the number of inactive serviceData entries is still more than maxServiceData, remove inactive services'
//     serviceData even if they are not older than pruneWait. Inactive services are sorted by currentSince,
//     and remove the older ones first.
func (m *ServiceManager) Prune(pruneWait time.Duration, maxServiceData int) {
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()

	now := timeNow()
	pruneLimit := now.Add(-pruneWait)
	for name, s := range m.services {
		if stateToStatus(s.state) == StatusInactive && s.currentSince.Before(pruneLimit) {
			delete(m.services, name)
		}
	}

	if len(m.services) > maxServiceData {
		var inactive []*serviceData
		for _, s := range m.services {
			if stateToStatus(s.state) == StatusInactive {
				inactive = append(inactive, s)
			}
		}
		slices.SortFunc(inactive, func(a *serviceData, b *serviceData) int {
			return a.currentSince.Compare(b.currentSince)
		})
		excess := max(len(inactive)-maxServiceData, 0)
		for i := range excess {
			delete(m.services, inactive[i].config.Name)
		}
	}
}

// servicesToStop is used during service manager shutdown to cleanly terminate
// all running services. Running services include both services in the
// stateRunning and stateBackoff, since a service in backoff state can start
// running once the timeout expires, which creates a race on service manager
// exit. If it starts just before, it would continue to run after the service
// manager is terminated. If it starts just after (before the main process
// exits), it would generate a runtime error as the reaper would already be dead.
// This function returns a slice of service names to stop, in dependency order,
// put in lanes.
func servicesToStop(m *ServiceManager) ([][]string, error) {
	currentPlan := m.getPlan()
	// Get all service names in plan.
	services := make([]string, 0, len(currentPlan.Services))
	for name := range currentPlan.Services {
		services = append(services, name)
	}

	// Order according to dependency order.
	stop, err := currentPlan.StopOrder(services)
	if err != nil {
		return nil, err
	}

	// Filter down to only those that are starting, running or in backoff
	m.servicesLock.Lock()
	defer m.servicesLock.Unlock()
	var result [][]string
	for _, services := range stop {
		var notStopped []string
		for _, name := range services {
			s := m.services[name]
			if s != nil && (s.state == stateStarting || s.state == stateRunning || s.state == stateBackoff) {
				notStopped = append(notStopped, name)
			}
		}
		if len(notStopped) > 0 {
			result = append(result, notStopped)
		}
	}
	return result, nil
}

// changeStatusChanged handles state transitions for run-service and restart-service changes.
// This coordinates the lifecycle: run-service → (exit) → restart-service → (success) → run-service
// Note: This callback is invoked while the state lock is held, so we spawn goroutines to avoid deadlock.
func (m *ServiceManager) changeStatusChanged(change *state.Change, old, new state.Status) {
	switch change.Kind() {
	case runServiceKind:
		if new == state.DoneStatus || new == state.ErrorStatus {
			// Service exited, handle based on configured action
			// Spawn goroutine to avoid deadlock (callback is called with state lock held)
			go m.handleRunServiceComplete(change)
		}

	case restartServiceKind:
		if new == state.DoneStatus {
			// Restart successful, create new run-service change
			// Spawn goroutine to avoid deadlock (callback is called with state lock held)
			go m.handleRestartServiceComplete(change)
		}
	}
}

// handleRunServiceComplete is called when a run-service change completes.
// It checks the exit action and creates a restart-service change if needed.
func (m *ServiceManager) handleRunServiceComplete(change *state.Change) {
	// Get change data with state lock
	m.state.Lock()
	var details runServiceDetails
	err := change.Get("run-service-details", &details)
	m.state.Unlock()

	if err != nil {
		logger.Noticef("Cannot get run-service details: %v", err)
		return
	}

	serviceName := details.ServiceName

	m.servicesLock.Lock()
	service := m.services[serviceName]
	m.servicesLock.Unlock()

	if service == nil {
		logger.Noticef("Service %q not found when handling run-service completion", serviceName)
		return
	}

	// Get exit information. First try the monitor-service task, then fall back
	// to the service's last exit info (for when service exits before monitor starts).
	var exitCode int
	var action plan.ServiceAction
	gotExitInfo := false

	m.state.Lock()
	tasks := change.Tasks()
	for _, t := range tasks {
		if t.Kind() == monitorServiceKind {
			var code int
			var actionStr string
			err1 := t.Get("exit-code", &code)
			err2 := t.Get("exit-action", &actionStr)
			if err1 == nil && err2 == nil {
				exitCode = code
				action = plan.ServiceAction(actionStr)
				gotExitInfo = true
			}
			break
		}
	}
	m.state.Unlock()

	if !gotExitInfo {
		// Fall back to the service's last exit info
		m.servicesLock.Lock()
		exitCode = service.lastExitCode
		action = service.lastExitAction
		m.servicesLock.Unlock()
		logger.Debugf("Using service's last exit info for %q: code=%d, action=%s",
			serviceName, exitCode, action)
	}

	// Based on action, decide what to do
	switch action {
	case plan.ActionIgnore:
		// Do nothing, service stays exited
		logger.Noticef("Service %q exited with code %d, action is ignore", serviceName, exitCode)

	case plan.ActionRestart:
		// Create restart-service change
		logger.Noticef("Service %q exited with code %d, creating restart change", serviceName, exitCode)

		m.state.Lock()
		// Get current backoff number from service for continuity and
		// transition service state to backoff for backward compatibility
		m.servicesLock.Lock()
		initialAttempts := service.backoffNum
		// Transition service to backoff state so status queries show StatusBackoff
		service.backoffNum++
		service.backoffTime = calculateNextBackoff(service.config, service.backoffTime)
		service.transition(stateBackoff)
		m.servicesLock.Unlock()

		config := m.state.Cached(runServiceConfigKey{change.ID()}).(*plan.Service)
		createRestartServiceChange(m.state, serviceName, config, exitCode, initialAttempts)
		m.state.EnsureBefore(0)
		m.state.Unlock()

		m.runner.Ensure()

	case plan.ActionShutdown, plan.ActionSuccessShutdown, plan.ActionFailureShutdown:
		// Trigger system shutdown
		logger.Noticef("Service %q exited, triggering shutdown (action: %s)", serviceName, action)

		var restartType restart.RestartType
		switch action {
		case plan.ActionShutdown:
			if exitCode != 0 {
				restartType = restart.RestartServiceFailure
			} else {
				restartType = restart.RestartDaemon
			}
		case plan.ActionSuccessShutdown:
			restartType = restart.RestartDaemon
		case plan.ActionFailureShutdown:
			restartType = restart.RestartServiceFailure
		}
		m.restarter.HandleRestart(restartType)
	}
}

// handleRestartServiceComplete is called when a restart-service change completes successfully.
// It creates a new run-service change to continue monitoring the service.
func (m *ServiceManager) handleRestartServiceComplete(change *state.Change) {
	m.state.Lock()

	tasks := change.Tasks()
	if len(tasks) == 0 {
		m.state.Unlock()
		logger.Noticef("restart-service change %s has no tasks", change.ID())
		return
	}

	var details restartServiceDetails
	err := tasks[0].Get("restart-details", &details)
	if err != nil {
		m.state.Unlock()
		logger.Noticef("Cannot get restart details: %v", err)
		return
	}

	serviceName := details.ServiceName

	// Get the service config from the restart-service change cache
	config := m.state.Cached(restartServiceConfigKey{change.ID()}).(*plan.Service)

	logger.Noticef("Service %q restarted successfully, creating new run-service change", serviceName)

	// Use monitor-only change since the service is already running
	changeID := createMonitorOnlyRunServiceChange(m.state, serviceName, config)
	m.state.EnsureBefore(0)
	m.state.Unlock()

	// Update the service's run change ID
	m.servicesLock.Lock()
	if service := m.services[serviceName]; service != nil {
		service.runChangeID = changeID
	}
	m.servicesLock.Unlock()

	m.runner.Ensure()
}
