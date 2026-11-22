package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestGatewayApprovalAPIListsAndResolvesPendingRequests(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision bee.ApprovalDecision
	}{
		{name: "approve", decision: bee.ApprovalApproved},
		{name: "deny", decision: bee.ApprovalDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := &bee.Application{Executor: &bee.Executor{}}
			if err := app.EnableApprovalBroker(); err != nil {
				t.Fatal(err)
			}
			service := &Service{app: app}
			decision := make(chan bee.ApprovalDecision, 1)
			errCh := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() {
				value, err := app.Executor.Approval.Approve(ctx, bee.ApprovalRequest{ID: "approval-" + test.name, ToolName: "exec", Summary: "run a command", Args: json.RawMessage(`{"token":"must-not-leak","command":"go test ./..."}`)})
				decision <- value
				errCh <- err
			}()

			var pending bee.PendingApproval
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				response := httptest.NewRecorder()
				service.handleApprovals(response, httptest.NewRequest(http.MethodGet, "/api/approvals", nil))
				var payload struct {
					Approvals []bee.PendingApproval `json:"approvals"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Approvals) == 1 {
					pending = payload.Approvals[0]
					break
				}
				time.Sleep(time.Millisecond)
			}
			if pending.ID == "" || pending.Request.ToolName != "exec" || pending.Status != bee.ApprovalStatusPending {
				t.Fatalf("pending approval=%#v", pending)
			}
			if strings.Contains(string(pending.Request.Args), "must-not-leak") || !strings.Contains(string(pending.Request.Args), "[REDACTED") {
				t.Fatalf("pending approval arguments were not redacted: %s", pending.Request.Args)
			}

			response := httptest.NewRecorder()
			body := `{"decision":"` + string(test.decision) + `"}`
			request := httptest.NewRequest(http.MethodPost, "/api/approvals/"+pending.ID, strings.NewReader(body))
			service.handleApprovalDecision(response, request, pending.ID)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := <-decision; got != test.decision {
				t.Fatalf("decision=%s", got)
			}
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
		})
	}
}
