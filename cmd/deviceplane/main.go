package main

import (
	"os"

	"github.com/deviceplane/cli/cmd/deviceplane/cliutils"
	"github.com/deviceplane/cli/cmd/deviceplane/configure"
	"github.com/deviceplane/cli/cmd/deviceplane/device"
	"github.com/deviceplane/cli/cmd/deviceplane/global"
	"github.com/deviceplane/cli/cmd/deviceplane/project"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	version = "dev"
)

var (
	app = kingpin.New("deviceplane", "The Deviceplane CLI.").UsageTemplate(cliutils.CustomTemplate).Version(version)

	config = global.Config{
		App:             app,
		ParsedCorrectly: app.Flag("internal-parsing-validator", "").Hidden().Default("true").Bool(),

		Flags: global.ConfigFlags{
			APIEndpoint: app.Flag("url", "API Endpoint.").Hidden().Default("https://cloud.deviceplane.com:443/api").URL(),
			AccessKey:   app.Flag("access-key", "Access key used for authentication. (env: DEVICEPLANE_ACCESS_KEY)").Envar("DEVICEPLANE_ACCESS_KEY").String(),
			Project:     app.Flag("project", "Project name. (env: DEVICEPLANE_PROJECT)").Envar("DEVICEPLANE_PROJECT").String(),
			ConfigFile:  app.Flag("config", "Config file to use.").Default("~/.deviceplane/config").String(),
		},

		APIClient: nil,
	}
)

func main() {
	configure.Initialize(&config)
	project.Initialize(&config)
	device.Initialize(&config)

	app.PreAction(cliutils.InitializeAPIClient(&config))
	preSSH, _ := cliutils.GetSSHArgs(os.Args[1:])
	kingpin.MustParse(app.Parse(preSSH))
}
