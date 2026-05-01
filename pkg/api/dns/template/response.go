package template

import "github.com/sitehostnz/gosh/pkg/models"

type (
	// DomainTemplate is a single DNS domain template entry.
	DomainTemplate struct {
		ClientID     string `json:"client_id"`
		TemplateID   string `json:"template_id"`
		TemplateName string `json:"template_name"`
		DomainCount  string `json:"domain_count"`
	}

	// ListResponse represents a request to list DNS domain templates.
	ListResponse struct {
		Return []DomainTemplate `json:"return"`
		models.APIResponse
	}
)
