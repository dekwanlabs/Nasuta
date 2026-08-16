package app

import (
	"database/sql"
	"fmt"

	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/internal/approval"
	"github.com/dekwanlabs/nasuta/internal/transport/incidenthttp"
	"github.com/dekwanlabs/nasuta/internal/writeaction"
	"github.com/dekwanlabs/nasuta/log"
)

// configureIncidents enables incident and approval workflows when the
// platform has a MySQL database available.
func (p *Platform) configureIncidents(evidence incident.EvidenceProvider) error {
	if p.db == nil {
		log.Warnf("[server] incident and approval disabled (MySQL unavailable)")
		return nil
	}
	return p.configureIncidentsWithDB(p.db, evidence)
}

// configureIncidentsWithDB creates incident services and write actions for the
// supplied database, then enables write actions on the existing QA service.
func (p *Platform) configureIncidentsWithDB(db *sql.DB, evidence incident.EvidenceProvider) error {
	if p.incident.manager != nil {
		return fmt.Errorf("incident workflows are already configured")
	}
	cfg := incident.Config{
		WebBaseURL:          p.cfg.WebBaseURL,
		NotifyFeishuWebhook: p.cfg.NotifyFeishuWebhook,
		NotifyWecomWebhook:  p.cfg.NotifyWecomWebhook,
		NotifyHTTPWebhook:   p.cfg.NotifyHTTPWebhook,
		FixDefaultAssignee:  p.cfg.FixDefaultAssignee,
		FixBranchPrefix:     p.cfg.FixBranchPrefix,
		LLMBaseURL:          p.settings.LLMBaseURL,
		LLMAPIKey:           p.settings.LLMAPIKey,
		LLMModel:            p.settings.LLMModel,
		LLMProvider:         p.settings.LLMProvider,
		LLMMaxTokens:        p.settings.LLMMaxTokens,
		VCSURL:              p.settings.VCSURL,
		VCSToken:            p.settings.VCSToken,
	}
	manager, err := incident.NewManager(
		cfg, db, p.cfg.WorkspaceRoot, evidence, p.tools,
	)
	if err != nil {
		return fmt.Errorf("configure incident manager: %w", err)
	}
	actions, err := approval.NewService(db, manager)
	if err != nil {
		return fmt.Errorf("configure approval service: %w", err)
	}
	if err := writeaction.RegisterBuiltins(p.registry, actions); err != nil {
		return fmt.Errorf("configure write actions: %w", err)
	}
	p.incident.manager = manager
	p.incident.api = incidenthttp.New(manager, actions, p.cfg.AlertWebhookSecret)
	p.setQARuntimeWriteAvailable(true)
	log.Infof("[server] incident and approval workflows enabled")
	return nil
}
