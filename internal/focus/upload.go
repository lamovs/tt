package focus

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

const stamp = "2006-01-02T15:04:05.000-0700"

type Client interface {
	CreateFocus(context.Context, api.FocusCreate) (*api.Focus, error)
	GetFocus(context.Context, string, api.FocusType) (*api.Focus, error)
	GetFocuses(context.Context, string, string, api.FocusType) ([]api.Focus, error)
}

type TopicClient interface {
	CredentialFingerprint() string
	ListTopics(context.Context) ([]webapi.Topic, error)
	CreateTopicFocus(context.Context, webapi.FocusCreate) (webapi.BatchResponse, error)
	GetTopicFocus(context.Context, string, int) (webapi.FocusCreate, error)
}

type TopicClientFactory func() (TopicClient, error)

var ErrBrowserNotConfigured = errors.New("Browser authorization is not configured; run tt login web --same-account for Timer topics")

type Result struct {
	Uploaded, Held int
	Errors         []error
}

func Upload(ctx context.Context, st *store.Store, client Client, includeAborted, retryRejected bool, topics ...TopicClientFactory) Result {
	var result Result
	var topicClient TopicClientFactory
	if len(topics) != 0 {
		topicClient = topics[0]
	}
	ids, err := st.PendingFocusSessionIDs(ctx, includeAborted)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			result.Errors = append(result.Errors, ctx.Err())
			break
		}
		if err := uploadOneWithTopic(ctx, st, client, topicClient, id, retryRejected); err != nil {
			result.Held++
			result.Errors = append(result.Errors, fmt.Errorf("session %s: %w", id, err))
		} else {
			result.Uploaded++
		}
	}
	return result
}

func requestFor(session store.TimerSession) (api.FocusCreate, error) {
	if session.NoteReviewPending {
		return api.FocusCreate{}, errors.New("focus session is waiting for a completion note or skip")
	}
	if !session.MillisecondPrecision || session.EndedAt.IsZero() {
		return api.FocusCreate{}, errors.New("session has no completed precise timing")
	}
	if store.IsLocalID(session.TaskID) {
		return api.FocusCreate{}, errors.New("task is still local; run tt sync before uploading this session")
	}
	request := api.FocusCreate{Type: api.FocusType(session.FocusType), TaskID: session.TaskID, Note: session.Note,
		StartTime: session.StartedAt.UTC().Format(stamp), EndTime: session.EndedAt.UTC().Format(stamp),
		PauseDuration: int64(session.PauseDuration / time.Second), Duration: int64(session.ActiveDuration / time.Second)}
	if request.Duration < 1 {
		return request, errors.New("less than one full second of active time; session remains in local history")
	}
	if session.EndedAt.Sub(session.StartedAt) > 30*24*time.Hour-2*time.Second {
		return request, errors.New("session exceeds the API confirmation window; retained locally")
	}
	return request, nil
}

func uploadOne(ctx context.Context, st *store.Store, client Client, id string, retryRejected bool) error {
	return uploadOneChecked(ctx, st, client, id, retryRejected, "")
}

func uploadOneWithTopic(ctx context.Context, st *store.Store, client Client, topicClient TopicClientFactory, id string, retryRejected bool) error {
	target, err := st.ReadFocusTarget(ctx, id)
	if err != nil {
		return err
	}
	if target.Session.TopicID != "" {
		if topicClient == nil {
			return errors.New("topic focus needs a valid Browser session; run tt login web --same-account")
		}
		resolved, err := topicClient()
		if err != nil {
			return err
		}
		return uploadTopicOneChecked(ctx, st, resolved, target, retryRejected, "")
	}
	return uploadOneChecked(ctx, st, client, id, retryRejected, "")
}

func topicRequestFor(session store.TimerSession) (webapi.FocusCreate, error) {
	if err := validateTopicSession(session); err != nil {
		return webapi.FocusCreate{}, err
	}
	var idBytes [12]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return webapi.FocusCreate{}, fmt.Errorf("generate remote focus id: %w", err)
	}
	start, end := session.StartedAt.UTC().Format(stamp), session.EndedAt.UTC().Format(stamp)
	return webapi.FocusCreate{ID: hex.EncodeToString(idBytes[:]), Type: session.FocusType, Status: 1, Added: true,
		StartTime: start, EndTime: end, PauseDuration: int64(session.PauseDuration / time.Second), Note: session.Note,
		Tasks: []webapi.FocusTopic{{TimerID: session.TopicID, TimerName: session.TopicName, StartTime: start, EndTime: end}}}, nil
}

func validateTopicSession(session store.TimerSession) error {
	if session.NoteReviewPending {
		return errors.New("focus session is waiting for a completion note or skip")
	}
	if !session.MillisecondPrecision || session.EndedAt.IsZero() || session.TopicID == "" || session.TopicName == "" || session.TopicCredential == "" || session.TaskID != "" {
		return errors.New("topic focus session has an invalid frozen destination or timing")
	}
	if session.ActiveDuration < time.Second {
		return errors.New("less than one full second of active time; session remains in local history")
	}
	if session.EndedAt.Sub(session.StartedAt) > 30*24*time.Hour-2*time.Second {
		return errors.New("session exceeds the API confirmation window; retained locally")
	}
	return nil
}

func uploadTopicOneChecked(ctx context.Context, st *store.Store, client TopicClient, target store.FocusTarget, retryRejected bool, expected string) error {
	if client == nil {
		return errors.New("topic focus needs a valid Browser session; run tt login web --same-account")
	}
	session := target.Session
	allowed, err := st.BrowserCredentialAllows(ctx, session.TopicCredential, client.CredentialFingerprint())
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("topic focus belongs to a different Browser credential; run tt login web --same-account to confirm recovery for the same account")
	}
	row, err := st.FocusUpload(ctx, session.ID)
	send := false
	if errors.Is(err, sql.ErrNoRows) {
		if target.Upload != nil || expected != "" && target.Version != expected {
			return errors.New("focus session or upload changed before preparation; refresh and retry")
		}
		request, err := topicRequestFor(session)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		row, send, err = st.ArmFocusUploadChecked(ctx, session.ID, "", raw, []string{}, target.Version)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if row.Phase == "confirmed" {
		return nil
	}
	var request webapi.FocusCreate
	if err := json.Unmarshal(row.Request, &request); err != nil || !frozenTopicRequestMatches(session, request) {
		return errors.New("invalid frozen topic focus request; retained without sending")
	}
	if row.Phase == "rejected" {
		if !retryRejected {
			return errors.New("server rejected this topic session; use tt timer sync --retry-failed after fixing the cause")
		}
		send, err = st.RearmRejectedFocusUploadChecked(ctx, row, expected)
		if err != nil {
			return err
		}
		row, err = st.FocusUpload(ctx, session.ID)
		if err != nil {
			return err
		}
	}
	if send {
		response, postErr := client.CreateTopicFocus(ctx, request)
		if postErr != nil {
			var status *webapi.StatusError
			var guard *webapi.RequestGuardError
			rejected := errors.As(postErr, &guard) || errors.As(postErr, &status) && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusConflict
			if err := durable(ctx, func(c context.Context) error { return st.RecordFocusUploadError(c, row, postErr.Error(), rejected) }); err != nil {
				return err
			}
			return postErr
		}
		raw, _ := json.Marshal(response)
		if err := durable(ctx, func(c context.Context) error { return st.AcceptFocusUpload(c, row, request.ID, raw) }); err != nil {
			return err
		}
		row, err = st.FocusUpload(context.WithoutCancel(ctx), session.ID)
		if err != nil {
			return err
		}
	}
	remote, err := client.GetTopicFocus(ctx, request.ID, request.Type)
	if err == nil {
		err = confirmTopic(request, remote)
	}
	if err != nil {
		if saved := durable(ctx, func(c context.Context) error { return st.RecordFocusUploadError(c, row, err.Error(), false) }); saved != nil {
			return saved
		}
		return err
	}
	raw, _ := json.Marshal(remote)
	return durable(ctx, func(c context.Context) error { return st.ConfirmFocusUpload(c, row, request.ID, raw, time.Now()) })
}

func confirmTopic(request, remote webapi.FocusCreate) error {
	if remote.ID != request.ID || remote.Type != request.Type || remote.Status != request.Status || remote.Added != request.Added || remote.StartTime != request.StartTime || remote.EndTime != request.EndTime || remote.PauseDuration != request.PauseDuration || remote.Note != request.Note || len(remote.Tasks) != 1 {
		return errors.New("topic focus read-back does not match the frozen record")
	}
	want, got := request.Tasks[0], remote.Tasks[0]
	if got.TimerID != want.TimerID || got.TimerName != want.TimerName || got.StartTime != want.StartTime || got.EndTime != want.EndTime {
		return errors.New("topic focus read-back does not match the frozen Timer topic")
	}
	return nil
}

func frozenTopicRequestMatches(session store.TimerSession, request webapi.FocusCreate) bool {
	if webapi.ValidateFocus(request) != nil || validateTopicSession(session) != nil {
		return false
	}
	start, end := session.StartedAt.UTC().Format(stamp), session.EndedAt.UTC().Format(stamp)
	topic := request.Tasks[0]
	return request.Type == session.FocusType && request.StartTime == start && request.EndTime == end &&
		request.PauseDuration == int64(session.PauseDuration/time.Second) && request.AdjustTime == 0 && request.Note == session.Note &&
		topic.TimerID == session.TopicID && topic.TimerName == session.TopicName && topic.StartTime == start && topic.EndTime == end
}

func uploadOneChecked(ctx context.Context, st *store.Store, client Client, id string, retryRejected bool, expected string) error {
	row, err := st.FocusUpload(ctx, id)
	send := false
	if errors.Is(err, sql.ErrNoRows) {
		target, err := st.ReadFocusTarget(ctx, id)
		if err != nil {
			return err
		}
		if target.Upload != nil || expected != "" && target.Version != expected {
			return errors.New("focus session or upload changed before preparation; refresh and retry")
		}
		session := target.Session
		request, err := requestFor(session)
		if err != nil {
			return err
		}
		from, to, err := window(request)
		if err != nil {
			return err
		}
		baseline, err := client.GetFocuses(ctx, from, to, request.Type)
		if err != nil {
			return err
		}
		prior, err := baselineIDs(baseline)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		row, send, err = st.ArmFocusUploadChecked(ctx, id, session.TaskID, raw, prior, target.Version)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if row.Phase == "confirmed" {
		return nil
	}
	var request api.FocusCreate
	if err := json.Unmarshal(row.Request, &request); err != nil {
		return errors.New("invalid frozen focus request; retained without sending")
	}
	if row.Phase == "rejected" {
		if !retryRejected {
			return errors.New("server rejected this session; use tt timer sync --retry-failed after fixing the cause")
		}
		send, err = st.RearmRejectedFocusUploadChecked(ctx, row, expected)
		if err != nil {
			return err
		}
		row, err = st.FocusUpload(ctx, id)
		if err != nil {
			return err
		}
	}
	if send {
		created, postErr := client.CreateFocus(ctx, request)
		if postErr != nil {
			var status *api.StatusError
			var guard *api.RequestGuardError
			rejected := errors.As(postErr, &guard) || errors.As(postErr, &status) && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout
			if err := durable(ctx, func(c context.Context) error { return st.RecordFocusUploadError(c, row, postErr.Error(), rejected) }); err != nil {
				return err
			}
			return postErr
		}
		if err := durable(ctx, func(c context.Context) error { return st.AcceptFocusUpload(c, row, created.ID, created.Raw) }); err != nil {
			return err
		}
		row, err = st.FocusUpload(context.WithoutCancel(ctx), id)
		if err != nil {
			return err
		}
	}
	var remote *api.Focus
	if row.RemoteID != "" {
		remote, err = client.GetFocus(ctx, row.RemoteID, request.Type)
		if err == nil && remote.ID != row.RemoteID {
			err = errors.New("focus read returned a different ID")
		}
	} else {
		from, to, windowErr := window(request)
		if windowErr != nil {
			return windowErr
		}
		var candidates []api.Focus
		candidates, err = client.GetFocuses(ctx, from, to, request.Type)
		if err == nil {
			_, err = baselineIDs(candidates)
		}
		if err == nil {
			matches := 0
			for i := range candidates {
				if confirm(request, row.PriorIDs, candidates[i]) == nil {
					remote = &candidates[i]
					matches++
				}
			}
			if matches != 1 {
				err = fmt.Errorf("uncertain upload has %d exact new correspondences; retained without another POST", matches)
			}
		}
	}
	if err == nil {
		err = confirm(request, row.PriorIDs, *remote)
	}
	if err != nil {
		if saved := durable(ctx, func(c context.Context) error { return st.RecordFocusUploadError(c, row, err.Error(), false) }); saved != nil {
			return saved
		}
		return err
	}
	return durable(ctx, func(c context.Context) error { return st.ConfirmFocusUpload(c, row, remote.ID, remote.Raw, time.Now()) })
}

func durable(ctx context.Context, fn func(context.Context) error) error {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return fn(c)
}

func window(request api.FocusCreate) (string, string, error) {
	start, err := parseStamp(request.StartTime)
	if err != nil {
		return "", "", err
	}
	end, err := parseStamp(request.EndTime)
	if err != nil {
		return "", "", err
	}
	if !end.After(start) || end.Sub(start) > 30*24*time.Hour-2*time.Second {
		return "", "", errors.New("invalid focus confirmation window")
	}
	return start.Add(-time.Second).UTC().Format(stamp), end.Add(time.Second).UTC().Format(stamp), nil
}

func parseStamp(value string) (time.Time, error) {
	for _, layout := range []string{stamp, "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid focus timestamp")
}

func baselineIDs(records []api.Focus) ([]string, error) {
	ids := make([]string, 0, len(records))
	seen := map[string]bool{}
	for _, record := range records {
		if record.ID == "" || seen[record.ID] {
			return nil, errors.New("focus history has missing or duplicate IDs")
		}
		seen[record.ID] = true
		ids = append(ids, record.ID)
	}
	return ids, nil
}

func confirm(request api.FocusCreate, prior []string, remote api.Focus) error {
	if remote.ID == "" {
		return errors.New("focus read is missing its ID")
	}
	for _, id := range prior {
		if remote.ID == id {
			return errors.New("focus read identifies a preexisting session")
		}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(remote.Raw, &raw) != nil || raw == nil {
		return errors.New("focus read is not an object")
	}
	for _, key := range []string{"id", "type", "startTime", "endTime", "duration", "pauseDuration"} {
		if value, ok := raw[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("focus read lacks explicit %s", key)
		}
	}
	start, err := parseStamp(request.StartTime)
	if err != nil {
		return err
	}
	end, err := parseStamp(request.EndTime)
	if err != nil {
		return err
	}
	interval := end.Sub(start).Milliseconds()
	if interval <= 0 || request.PauseDuration < 0 || request.PauseDuration > interval/1000 {
		return errors.New("focus request has an invalid active interval")
	}

	expectedDuration := interval - request.PauseDuration*1000
	if remote.Type != request.Type || remote.Note != request.Note || remote.Duration != expectedDuration || remote.PauseDuration != request.PauseDuration {
		return errors.New("focus read does not match type, note, active duration or pause")
	}
	for _, pair := range [][2]string{{request.StartTime, remote.StartTime}, {request.EndTime, remote.EndTime}} {
		want, err := parseStamp(pair[0])
		if err != nil {
			return err
		}
		got, err := parseStamp(pair[1])
		if err != nil || !got.Equal(want) {
			return errors.New("focus read does not match the measured interval")
		}
	}
	if rawNote, ok := raw["note"]; ok && bytes.Equal(bytes.TrimSpace(rawNote), []byte("null")) {
		return errors.New("focus note is null")
	}
	if request.TaskID == "" {
		if remote.TaskID != "" || len(remote.Tasks) != 0 && !anonymousFocusInterval(request, remote, raw) {
			return errors.New("unbound focus acquired a task relation")
		}
	} else {
		if remote.TaskID != "" && remote.TaskID != request.TaskID {
			return errors.New("focus has a different task relation")
		}
		if len(remote.Tasks) != 1 || remote.Tasks[0].TaskID != request.TaskID {
			return errors.New("focus does not contain the exact task relation")
		}
	}
	return nil
}

func anonymousFocusInterval(request api.FocusCreate, remote api.Focus, raw map[string]json.RawMessage) bool {
	if len(remote.RelationType) != 1 || remote.RelationType[0] != 0 {
		return false
	}
	var intervals []map[string]json.RawMessage
	if json.Unmarshal(raw["tasks"], &intervals) != nil || len(intervals) != 1 || len(intervals[0]) != 2 {
		return false
	}
	for key, value := range map[string]string{"startTime": request.StartTime, "endTime": request.EndTime} {
		var got string
		if json.Unmarshal(intervals[0][key], &got) != nil {
			return false
		}
		wantTime, wantErr := parseStamp(value)
		gotTime, gotErr := parseStamp(got)
		if wantErr != nil || gotErr != nil || !gotTime.Equal(wantTime) {
			return false
		}
	}
	return true
}
