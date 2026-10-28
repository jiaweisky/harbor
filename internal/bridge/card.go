package bridge

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/core"
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
func BuildCard(
	ln agent.Launcher, spec core.AgentSpec, endpoint string, bearer bool,
) *a2a.AgentCard {
	caps := ln.Capabilities()

	version := spec.Card.Version
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
		Name:             spec.Card.Name,
		Description:      spec.Card.Description,
		Version:          version,
		IconURL:          spec.Card.IconURL,
		DocumentationURL: spec.Card.DocumentationURL,
		Provider:         cardProvider(spec.Card.Provider),
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
			Extensions:        cardExtensions(caps, spec.Card.Extensions),
		},
		DefaultInputModes:  in,
		DefaultOutputModes: out,
		Skills:             cardSkills(spec.Card.Skills),
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

// cardSkills maps the operator-configured skills onto card skills.
func cardSkills(skills []core.Skill) []a2a.AgentSkill {
	out := make([]a2a.AgentSkill, 0, len(skills))

	for _, s := range skills {
		out = append(out, a2a.AgentSkill{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Tags:        append([]string{}, s.Tags...), // non-nil so tags serialize as [], not null
			Examples:    s.Examples,
			InputModes:  s.InputModes,
			OutputModes: s.OutputModes,
		})
	}

	return out
}

// cardProvider maps the configured provider onto its card type.
func cardProvider(p *core.Provider) *a2a.AgentProvider {
	if p == nil {
		return nil
	}

	return &a2a.AgentProvider{Org: p.Organization, URL: p.URL}
}

// cardExtensions merges operator-configured extensions with the interaction
// extension. A configured extension with the same URI takes precedence.
func cardExtensions(caps agent.Capabilities, configured []core.Extension) []a2a.AgentExtension {
	out := make([]a2a.AgentExtension, 0, len(configured)+1)
	seen := make(map[string]bool, len(configured)+1)

	for _, e := range configured {
		out = append(out, a2a.AgentExtension{
			URI: e.URI, Description: e.Description, Params: e.Params, Required: e.Required,
		})
		seen[e.URI] = true
	}

	for _, e := range interactionExtensions(caps) {
		if !seen[e.URI] {
			out = append(out, e)
		}
	}

	return out
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
