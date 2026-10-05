package digitalocean

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

func TestDeleteRunnerConfirmsAbsenceAfterMissingOwnership(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			gets, deletes := 0, 0
			httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodDelete {
					deletes++
					return jsonResponse(http.StatusNoContent, ""), nil
				}
				gets++
				if gets == 1 || status == http.StatusOK {
					return jsonResponse(http.StatusOK, `{"droplet":{"id":1,"tags":[]}}`), nil
				}
				return jsonResponse(status, `{"id":"not_found","message":"resource unavailable"}`), nil
			})}
			client := &Client{client: godo.NewClient(httpClient), controllerTag: "runner-controller-test"}
			err := client.DeleteRunner(context.Background(), "1", "org/repo:1")
			if gets != 2 || deletes != 0 {
				t.Fatalf("gets=%d deletes=%d; want two reads and no deletion", gets, deletes)
			}
			switch status {
			case http.StatusNotFound:
				if err != nil {
					t.Fatalf("confirmed absent resource: %v", err)
				}
			case http.StatusOK:
				if !errors.Is(err, compute.ErrOwnershipMismatch) {
					t.Fatalf("still-existing unowned resource: %v", err)
				}
			default:
				if err == nil || errors.Is(err, compute.ErrOwnershipMismatch) {
					t.Fatalf("unavailable confirmation must remain retryable: %v", err)
				}
			}
		})
	}
}

func TestRunnerAbsentRequiresAuthoritative404(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusOK, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := &Client{client: godo.NewClient(&http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.Path != "/v2/droplets/123" {
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
				return jsonResponse(status, `{"droplet":{"id":123,"tags":[]},"message":"unavailable"}`), nil
			})})}
			absent, err := client.RunnerAbsent(context.Background(), "123")
			if absent != (status == http.StatusNotFound) || (err != nil) != (status != http.StatusOK && status != http.StatusNotFound) {
				t.Fatalf("status %d: absent=%v err=%v", status, absent, err)
			}
		})
	}
	t.Run("transport failure", func(t *testing.T) {
		client := &Client{client: godo.NewClient(&http.Client{Transport: doRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})})}
		if absent, err := client.RunnerAbsent(context.Background(), "123"); absent || err == nil {
			t.Fatalf("transport failure: absent=%v err=%v", absent, err)
		}
	})
}
