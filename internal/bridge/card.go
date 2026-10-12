package bridge

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/blob"
	"github.com/jiaweisky/harbor/internal/config"
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

// StorageExtensionURI is declared when the agent can exchange files out of band:
// its params carry the ingest contract a sender uses to hand it a file.
const StorageExtensionURI = "https://github.com/jiaweisky/harbor/extensions/storage/v1"

// StorageContract is the machine-readable ingest contract advertised on the
// agent card. It is assembled from the storage configuration and the store.
type StorageContract struct {
	Schemes        []string         `json:"schemes"`
	MediaTypes     []string         `json:"mediaTypes"`
	MaxInlineBytes int64            `json:"maxInlineBytes"`
	MaxBytes       int64            `json:"maxBytes"`
	Modes          []map[string]any `json:"modes"`
}

// NewStorageContract assembles the advertised contract, or nil when storage is
// disabled.
func NewStorageContract(sc config.StorageConfig, store blob.Store) *StorageContract {
	if store == nil {
		return nil
	}
	c := &StorageContract{
		Schemes:        storageSchemes(sc.Backend),
		MediaTypes:     sc.MediaTypes,
		MaxInlineBytes: sc.InlineMaxBytes,
		MaxBytes:       sc.Download.MaxBytes,
	}
	if t := store.Target(); t.URL != "" {
		upload := map[string]any{"base": t.URL, "method": t.Method, "auth": "none"}
		if t.KeyRule != "" {
			upload["key"] = t.KeyRule
		}
		if ref := t.Ref(); ref != t.URL {
			upload["reference"] = ref
		}
		c.Modes = append(c.Modes, map[string]any{"mode": "reference", "upload": upload})
	}
	if supportsBroker(sc) {
		c.Modes = append(c.Modes, map[string]any{"mode": "broker"})
	}
	return c
}

// storageSchemes maps a backend to the reference schemes it accepts.
func storageSchemes(backend string) []string {
	switch backend {
	case "http":
		return []string{"https"}
	case "s3":
		return []string{"s3"}
	case "cos":
		return []string{"cos"}
	case "local":
		return []string{"file"}
	}
	return nil
}

// supportsBroker reports whether the backend can issue a per-file presigned
// upload target.
func supportsBroker(sc config.StorageConfig) bool {
	switch sc.Backend {
	case "s3", "cos":
		return true
	case "http":
		return sc.HTTP.PresignSecret != "" || sc.HTTP.PresignURL != ""
	}
	return false
}

// BuildCard builds an AgentCard for one launcher at the given endpoint URL. An
// optional storage contract advertises the storage extension and the
// file-exchange skill.
func BuildCard(
	ln agent.Launcher, info agent.Info, endpoint string, bearer bool, contracts ...*StorageContract,
) *a2a.AgentCard {
	var contract *StorageContract
	if len(contracts) > 0 {
		contract = contracts[0]
	}
	caps := ln.Capabilities()

	displayName := info.DisplayName
	if displayName == "" {
		displayName = ln.Name()
	}

	version := info.Version
	if version == "" {
		version = "0.1.0"
	}
	description := info.Description
	if description == "" {
		description = "Remote A2A wrapper for the " + displayName + " coding agent CLI"
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
		Description: description,
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
			Extensions:        cardExtensions(caps, contract),
		},
		DefaultInputModes:  in,
		DefaultOutputModes: out,
		Skills:             cardSkills(info.Skills, contract),
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

// cardExtensions lists the extensions the agent advertises: interaction when the
// CLI can pause for input, and storage when file exchange is configured.
func cardExtensions(caps agent.Capabilities, contract *StorageContract) []a2a.AgentExtension {
	exts := interactionExtensions(caps)
	if contract != nil {
		exts = append(exts, a2a.AgentExtension{
			URI:         StorageExtensionURI,
			Description: "Large-file exchange over a shared storage backend",
			Params:      map[string]any{"ingest": contract},
		})
	}
	return exts
}

// cardSkills maps the operator-configured skills onto card skills, appending an
// informational file-exchange skill when storage is configured.
func cardSkills(skills []agent.Skill, contract *StorageContract) []a2a.AgentSkill {
	out := make([]a2a.AgentSkill, 0, len(skills)+1)
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
	if contract != nil {
		out = append(out, fileExchangeSkill(contract))
	}
	return out
}

// fileExchangeSkill is the human-readable companion to the storage extension.
func fileExchangeSkill(contract *StorageContract) a2a.AgentSkill {
	return a2a.AgentSkill{
		ID:          "file-exchange",
		Name:        "File exchange",
		Description: "Exchanges large files through a shared storage backend; the storage extension carries the ingest contract.",
		Tags:        []string{"files", "storage"},
		InputModes:  contract.MediaTypes,
		OutputModes: contract.MediaTypes,
	}
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
