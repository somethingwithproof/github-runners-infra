package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TrustedWorkflowRun verifies GitHub's stored run identity and trigger before
// an explicitly allowlisted public repository can allocate cloud capacity.
func (a *App) TrustedWorkflowRun(ctx context.Context, owner, repo string, runID int64) (bool, error) {
	if runID <= 0 {
		return false, nil
	}
	if !a.ReadWorkflowRuns {
		return false, fmt.Errorf("workflow run read scope is not configured")
	}
	token, err := a.InstallationTokenContext(ctx)
	if err != nil {
		return false, err
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repo), runID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", bearerPrefix+token)
	request.Header.Set("Accept", acceptGitHubJSON)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	response, err := HTTPClient.Do(request)
	if err != nil {
		return false, fmt.Errorf("verify workflow run: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return false, apiStatusError(response, "verifying workflow run")
	}
	var run struct {
		ID         int64  `json:"id"`
		Event      string `json:"event"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository struct {
			ID       int64  `json:"id"`
			Fork     bool   `json:"fork"`
			FullName string `json:"full_name"`
		} `json:"head_repository"`
		PullRequests []struct {
			Number int64 `json:"number"`
			Head   struct {
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		} `json:"pull_requests"`
	}
	if err := decodeJSON(response.Body, &run); err != nil {
		return false, err
	}
	fullName := owner + "/" + repo
	if run.ID != runID || !strings.EqualFold(run.Repository.FullName, fullName) || !strings.EqualFold(run.HeadRepository.FullName, fullName) {
		return false, nil
	}
	if run.Repository.ID <= 0 || run.HeadRepository.ID != run.Repository.ID || run.HeadRepository.Fork {
		return false, nil
	}
	switch run.Event {
	case "pull_request":
		if len(run.PullRequests) == 0 {
			return false, nil
		}
		for _, pr := range run.PullRequests {
			if pr.Number <= 0 || pr.Head.Repo.ID != run.Repository.ID || pr.Base.Repo.ID != run.Repository.ID {
				return false, nil
			}
		}
		return true, nil
	case "push", "schedule", "workflow_dispatch":
		return true, nil
	default:
		return false, nil
	}
}
