package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"time"

	"github.com/apex/log"
	"github.com/deviceplane/cli/pkg/agent/client"
	"github.com/deviceplane/cli/pkg/agent/info"
	"github.com/deviceplane/cli/pkg/agent/metrics"
	"github.com/deviceplane/cli/pkg/agent/netns"
	"github.com/deviceplane/cli/pkg/agent/server/local"
	"github.com/deviceplane/cli/pkg/agent/server/remote"
	"github.com/deviceplane/cli/pkg/agent/service"
	"github.com/deviceplane/cli/pkg/agent/status"
	"github.com/deviceplane/cli/pkg/agent/supervisor"
	"github.com/deviceplane/cli/pkg/agent/updater"
	"github.com/deviceplane/cli/pkg/agent/validator"
	"github.com/deviceplane/cli/pkg/agent/validator/customcommands"
	"github.com/deviceplane/cli/pkg/agent/validator/image"
	"github.com/deviceplane/cli/pkg/agent/variables"
	"github.com/deviceplane/cli/pkg/agent/variables/fsnotify"
	dpcontext "github.com/deviceplane/cli/pkg/context"
	"github.com/deviceplane/cli/pkg/engine"
	"github.com/deviceplane/cli/pkg/file"
	"github.com/deviceplane/cli/pkg/models"
	"github.com/pkg/errors"
)

const (
	accessKeyFilename = "access-key"
	deviceIDFilename  = "device-id"
	bundleFilename    = "bundle"
)

var (
	errVersionNotSet = errors.New("version not set")
)

type Agent struct {
	client                 *client.Client // TODO: interface
	variables              variables.Interface
	projectID              string
	registrationToken      string
	confDir                string
	stateDir               string
	serverPort             int
	supervisor             *supervisor.Supervisor
	statusGarbageCollector *status.GarbageCollector
	metricsPusher          *metrics.MetricsPusher
	infoReporter           *info.Reporter
	localServer            *local.Server
	remoteServer           *remote.Server
	updater                *updater.Updater
}

func NewAgent(
	client *client.Client, engine engine.Engine,
	projectID, registrationToken, confDir, stateDir, version, binaryPath string, serverPort int,
) (*Agent, error) {
	if version == "" {
		return nil, errVersionNotSet
	}

	if err := os.MkdirAll(confDir, 0700); err != nil {
		return nil, err
	}

	variables := fsnotify.NewVariables(confDir)
	if err := variables.Start(); err != nil {
		return nil, errors.Wrap(err, "start fsnotify variables")
	}

	supervisor := supervisor.NewSupervisor(
		engine,
		variables,
		func(ctx *dpcontext.Context, applicationID, currentReleaseID string) error {
			return client.SetDeviceApplicationStatus(ctx, applicationID, models.SetDeviceApplicationStatusRequest{
				CurrentReleaseID: currentReleaseID,
			})
		},
		client.SetDeviceServiceStatus,
		client.SetDeviceServiceState,
		[]validator.Validator{
			image.NewValidator(variables),
			customcommands.NewValidator(variables),
		},
	)

	netnsManager := netns.NewManager(engine)
	netnsManager.Start()

	serviceMetricsFetcher := metrics.NewServiceMetricsFetcher(
		supervisor,
		netnsManager,
	)

	service := service.NewService(variables, supervisor, engine, confDir, serviceMetricsFetcher)

	return &Agent{
		client:            client,
		variables:         variables,
		projectID:         projectID,
		registrationToken: registrationToken,
		confDir:           confDir,
		stateDir:          stateDir,
		serverPort:        serverPort,
		supervisor:        supervisor,
		statusGarbageCollector: status.NewGarbageCollector(
			client.DeleteDeviceApplicationStatus,
			client.DeleteDeviceServiceStatus,
			client.DeleteDeviceServiceState,
		),
		metricsPusher: metrics.NewMetricsPusher(client, serviceMetricsFetcher),
		infoReporter:  info.NewReporter(client, version),
		localServer:   local.NewServer(service),
		remoteServer:  remote.NewServer(client, service),
		updater:       updater.NewUpdater(projectID, version, binaryPath),
	}, nil
}

func (a *Agent) fileLocation(elem ...string) string {
	return path.Join(
		append(
			[]string{a.stateDir, a.projectID},
			elem...,
		)...,
	)
}

func (a *Agent) writeFile(contents []byte, elem ...string) error {
	if err := os.MkdirAll(a.fileLocation(), 0700); err != nil {
		return err
	}
	if err := file.WriteFileAtomic(a.fileLocation(elem...), contents, 0644); err != nil {
		return err
	}
	return nil
}

func (a *Agent) Initialize() error {
	if _, err := os.Stat(a.fileLocation(accessKeyFilename)); err == nil {
		log.Info("device already registered")
	} else if os.IsNotExist(err) {
		log.Info("registering device")
		if err = a.register(); err != nil {
			return errors.Wrap(err, "failed to register device")
		}
	} else if err != nil {
		return errors.Wrap(err, "failed to check for access key")
	}

	accessKeyBytes, err := ioutil.ReadFile(a.fileLocation(accessKeyFilename))
	if err != nil {
		return errors.Wrap(err, "failed to read access key")
	}

	deviceIDBytes, err := ioutil.ReadFile(a.fileLocation(deviceIDFilename))
	if err != nil {
		return errors.Wrap(err, "failed to read device ID")
	}

	a.client.SetAccessKey(string(accessKeyBytes))
	a.client.SetDeviceID(string(deviceIDBytes))

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.serverPort))
		if err == nil {
			a.localServer.SetListener(listener)
			return nil
		}

		<-ticker.C
	}
}

func (a *Agent) register() error {
	ctx, cancel := dpcontext.New(context.Background(), time.Minute)
	defer cancel()

	registerDeviceResponse, err := a.client.RegisterDevice(ctx, a.registrationToken)
	if err != nil {
		return errors.Wrap(err, "failed to register device")
	}
	if err := a.writeFile([]byte(registerDeviceResponse.DeviceAccessKeyValue), accessKeyFilename); err != nil {
		return errors.Wrap(err, "failed to save access key")
	}
	if err := a.writeFile([]byte(registerDeviceResponse.DeviceID), deviceIDFilename); err != nil {
		return errors.Wrap(err, "failed to save device ID")
	}
	return nil
}

func (a *Agent) Run() {
	go a.runBundleApplier()
	go a.runInfoReporter()
	go a.runRemoteServer()
	go a.runLocalServer()
	select {}
}

func (a *Agent) runBundleApplier() {
	bundle := a.loadSavedBundle()
	if bundle != nil {
		a.supervisor.Set(*bundle, bundle.Applications)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		bundle = a.downloadLatestBundle(bundle)
		if bundle != nil {
			a.supervisor.Set(*bundle, bundle.Applications)
			a.statusGarbageCollector.SetBundle(*bundle)
			a.updater.SetDesiredVersion(bundle.DesiredAgentVersion)
			a.metricsPusher.SetBundle(*bundle)
		}

		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) loadSavedBundle() *models.Bundle {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(a.fileLocation(bundleFilename)); err == nil {
			savedBundleBytes, err := ioutil.ReadFile(a.fileLocation(bundleFilename))
			if err != nil {
				log.WithError(err).Error("read saved bundle")
				goto cont
			}

			var savedBundle models.Bundle
			if err = json.Unmarshal(savedBundleBytes, &savedBundle); err != nil {
				log.WithError(err).Error("discarding invalid saved bundle")
				return nil
			}

			return &savedBundle
		} else if os.IsNotExist(err) {
			return nil
		} else {
			log.WithError(err).Error("check if saved bundle exists")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) downloadLatestBundle(oldBundle *models.Bundle) *models.Bundle {
	ctx, cancel := dpcontext.New(context.Background(), time.Minute)
	defer cancel()

	bundleBytes, err := a.client.GetBundleBytes(ctx)
	if err != nil {
		log.WithError(err).Error("get bundle")
		return nil
	}

	bundle := mergeBundle(oldBundle, bundleBytes)

	bundleBytes, err = json.Marshal(bundle)
	if err != nil {
		log.WithError(err).Error("marshal bundle")
		return nil
	}

	if err = a.writeFile(bundleBytes, bundleFilename); err != nil {
		log.WithError(err).Error("save bundle")
		return nil
	}

	return bundle
}

func mergeBundle(oldBundle *models.Bundle, bundleBytes []byte) *models.Bundle {
	var bundle models.Bundle
	err := json.Unmarshal(bundleBytes, &bundle)
	if err != nil {
		log.WithError(err).Error("unmarshaling full bundle")

		var minimalBundle struct {
			DesiredAgentVersion string `json:"desiredAgentVersion" yaml:"desiredAgentVersion"`
		}
		err := json.Unmarshal(bundleBytes, &minimalBundle)
		if err != nil {
			log.WithError(err).Error("unmarshaling minimal bundle")
			return nil
		}

		if oldBundle != nil {
			bundle = *oldBundle
		}
		bundle.DesiredAgentVersion = minimalBundle.DesiredAgentVersion
	}

	return &bundle
}

func (a *Agent) runInfoReporter() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		if err := a.infoReporter.Report(); err != nil {
			log.WithError(err).Error("report device info")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) runLocalServer() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := a.localServer.Serve(); err != nil {
			log.WithError(err).Error("serve local device API")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) runRemoteServer() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := a.remoteServer.Serve(); err != nil {
			log.WithError(err).Error("serve remote device API")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}
