package provision

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"text/template"

	"github.com/charmbracelet/log"
)

//go:embed init.sh
var initScript string

type ProvisionResult struct {
	ServerIP        net.IP
	ServerWgIp      net.IP
	ServerPublicKey string
}

type ProvisionArguments struct {
	ClientPublicKey string
	ClientWgIp      net.IP
	ServerWgIp      net.IP
	WgPort          uint16
	Type            string
	Region          string
}

type DeProvisionArguments struct {
	Region string
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"county"`
	City      string  `json:"city"`
	Key       string  `json:"key"`
}

type Provisioner interface {
	Provision(ctx context.Context, id string, args ProvisionArguments) (ProvisionResult, error)
	DeProvision(ctx context.Context, id string, args DeProvisionArguments) error
	Locations(ctx context.Context) ([]Location, error)
}

type RunInitScriptOutput struct {
	ServerWgPublicKey string `json:"ServerWgPublicKey"`
}

func (a ProvisionArguments) RunInitScript(ctx context.Context, runShellFunc func(string) (string, error)) (*RunInitScriptOutput, error) {
	var outputSeparator = "93b5409013b3265be85973fc8434a05e8f2e31bd9dae057501e704d40a8ac39f"
	tpl, err := template.New("initScript").Parse(initScript)
	if err != nil {
		return nil, err
	}

	var script strings.Builder
	params := map[string]string{}
	params["OutputSeparator"] = outputSeparator
	params["WgPort"] = strconv.Itoa(int(a.WgPort))
	params["ClientWgIp"] = a.ClientWgIp.String()
	params["ClientPublicKey"] = a.ClientPublicKey
	params["ServerWgIp"] = a.ServerWgIp.String()
	params["Region"] = a.Region
	params["Type"] = a.Type

	err = tpl.Execute(&script, params)
	if err != nil {
		return nil, err
	}

	stdout, err := runShellFunc(script.String())
	if err != nil {
		log.Error("failed to run init script", "stdout", stdout, "err", err)
		return nil, err
	}

	parts := strings.SplitAfter(stdout, outputSeparator)
	if len(parts) != 2 {
		log.Error("init script did not return expected output", "stdout", stdout)
		return nil, errors.New("init script did not return expected output")
	}

	outputParams := RunInitScriptOutput{}
	err = json.Unmarshal([]byte(parts[1]), &outputParams)

	return &outputParams, err
}
