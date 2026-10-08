// Command register is a test Go plugin: it exports Register, which registers a
// stub adapter for "plugin-cli".
package main

import (
	"context"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
)

type stub struct{}

func (stub) Name() string                     { return "plugin-cli" }
func (stub) Capabilities() agent.Capabilities { return agent.Capabilities{InputText: true} }
func (stub) Probe(context.Context) agent.Info { return agent.Info{Name: "plugin-cli"} }
func (stub) Auth() agent.Authenticator        { return nil }
func (stub) AuthReady(context.Context) bool   { return true }
func (stub) Start(context.Context, *agent.Request) (agent.Session, error) {
	return nil, nil
}

// Register is called by plugins.Load after the plugin is opened.
func Register() {
	agent.RegisterAdapter("plugin-cli", func(config.AgentConfig, agent.AdapterOptions) (agent.Launcher, error) {
		return stub{}, nil
	})
}

func main() {}
