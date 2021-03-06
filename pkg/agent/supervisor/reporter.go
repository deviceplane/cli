package supervisor

import (
	"context"
	"sync"
	"time"

	"github.com/apex/log"
	dpcontext "github.com/deviceplane/cli/pkg/context"
	"github.com/deviceplane/cli/pkg/models"
)

type Reporter struct {
	applicationID           string
	reportApplicationStatus func(ctx *dpcontext.Context, applicationID, currentRelease string) error
	reportServiceStatus     func(ctx *dpcontext.Context, applicationID, service string, req models.SetDeviceServiceStatusRequest) error
	reportServiceState      func(ctx *dpcontext.Context, applicationID, service string, req models.SetDeviceServiceStateRequest) error

	desiredApplicationRelease      string
	desiredApplicationServiceNames map[string]struct{}
	reportedApplicationRelease     string
	applicationStatusReporterDone  chan struct{}

	serviceStatuses           map[string]models.SetDeviceServiceStatusRequest
	reportedServiceStatuses   map[string]models.SetDeviceServiceStatusRequest
	serviceStatusReporterDone chan struct{}

	serviceStates            map[string]models.SetDeviceServiceStateRequest
	reportedServiceStates    map[string]models.SetDeviceServiceStateRequest
	serviceStateReporterDone chan struct{}

	once   sync.Once
	lock   sync.RWMutex
	ctx    context.Context
	cancel func()
}

func NewReporter(
	applicationID string,
	reportApplicationStatus func(ctx *dpcontext.Context, applicationID, currentRelease string) error,
	reportServiceStatus func(ctx *dpcontext.Context, applicationID, service string, req models.SetDeviceServiceStatusRequest) error,
	reportServiceState func(ctx *dpcontext.Context, applicationID, service string, req models.SetDeviceServiceStateRequest) error,
) *Reporter {
	ctx, cancel := context.WithCancel(context.Background())
	return &Reporter{
		applicationID:           applicationID,
		reportApplicationStatus: reportApplicationStatus,
		reportServiceStatus:     reportServiceStatus,
		reportServiceState:      reportServiceState,

		desiredApplicationServiceNames: make(map[string]struct{}),
		applicationStatusReporterDone:  make(chan struct{}),

		serviceStatuses:           make(map[string]models.SetDeviceServiceStatusRequest),
		reportedServiceStatuses:   make(map[string]models.SetDeviceServiceStatusRequest),
		serviceStatusReporterDone: make(chan struct{}),

		serviceStates:            make(map[string]models.SetDeviceServiceStateRequest),
		reportedServiceStates:    make(map[string]models.SetDeviceServiceStateRequest),
		serviceStateReporterDone: make(chan struct{}),

		ctx:    ctx,
		cancel: cancel,
	}
}

func (r *Reporter) SetDesiredApplication(release string, applicationConfig map[string]models.Service) {
	serviceNames := make(map[string]struct{})
	for serviceName := range applicationConfig {
		serviceNames[serviceName] = struct{}{}
	}

	r.lock.Lock()
	r.desiredApplicationRelease = release
	r.desiredApplicationServiceNames = serviceNames
	r.lock.Unlock()

	r.once.Do(func() {
		go r.applicationStatusReporter()
		go r.serviceStatusReporter()
		go r.serviceStateReporter()
	})
}

func (r *Reporter) SetServiceStatus(serviceName string, status models.SetDeviceServiceStatusRequest) {
	r.lock.Lock()
	r.serviceStatuses[serviceName] = status
	r.lock.Unlock()
}

func (r *Reporter) SetServiceState(serviceName string, state models.SetDeviceServiceStateRequest) {
	r.lock.Lock()
	r.serviceStates[serviceName] = state
	r.lock.Unlock()
}

func (r *Reporter) Stop() {
	r.cancel()
	// TODO: don't do this if SetDesiredApplication was never called
	<-r.applicationStatusReporterDone
	<-r.serviceStatusReporterDone
	<-r.serviceStateReporterDone
}

func (r *Reporter) applicationStatusReporter() {
	ticker := time.NewTicker(defaultTickerFrequency)
	defer ticker.Stop()

	for {
		var ctx *dpcontext.Context
		var cancel func()

		r.lock.RLock()
		releaseToReport := r.desiredApplicationRelease
		if releaseToReport == r.reportedApplicationRelease {
			r.lock.RUnlock()
			goto cont
		}
		for serviceName := range r.desiredApplicationServiceNames {
			status, ok := r.serviceStatuses[serviceName]
			if !ok || status.CurrentReleaseID != releaseToReport {
				r.lock.RUnlock()
				goto cont
			}
		}
		r.lock.RUnlock()

		ctx, cancel = dpcontext.New(r.ctx, time.Minute)

		if err := r.reportApplicationStatus(ctx, r.applicationID, releaseToReport); err != nil {
			log.WithError(err).Error("report application status")
			goto cont
		}

		cancel()

		r.reportedApplicationRelease = releaseToReport

	cont:
		select {
		case <-r.ctx.Done():
			r.applicationStatusReporterDone <- struct{}{}
			return
		case <-ticker.C:
			continue
		}
	}
}

func (r *Reporter) serviceStatusReporter() {
	ticker := time.NewTicker(defaultTickerFrequency)
	defer ticker.Stop()

	for {
		var ctx *dpcontext.Context
		var cancel func()

		r.lock.RLock()
		diff := make(map[string]models.SetDeviceServiceStatusRequest)
		copy := make(map[string]models.SetDeviceServiceStatusRequest)
		for service, status := range r.serviceStatuses {
			reportedStatus, ok := r.reportedServiceStatuses[service]
			if !ok || reportedStatus.CurrentReleaseID != status.CurrentReleaseID {
				diff[service] = status
			}
			copy[service] = status
		}
		r.lock.RUnlock()

		for serviceName, status := range diff {
			ctx, cancel = dpcontext.New(r.ctx, time.Minute)

			if err := r.reportServiceStatus(
				ctx,
				r.applicationID,
				serviceName,
				status,
			); err != nil {
				log.WithError(err).Error("report service status")
				goto cont
			}

			cancel()
		}

		r.reportedServiceStatuses = copy

	cont:
		select {
		case <-r.ctx.Done():
			r.serviceStatusReporterDone <- struct{}{}
			return
		case <-ticker.C:
			continue
		}
	}
}

func (r *Reporter) serviceStateReporter() {
	ticker := time.NewTicker(defaultTickerFrequency)
	defer ticker.Stop()

	for {
		var ctx *dpcontext.Context
		var cancel func()

		r.lock.RLock()
		diff := make(map[string]models.SetDeviceServiceStateRequest)
		copy := make(map[string]models.SetDeviceServiceStateRequest)
		for service, state := range r.serviceStates {
			reportedState, ok := r.reportedServiceStates[service]
			if !ok ||
				(reportedState.State != state.State ||
					reportedState.ErrorMessage != state.ErrorMessage) {
				diff[service] = state
			}
			copy[service] = state
		}
		r.lock.RUnlock()

		for serviceName, state := range diff {
			ctx, cancel = dpcontext.New(r.ctx, time.Minute)

			if err := r.reportServiceState(
				ctx,
				r.applicationID,
				serviceName,
				state,
			); err != nil {
				log.WithError(err).Error("report service state")
				goto cont
			}

			cancel()
		}

		r.reportedServiceStates = copy

	cont:
		select {
		case <-r.ctx.Done():
			r.serviceStateReporterDone <- struct{}{}
			return
		case <-ticker.C:
			continue
		}
	}
}
