// SPDX-License-Identifier: MIT
package tools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mudler/LocalAI/addons/gmail/internal/gmailapi"
	"github.com/mudler/LocalAI/addons/gmail/internal/tools"
	"github.com/mudler/LocalAI/addons/internal/googleapi"
	"github.com/mudler/LocalAI/pkg/addons/contract"
	"github.com/mudler/LocalAI/pkg/addons/sdk/protocol"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Calendar and Tasks tools", func() {
	It("lists only the primary calendar and the selected Tasks list through their APIs", func() {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case "/calendar/v3/calendars/primary/events":
				_, _ = w.Write([]byte(`{"items":[{"id":"event-1","summary":"Planning","start":{"dateTime":"2026-04-04T10:00:00Z"},"end":{"dateTime":"2026-04-04T11:00:00Z"}}]}`))
			case "/tasks/v1/users/@me/lists":
				_, _ = w.Write([]byte(`{"items":[{"id":"list-1","title":"Work"}]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		DeferCleanup(srv.Close)
		rt := tools.New(gmailapi.New())
		rt.CalendarAPI = googleapi.New(srv.URL+"/calendar/v3", "gmail-test")
		rt.TasksAPI = googleapi.New(srv.URL, "gmail-test")
		scopes := []string{tools.ScopeCalendarRead, tools.ScopeTasksRead}

		read := func(name string, args map[string]any) map[string]any {
			args["label"] = "Work"
			result, err := rt.Read(context.Background(), envFor(token, "Work", name, args, scopes...))
			Expect(err).NotTo(HaveOccurred())
			var wrapped reply
			Expect(json.Unmarshal(result.Payload, &wrapped)).To(Succeed())
			return wrapped.Data
		}
		cal := read("calendar-events-list", map[string]any{"from": "2026-04-04T00:00:00Z", "to": "2026-04-05T00:00:00Z"})
		Expect(cal["items"]).To(HaveLen(1))
		Expect(paths[0]).To(Equal("/calendar/v3/calendars/primary/events"))
		lists := read("task-lists-list", map[string]any{})
		Expect(lists["items"]).To(HaveLen(1))
		Expect(paths[1]).To(Equal("/tasks/v1/users/@me/lists"))
	})

	It("requires read scopes before contacting Google", func() {
		srv := httptest.NewServer(http.NotFoundHandler())
		DeferCleanup(srv.Close)
		rt := tools.New(gmailapi.New())
		rt.CalendarAPI = googleapi.New(srv.URL+"/calendar/v3", "gmail-test")
		_, err := rt.Read(context.Background(), envFor(token, "Work", "calendar-events-list", map[string]any{"label": "Work"}))
		Expect(err).To(MatchError(protocol.ErrBadRequest))
		Expect(err.Error()).To(ContainSubstring("calendar.events.readonly"))
	})

	It("previews event creation and applies it with invitation notices disabled", func() {
		var receivedPath, sentTitle, sendUpdates string
		methods := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods++
			receivedPath = r.URL.Path
			sendUpdates = r.URL.Query().Get("sendUpdates")
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			sentTitle, _ = body["summary"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"created","summary":"Plan","start":{"dateTime":"2026-04-04T10:00:00Z"},"end":{"dateTime":"2026-04-04T11:00:00Z"}}`))
		}))
		DeferCleanup(srv.Close)
		rt := tools.New(gmailapi.New())
		rt.CalendarAPI = googleapi.New(srv.URL+"/calendar/v3", "gmail-test")
		args := map[string]any{"label": "Work", "title": "Plan", "start": "2026-04-04T10:00:00Z"}
		e := envFor(token, "Work", "calendar-event-create", args, tools.ScopeCalendarRead, tools.ScopeCalendarWrite)
		e.Request.Action = "write"
		e.Request.OperationID = "calendar-create-1"
		preview, err := rt.Preview(context.Background(), e)
		Expect(err).NotTo(HaveOccurred())
		Expect(methods).To(Equal(0))
		receipt, err := rt.Apply(context.Background(), e, preview.Revision)
		Expect(err).NotTo(HaveOccurred())
		Expect(receipt.State).To(Equal(contract.ReceiptCommitted))
		Expect(receivedPath).To(Equal("/calendar/v3/calendars/primary/events"))
		Expect(sentTitle).To(Equal("Plan"))
		Expect(sendUpdates).To(Equal("none"))
	})

	It("creates a task in the selected or default list only after approval", func() {
		methods := 0
		var gotPath, gotTitle string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods++
			gotPath = r.URL.Path
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			gotTitle, _ = body["title"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"task-1","title":"Prepare report","status":"needsAction"}`))
		}))
		DeferCleanup(srv.Close)
		rt := tools.New(gmailapi.New())
		rt.TasksAPI = googleapi.New(srv.URL, "gmail-test")
		args := map[string]any{"label": "Work", "title": "Prepare report"}
		e := envFor(token, "Work", "task-create", args, tools.ScopeTasksRead, tools.ScopeTasksWrite)
		e.Request.Action = "write"
		e.Request.OperationID = "task-create-1"
		preview, err := rt.Preview(context.Background(), e)
		Expect(err).NotTo(HaveOccurred())
		Expect(methods).To(Equal(0))
		receipt, err := rt.Apply(context.Background(), e, preview.Revision)
		Expect(err).NotTo(HaveOccurred())
		Expect(receipt.State).To(Equal(contract.ReceiptCommitted))
		Expect(gotPath).To(Equal("/tasks/v1/lists/@default/tasks"))
		Expect(gotTitle).To(Equal("Prepare report"))
	})

	It("rejects a stale event approval without applying the change", func() {
		patches := 0
		gets := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				gets++
				title := "Old"
				updated := "2026-04-01T00:00:00Z"
				if gets > 1 {
					title = "Changed"
					updated = "2026-04-02T00:00:00Z"
				}
				_, _ = w.Write([]byte(`{"id":"event-1","summary":"` + title + `","updated":"` + updated + `","start":{"dateTime":"2026-04-04T10:00:00Z"},"end":{"dateTime":"2026-04-04T11:00:00Z"}}`))
			case http.MethodPatch:
				patches++
				_, _ = w.Write([]byte(`{}`))
			default:
				http.NotFound(w, r)
			}
		}))
		DeferCleanup(srv.Close)
		rt := tools.New(gmailapi.New())
		rt.CalendarAPI = googleapi.New(srv.URL+"/calendar/v3", "gmail-test")
		args := map[string]any{"label": "Work", "id": "event-1", "title": "New title"}
		e := envFor(token, "Work", "calendar-event-update", args, tools.ScopeCalendarRead, tools.ScopeCalendarWrite)
		e.Request.Action = "write"
		e.Request.OperationID = "calendar-update-stale"
		preview, err := rt.Preview(context.Background(), e)
		Expect(err).NotTo(HaveOccurred())
		receipt, err := rt.Apply(context.Background(), e, preview.Revision)
		Expect(err).NotTo(HaveOccurred())
		Expect(receipt.State).To(Equal(contract.ReceiptRejected))
		Expect(strings.Contains(string(receipt.Result.Payload), "stale")).To(BeTrue())
		Expect(patches).To(Equal(0))
	})
})
