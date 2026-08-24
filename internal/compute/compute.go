// Package compute defines the cloud-neutral runner lifecycle contract.
package compute

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"text/template"
)

// ErrOwnershipMismatch marks a provider resource that the configured
// controller cannot safely mutate.
var ErrOwnershipMismatch = errors.New("provider resource ownership mismatch")

// ErrDuplicateInstances marks multiple controller-owned resources for one job.
// Callers must reconcile all of them rather than retrying creation.
var ErrDuplicateInstances = errors.New("duplicate provider instances")

// ErrCreateOutcomeUnknown marks a provider create that may have succeeded even
// though no resource identity was returned. Retrying would risk duplication.
var ErrCreateOutcomeUnknown = errors.New("provider create outcome unknown")

var controllerIDRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidControllerID applies the provider-safe controller ownership identifier
// contract shared by all cloud adapters.
func ValidControllerID(value string) bool {
	return controllerIDRegex.MatchString(value)
}

// RunnerParams contains the non-secret values rendered into runner user data.
type RunnerParams struct {
	JobKey              string
	ProvisionEpoch      int
	RunnerName          string
	RunnerJITConfig     string
	RunnerVersion       string
	RunnerSHA256        string
	ChefInstallerSHA256 string
}

// RunnerInstance is a provider-neutral resource identity.
type RunnerInstance struct {
	ID   string
	Name string
}

// RenderCloudInit renders the shared, provider-neutral cloud-init template.
func RenderCloudInit(tmpl *template.Template, params RunnerParams) (string, error) {
	if tmpl == nil {
		return "", fmt.Errorf("cloud-init template is not configured")
	}
	var userData bytes.Buffer
	if err := tmpl.Execute(&userData, params); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return userData.String(), nil
}
