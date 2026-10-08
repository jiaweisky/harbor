package bridge

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
)

// RESTMount is the HTTP+JSON (A2A REST) binding path segment under an agent's
// base path; RESTPath is that segment with a trailing slash, i.e. the REST base
// URL suffix of an agent's endpoint.
const (
	RESTMount = "rest"
	RESTPath  = RESTMount + "/"
)

// InteractionExtensionURI is declared when a CLI can pause a task for input:
// the input-required message then carries a DataPart describing a permission,
// question, or plan-approval request.
const InteractionExtensionURI = "https://github.com/jiaweisky/harbor/extensions/interaction/v1"

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
		SupportedInterfaces: []*a2a.AgentInterface{
			{
				URL:             endpoint,
				ProtocolBinding: a2a.TransportProtocolJSONRPC,
				ProtocolVersion: a2a.Version,
			},
			{
				URL:             endpoint + RESTPath,
				ProtocolBinding: a2a.TransportProtocolHTTPJSON,
				ProtocolVersion: a2a.Version,
			},
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         caps.Streaming != agent.StreamNone,
			PushNotifications: true,
			Extensions:        interactionExtensions(caps),
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

// interactionExtensions declares the interaction extension when the CLI can
// pause for a permission, a proactive question, or a plan approval. Clients
// that ignore it fall back to answering the human-readable text part.
func interactionExtensions(caps agent.Capabilities) []a2a.AgentExtension {
	if caps.Permission != agent.PermInteractive && !caps.AskQuestion && !caps.PlanApproval {
		return nil
	}
	return []a2a.AgentExtension{{
		URI:         InteractionExtensionURI,
		Description: "Structured permission/question/plan-approval requests over input-required",
	}}
}
