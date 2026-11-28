// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mudler/LocalAI/addons/gmail/internal/gmailapi"
	"github.com/mudler/LocalAI/addons/internal/googleapi"
	"github.com/mudler/LocalAI/addons/internal/writeflow"
	"github.com/mudler/LocalAI/pkg/addons/contract"
	"github.com/mudler/LocalAI/pkg/addons/sdk/argutil"
)

const (
	ToolCalendarCreate = "calendar-event-create"
	ToolCalendarUpdate = "calendar-event-update"
	ToolCalendarDelete = "calendar-event-delete"
	ToolTaskCreate     = "task-create"
	ToolTaskUpdate     = "task-update"
	ToolTaskComplete   = "task-complete"
	ToolTaskDelete     = "task-delete"

	defaultPageSize = 30
	maxPageSize     = 50
)

var (
	calendarReadTools = []string{ToolCalendarEvents, ToolCalendarSearch, ToolCalendarGet}
	tasksReadTools    = []string{ToolTaskLists, ToolTasksList}
	productWriteTools = []string{ToolCalendarCreate, ToolCalendarUpdate, ToolCalendarDelete,
		ToolTaskCreate, ToolTaskUpdate, ToolTaskComplete, ToolTaskDelete}
)

type productError struct{ err error }

func (e *productError) Error() string { return "Google Calendar or Tasks request failed" }
func (e *productError) Unwrap() error { return e.err }

func (r *Runtime) describe(err error) string {
	var pe *productError
	if !errors.As(err, &pe) {
		return gmailapi.Describe(err)
	}
	switch {
	case errors.Is(pe.err, googleapi.ErrScope):
		return "Google Calendar or Tasks refused the requested access"
	case errors.Is(pe.err, googleapi.ErrNotFound):
		return "the Calendar or Tasks item was not found or is not accessible"
	case errors.Is(pe.err, googleapi.ErrForbidden):
		return "Google Calendar or Tasks refused the request"
	case errors.Is(pe.err, googleapi.ErrInvalid):
		return "Google Calendar or Tasks refused the request"
	case errors.Is(pe.err, googleapi.ErrUnavailable):
		return "Google Calendar or Tasks is not reachable now"
	default:
		return "Google Calendar or Tasks failed to answer"
	}
}

func scopeName(scope string) string { return scope[strings.LastIndex(scope, "/")+1:] }

type calendarDateTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type calendarEvent struct {
	ID             string           `json:"id"`
	Summary        string           `json:"summary"`
	Description    string           `json:"description,omitempty"`
	DescriptionCut bool             `json:"descriptionCut,omitempty"`
	Location       string           `json:"location,omitempty"`
	Status         string           `json:"status,omitempty"`
	Updated        string           `json:"updated,omitempty"`
	Start          calendarDateTime `json:"start"`
	End            calendarDateTime `json:"end"`
}

type calendarEventsPage struct {
	Items         []calendarEvent `json:"items"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
}

type taskList struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type taskListsPage struct {
	Items         []taskList `json:"items"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

type googleTask struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Notes     string `json:"notes,omitempty"`
	NotesCut  bool   `json:"notesCut,omitempty"`
	Status    string `json:"status"`
	Due       string `json:"due,omitempty"`
	Updated   string `json:"updated,omitempty"`
	Completed string `json:"completed,omitempty"`
}

type tasksPage struct {
	Items         []googleTask `json:"items"`
	NextPageToken string       `json:"nextPageToken,omitempty"`
}

func (c *call) readProductivity(tool string, raw map[string]json.RawMessage) (any, error) {
	label, err := resourceLabel(raw)
	if err != nil {
		return nil, err
	}
	if err := c.checkProductLabel(label); err != nil {
		return nil, err
	}
	if slices.Contains(calendarReadTools, tool) {
		if !c.granted(ScopeCalendarRead) {
			return nil, bad("Allow %s after reconnect", scopeName(ScopeCalendarRead))
		}
		return c.readCalendar(tool, raw)
	}
	if slices.Contains(tasksReadTools, tool) {
		if !c.granted(ScopeTasksRead) {
			return nil, bad("Allow %s after reconnect", scopeName(ScopeTasksRead))
		}
		return c.readTasks(tool, raw)
	}
	return nil, bad("unknown tool")
}

func (c *call) readCalendar(tool string, raw map[string]json.RawMessage) (any, error) {
	switch tool {
	case ToolCalendarEvents, ToolCalendarSearch:
		var a struct {
			Label    string `json:"label"`
			From     string `json:"from"`
			To       string `json:"to"`
			Query    string `json:"query"`
			Page     string `json:"pageToken"`
			PageSize int    `json:"pageSize"`
		}
		if err := argutil.DecodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if tool == ToolCalendarSearch && strings.TrimSpace(a.Query) == "" {
			return nil, bad("query is required")
		}
		now := time.Now().UTC()
		if a.From == "" {
			a.From = now.Format(time.RFC3339)
		}
		if a.To == "" {
			a.To = now.AddDate(0, 0, 7).Format(time.RFC3339)
		}
		from, err := parseCalendarTime(a.From)
		if err != nil {
			return nil, bad("from must be an RFC3339 date-time or YYYY-MM-DD")
		}
		to, err := parseCalendarTime(a.To)
		if err != nil || !to.After(from) {
			return nil, bad("to must be after from and use RFC3339 or YYYY-MM-DD")
		}
		q := url.Values{
			"timeMin":      {a.From},
			"timeMax":      {a.To},
			"singleEvents": {"true"},
			"orderBy":      {"startTime"},
			"maxResults":   {fmt.Sprint(pageSize(a.PageSize))},
		}
		if tool == ToolCalendarSearch {
			q.Set("q", strings.TrimSpace(a.Query))
		}
		if a.Page != "" {
			q.Set("pageToken", a.Page)
		}
		var page calendarEventsPage
		err = c.r.CalendarAPI.GetJSON(c.ctx, c.token, "/calendars/primary/events", q, &page)
		if err != nil {
			return nil, &productError{err}
		}
		for i := range page.Items {
			page.Items[i].Description, page.Items[i].DescriptionCut = cutText(page.Items[i].Description, 600)
		}
		return map[string]any{"items": page.Items, "nextPageToken": page.NextPageToken, "timeMin": a.From, "timeMax": a.To}, nil
	case ToolCalendarGet:
		var a struct {
			Label string `json:"label"`
			ID    string `json:"id"`
		}
		if err := argutil.DecodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if err := checkProductID("id", a.ID); err != nil {
			return nil, err
		}
		var event calendarEvent
		err := c.r.CalendarAPI.GetJSON(c.ctx, c.token, "/calendars/primary/events/"+argutil.IDPath(a.ID), nil, &event)
		if err != nil {
			return nil, &productError{err}
		}
		return map[string]any{"event": event}, nil
	default:
		return nil, bad("unknown Calendar tool")
	}
}

func (c *call) readTasks(tool string, raw map[string]json.RawMessage) (any, error) {
	var a struct {
		Label      string `json:"label"`
		TaskListID string `json:"taskListId"`
		Page       string `json:"pageToken"`
		PageSize   int    `json:"pageSize"`
	}
	if err := argutil.DecodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if tool == ToolTaskLists {
		q := url.Values{"maxResults": {fmt.Sprint(pageSize(a.PageSize))}}
		if a.Page != "" {
			q.Set("pageToken", a.Page)
		}
		var page taskListsPage
		err := c.r.TasksAPI.GetJSON(c.ctx, c.token, "/tasks/v1/users/@me/lists", q, &page)
		if err != nil {
			return nil, &productError{err}
		}
		return map[string]any{"items": page.Items, "nextPageToken": page.NextPageToken}, nil
	}
	if a.TaskListID == "" {
		a.TaskListID = "@default"
	}
	if err := checkProductID("taskListId", a.TaskListID); err != nil {
		return nil, err
	}
	q := url.Values{
		"maxResults":    {fmt.Sprint(pageSize(a.PageSize))},
		"showCompleted": {"true"},
		"showHidden":    {"true"},
	}
	if a.Page != "" {
		q.Set("pageToken", a.Page)
	}
	var page tasksPage
	path := "/tasks/v1/lists/" + argutil.IDPath(a.TaskListID) + "/tasks"
	if err := c.r.TasksAPI.GetJSON(c.ctx, c.token, path, q, &page); err != nil {
		return nil, &productError{err}
	}
	for i := range page.Items {
		page.Items[i].Notes, page.Items[i].NotesCut = cutText(page.Items[i].Notes, 600)
	}
	return map[string]any{"taskListId": a.TaskListID, "items": page.Items, "nextPageToken": page.NextPageToken}, nil
}

func pageSize(n int) int {
	if n < 1 {
		return defaultPageSize
	}
	return min(n, maxPageSize)
}

func cutText(value string, limit int) (string, bool) {
	text, cut := argutil.Runes(value, limit)
	return text, cut
}

func parseCalendarTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

func checkProductID(name, value string) error {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
		return bad("%s is invalid", name)
	}
	return nil
}

func (c *call) checkProductLabel(label string) error {
	if label == "" || c.key == "" || label != c.key {
		return bad("use the label of this connection")
	}
	return nil
}

func resourceLabel(raw map[string]json.RawMessage) (string, error) {
	value, ok := raw["label"]
	if !ok {
		return "", bad("use the label of this connection")
	}
	var label string
	if err := json.Unmarshal(value, &label); err != nil {
		return "", bad("use the label of this connection")
	}
	return label, nil
}

func checkProductText(name, value string, max int, required bool) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) > max || len(value) > max {
		return bad("%s exceeds %d characters", name, max)
	}
	if required && strings.TrimSpace(value) == "" {
		return bad("%s is required", name)
	}
	return nil
}

type productWrite struct {
	tool        string
	title       *string
	start       *string
	end         *string
	description *string
	location    *string
	taskListID  string
	taskID      string
	notes       *string
	due         *string
	status      *string
}

func (c *call) parseProductWrite(tool string, raw map[string]json.RawMessage) (productWrite, error) {
	var a struct {
		Label       string  `json:"label"`
		ID          string  `json:"id"`
		Title       *string `json:"title"`
		Start       *string `json:"start"`
		End         *string `json:"end"`
		Description *string `json:"description"`
		Location    *string `json:"location"`
		TaskListID  string  `json:"taskListId"`
		TaskID      string  `json:"taskId"`
		Notes       *string `json:"notes"`
		Due         *string `json:"due"`
		Status      *string `json:"status"`
	}
	if err := argutil.DecodeArgs(raw, &a); err != nil {
		return productWrite{}, err
	}
	w := productWrite{tool: tool, title: a.Title, start: a.Start, end: a.End, description: a.Description,
		location: a.Location, taskListID: a.TaskListID, taskID: a.ID, notes: a.Notes, due: a.Due, status: a.Status}
	switch tool {
	case ToolCalendarCreate:
		if w.title == nil || w.start == nil {
			return w, bad("title and start are required")
		}
	case ToolCalendarUpdate, ToolCalendarDelete:
		if err := checkProductID("id", w.taskID); err != nil {
			return w, err
		}
	case ToolTaskCreate:
		if w.title == nil {
			return w, bad("title is required")
		}
		w.taskListID = defaultTaskList(w.taskListID)
		if err := checkProductID("taskListId", w.taskListID); err != nil {
			return w, err
		}
	case ToolTaskUpdate, ToolTaskComplete, ToolTaskDelete:
		if err := checkProductID("id", w.taskID); err != nil {
			return w, err
		}
		w.taskListID = defaultTaskList(w.taskListID)
		if err := checkProductID("taskListId", w.taskListID); err != nil {
			return w, err
		}
	}
	if w.title != nil {
		maxTitle := 1024
		if strings.HasPrefix(tool, "calendar-") {
			maxTitle = 200
		} else {
			maxTitle = 300
		}
		if err := checkProductText("title", *w.title, maxTitle, true); err != nil {
			return w, err
		}
	}
	if w.description != nil {
		if err := checkProductText("description", *w.description, 1000, false); err != nil {
			return w, err
		}
	}
	if w.location != nil {
		if err := checkProductText("location", *w.location, 300, false); err != nil {
			return w, err
		}
	}
	if w.notes != nil {
		if err := checkProductText("notes", *w.notes, 1000, false); err != nil {
			return w, err
		}
	}
	if w.start != nil {
		if _, err := parseCalendarTime(*w.start); err != nil {
			return w, bad("start must be RFC3339 or YYYY-MM-DD")
		}
	}
	if w.end != nil {
		if _, err := parseCalendarTime(*w.end); err != nil {
			return w, bad("end must be RFC3339 or YYYY-MM-DD")
		}
	}
	if tool == ToolCalendarCreate && w.end != nil && w.start != nil {
		if (len(*w.start) == len("2006-01-02")) != (len(*w.end) == len("2006-01-02")) {
			return w, bad("start and end must both be dates or date-times")
		}
		start, _ := parseCalendarTime(*w.start)
		end, _ := parseCalendarTime(*w.end)
		if !end.After(start) {
			return w, bad("end must be after start")
		}
	}
	if w.due != nil && *w.due != "" {
		if _, err := time.Parse(time.RFC3339, *w.due); err != nil {
			return w, bad("due must be an RFC3339 date-time")
		}
	}
	if w.status != nil && *w.status != "needsAction" && *w.status != "completed" {
		return w, bad("status must be needsAction or completed")
	}
	if tool == ToolCalendarUpdate && w.title == nil && w.start == nil && w.end == nil && w.description == nil && w.location == nil {
		return w, bad("provide at least one event field to update")
	}
	if tool == ToolTaskUpdate && w.title == nil && w.notes == nil && w.due == nil && w.status == nil {
		return w, bad("provide at least one task field to update")
	}
	return w, nil
}

func defaultTaskList(id string) string {
	if id == "" {
		return "@default"
	}
	return id
}

func isProductWrite(tool string) bool { return slices.Contains(productWriteTools, tool) }

func (r *Runtime) previewProductivity(ctx context.Context, env contract.Envelope, tool string, raw map[string]json.RawMessage) (contract.Preview, error) {
	w, c, err := r.prepareProductWrite(ctx, env, tool, raw)
	if err != nil {
		return contract.Preview{}, err
	}
	s, rev, err := c.inspectProductWrite(w)
	if err != nil {
		return contract.Preview{}, r.translateWrite(err, false)
	}
	summary := argutil.PlainJSON(map[string]any{"count": 1, "sample": s})
	if len(summary) > 3<<10 {
		return contract.Preview{}, bad("preview too large shorten the change")
	}
	return contract.Preview{OperationID: env.Request.OperationID, Revision: contract.Revision{Schema: 1, Data: rev},
		ExpiresAt: r.Time().Add(24 * time.Hour), Summary: summary}, nil
}

func (r *Runtime) prepareProductWrite(ctx context.Context, env contract.Envelope, tool string, raw map[string]json.RawMessage) (productWrite, *call, error) {
	c := r.newCall(ctx, env)
	if c.token == "" {
		return productWrite{}, nil, bad("no access token for this call")
	}
	label, err := resourceLabel(raw)
	if err != nil {
		return productWrite{}, nil, err
	}
	if err := c.checkProductLabel(label); err != nil {
		return productWrite{}, nil, err
	}
	write := productWrite{}
	if err := c.withProductWriteScope(tool, func() error {
		var err error
		write, err = c.parseProductWrite(tool, raw)
		return err
	}); err != nil {
		return productWrite{}, nil, err
	}
	return write, c, nil
}

func (c *call) withProductWriteScope(tool string, f func() error) error {
	if strings.HasPrefix(tool, "calendar-") {
		if !slices.Contains(c.scopes, ScopeCalendarWrite) {
			return bad("Allow %s after reconnect", scopeName(ScopeCalendarWrite))
		}
	} else if !slices.Contains(c.scopes, ScopeTasksWrite) {
		return bad("Allow %s after reconnect", scopeName(ScopeTasksWrite))
	}
	return f()
}

type productTarget struct {
	event *calendarEvent
	task  *googleTask
}

func (c *call) inspectProductWrite(w productWrite) (map[string]any, uint64, error) {
	sample := map[string]any{"tool": w.tool, "action": w.tool}
	if w.title != nil {
		sample["title"] = *w.title
	}
	if w.start != nil {
		sample["start"] = *w.start
	}
	if w.end != nil {
		sample["end"] = *w.end
	}
	if w.description != nil {
		sample["description"] = *w.description
	}
	if w.location != nil {
		sample["location"] = *w.location
	}
	if w.notes != nil {
		sample["notes"] = *w.notes
	}
	if w.due != nil {
		sample["due"] = *w.due
	}
	if w.status != nil {
		sample["status"] = *w.status
	}
	if w.taskID != "" {
		sample["id"] = w.taskID
	}
	if w.taskListID != "" {
		sample["taskListId"] = w.taskListID
	}
	raw, _ := json.Marshal([]any{w.tool, w.title, w.start, w.end, w.description, w.location, w.taskListID, w.taskID, w.notes, w.due, w.status})
	revParts := []string{string(raw)}
	switch w.tool {
	case ToolCalendarUpdate, ToolCalendarDelete:
		var target calendarEvent
		err := c.r.CalendarAPI.GetJSON(c.ctx, c.token, "/calendars/primary/events/"+argutil.IDPath(w.taskID), nil, &target)
		if err != nil {
			return nil, 0, &productError{err}
		}
		dateStart := target.Start.Date != ""
		if w.start != nil && (len(*w.start) == len("2006-01-02")) != dateStart {
			return nil, 0, bad("start date type must match the event")
		}
		if w.end != nil && (len(*w.end) == len("2006-01-02")) != (target.End.Date != "") {
			return nil, 0, bad("end date type must match the event")
		}
		if w.start != nil || w.end != nil {
			startValue := target.Start.DateTime
			if startValue == "" {
				startValue = target.Start.Date
			}
			endValue := target.End.DateTime
			if endValue == "" {
				endValue = target.End.Date
			}
			if w.start != nil {
				startValue = *w.start
			}
			if w.end != nil {
				endValue = *w.end
			}
			start, startErr := parseCalendarTime(startValue)
			end, endErr := parseCalendarTime(endValue)
			if startErr != nil || endErr != nil || !end.After(start) {
				return nil, 0, bad("end must be after start")
			}
		}
		sample["target"] = map[string]any{"id": target.ID, "title": target.Summary, "start": target.Start, "end": target.End, "updated": target.Updated}
		targetRaw, _ := json.Marshal(target)
		revParts = append(revParts, string(targetRaw))
	case ToolTaskUpdate, ToolTaskComplete, ToolTaskDelete:
		var target googleTask
		path := taskPath(w.taskListID, w.taskID)
		if err := c.r.TasksAPI.GetJSON(c.ctx, c.token, path, nil, &target); err != nil {
			return nil, 0, &productError{err}
		}
		sample["target"] = map[string]any{"id": target.ID, "title": target.Title, "status": target.Status, "due": target.Due, "updated": target.Updated}
		targetRaw, _ := json.Marshal(target)
		revParts = append(revParts, string(targetRaw))
	}
	return sample, argutil.Sum64(revParts...), nil
}

type productSession struct {
	c *call
	w productWrite
}

func (s *productSession) Inspect() (uint64, error) {
	_, rev, err := s.c.inspectProductWrite(s.w)
	return rev, err
}
func (*productSession) InspectFailed(bool, error) (map[string]any, bool) { return nil, false }
func (*productSession) Resume(started bool) (map[string]any, bool, error) {
	if started {
		return nil, false, errors.New("an earlier Calendar or Tasks change may have gone through; check the service before retrying")
	}
	return nil, false, nil
}

func (s *productSession) Send() (map[string]any, error) {
	out, err := s.c.sendProductWrite(s.w)
	if err != nil {
		return nil, &productError{err}
	}
	return out, nil
}

func (r *Runtime) applyProductivity(ctx context.Context, env contract.Envelope, rev contract.Revision, tool string, raw map[string]json.RawMessage) (contract.Receipt, error) {
	return r.Runtime.Apply(env.Request.OperationID, rev, func() (writeflow.Session, error) {
		w, c, err := r.prepareProductWrite(ctx, env, tool, raw)
		if err != nil {
			return nil, err
		}
		return &productSession{c: c, w: w}, nil
	})
}

func taskPath(listID, taskID string) string {
	return "/tasks/v1/lists/" + argutil.IDPath(listID) + "/tasks/" + argutil.IDPath(taskID)
}

func eventDateTime(value string) calendarDateTime {
	if len(value) == len("2006-01-02") {
		return calendarDateTime{Date: value}
	}
	return calendarDateTime{DateTime: value}
}

func (c *call) sendProductWrite(w productWrite) (map[string]any, error) {
	switch w.tool {
	case ToolCalendarCreate:
		end := time.Time{}
		start, _ := parseCalendarTime(*w.start)
		if w.end == nil {
			end = start.Add(time.Hour)
		} else {
			end, _ = parseCalendarTime(*w.end)
		}
		endString := end.Format(time.RFC3339)
		if len(*w.start) == len("2006-01-02") {
			endString = end.AddDate(0, 0, 1).Format("2006-01-02")
		}
		event := map[string]any{"summary": *w.title, "start": eventDateTime(*w.start), "end": eventDateTime(endString)}
		if w.description != nil {
			event["description"] = *w.description
		}
		if w.location != nil {
			event["location"] = *w.location
		}
		var created calendarEvent
		err := c.r.CalendarAPI.SendJSON(c.ctx, http.MethodPost, c.token, "/calendars/primary/events", url.Values{"sendUpdates": {"none"}}, event, &created)
		return map[string]any{"event": created}, err
	case ToolCalendarUpdate:
		update := map[string]any{}
		if w.title != nil {
			update["summary"] = *w.title
		}
		if w.start != nil {
			update["start"] = eventDateTime(*w.start)
		}
		if w.end != nil {
			update["end"] = eventDateTime(*w.end)
		}
		if w.description != nil {
			update["description"] = *w.description
		}
		if w.location != nil {
			update["location"] = *w.location
		}
		var updated calendarEvent
		path := "/calendars/primary/events/" + argutil.IDPath(w.taskID)
		err := c.r.CalendarAPI.SendJSON(c.ctx, http.MethodPatch, c.token, path, url.Values{"sendUpdates": {"none"}}, update, &updated)
		return map[string]any{"event": updated}, err
	case ToolCalendarDelete:
		path := "/calendars/primary/events/" + argutil.IDPath(w.taskID)
		err := c.r.CalendarAPI.Send(c.ctx, c.token, googleapi.Request{Method: http.MethodDelete, Path: path, Query: url.Values{"sendUpdates": {"none"}}}, nil)
		return map[string]any{"deleted": w.taskID}, err
	case ToolTaskCreate:
		task := map[string]any{"title": *w.title}
		if w.notes != nil {
			task["notes"] = *w.notes
		}
		if w.due != nil {
			task["due"] = *w.due
		}
		var created googleTask
		err := c.r.TasksAPI.SendJSON(c.ctx, http.MethodPost, c.token, "/tasks/v1/lists/"+argutil.IDPath(defaultTaskList(w.taskListID))+"/tasks", nil, task, &created)
		return map[string]any{"task": created}, err
	case ToolTaskUpdate, ToolTaskComplete:
		update := map[string]any{}
		if w.tool == ToolTaskComplete {
			update["status"] = "completed"
		} else {
			if w.title != nil {
				update["title"] = *w.title
			}
			if w.notes != nil {
				update["notes"] = *w.notes
			}
			if w.due != nil {
				update["due"] = *w.due
			}
			if w.status != nil {
				update["status"] = *w.status
			}
		}
		var updated googleTask
		err := c.r.TasksAPI.SendJSON(c.ctx, http.MethodPatch, c.token, taskPath(w.taskListID, w.taskID), nil, update, &updated)
		return map[string]any{"task": updated}, err
	case ToolTaskDelete:
		err := c.r.TasksAPI.Send(c.ctx, c.token, googleapi.Request{Method: http.MethodDelete, Path: taskPath(w.taskListID, w.taskID)}, nil)
		return map[string]any{"deleted": w.taskID}, err
	default:
		return nil, errors.New("unknown Google write tool")
	}
}
