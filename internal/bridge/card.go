package bridge

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
)

// BuildCard builds an AgentCard for one launcher at the given endpoint URL.
func BuildCard(ln agent.Launcher, info agent.Info, endpoint string, bearer bool) *a2a.AgentCard {
	caps := ln.Capabilities()

	displayName := info.DisplayName
	if displayName == "" {
		displayName = ln.Name()
	}

	version := info.Version
	if version == "" {
		version = "0.1.0"
	}
	in := caps.InputModes()
	if len(in) == 0 {
		in = []string{"text/plain"}
	}
	out := caps.OutputModes()
	if len(out) == 0 {
		out = []string{"text/plain"}
	}

	card := &a2a.AgentCard{
		Name:        "Harbor/" + displayName,
		Description: "Remote A2A wrapper for the " + displayName + " coding agent CLI",
		Version:     version,
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             endpoint,
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		}},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         caps.Streaming != agent.StreamNone,
			PushNotifications: false,
		},
		DefaultInputModes:  in,
		DefaultOutputModes: out,
		Skills: []a2a.AgentSkill{{
			ID:          ln.Name(),
			Name:        displayName,
			Description: "Run a coding task with " + displayName,
			Tags:        []string{"coding", "cli", ln.Name()},
			Examples:    []string{"fix the failing test in ./pkg", "review this diff"},
		}},
	}

	if bearer {
		name := a2a.SecuritySchemeName("bearer")
		card.SecuritySchemes = a2a.NamedSecuritySchemes{
			name: a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"},
		}
		card.SecurityRequirements = a2a.SecurityRequirementsOptions{
			{name: a2a.SecuritySchemeScopes{}},
		}
	}
	return card
}
