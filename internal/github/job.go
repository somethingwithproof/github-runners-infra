package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// JobStatus describes current demand, rather than a delayed queued webhook.
type JobStatus string

const (
	JobQueued     JobStatus = "queued"
	JobInProgress JobStatus = "in_progress"
	JobCompleted  JobStatus = "completed"
)

// WorkflowJobStatus checks the exact repository job using Actions read scope.
// Missing jobs and API failures are errors, never evidence to create a runner.
func (a *App) WorkflowJobStatus(ctx context.Context, owner, repo string, jobID int64) (JobStatus, error) {
	if jobID <= 0 {
		return "", fmt.Errorf("workflow job ID must be positive")
	}
	if !a.ReadWorkflowRuns {
		return "", fmt.Errorf("workflow job read scope is not configured")
	}
	token, err := a.InstallationTokenContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get job status installation token: %w", err)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/jobs/%d", url.PathEscape(owner), url.PathEscape(repo), jobID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", bearerPrefix+token)
	request.Header.Set("Accept", acceptGitHubJSON)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	response, err := HTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("get workflow job status: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", apiStatusError(response, "getting workflow job status")
	}
	var job struct {
		ID     int64     `json:"id"`
		Status JobStatus `json:"status"`
	}
	if err := decodeJSON(response.Body, &job); err != nil {
		return "", fmt.Errorf("decode workflow job status: %w", err)
	}
	if job.ID != jobID {
		return "", fmt.Errorf("GitHub returned job ID %d while querying %d", job.ID, jobID)
	}
	switch job.Status {
	case JobQueued, JobInProgress, JobCompleted:
		return job.Status, nil
	default:
		return "", fmt.Errorf("GitHub returned an unsupported workflow job status")
	}
}
