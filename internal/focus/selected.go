package focus

import (
	"context"
	"errors"

	"github.com/movsar/tt/internal/store"
)

func ValidateTarget(target store.FocusTarget, includeAborted, retry bool) error {
	session := target.Session
	if target.Version == "" || session.ID == "" {
		return errors.New("select an exact focus session")
	}
	if session.SyncedAt != nil {
		return errors.New("focus session is already confirmed")
	}
	if session.Outcome != "done" && !(includeAborted && session.Outcome == "aborted") {
		return errors.New("session is retained locally by the upload policy")
	}
	if session.TopicID != "" {
		if err := validateTopicSession(session); err != nil {
			return err
		}
	} else if _, err := requestFor(session); err != nil {
		return err
	}
	if retry && (target.Upload == nil || target.Upload.Phase != "rejected") {
		return errors.New("retry requires a definitive rejection; uncertain uploads only allow read-back")
	}
	if !retry && target.Upload != nil && target.Upload.Phase == "rejected" {
		return errors.New("server rejected this session; review a separate retry")
	}
	return nil
}

func UploadSelected(ctx context.Context, st *store.Store, client Client, target store.FocusTarget, includeAborted, retry bool, topics ...TopicClientFactory) Result {
	var out Result
	err := ValidateTarget(target, includeAborted, retry)
	if err == nil {
		err = st.CheckFocusTarget(ctx, target)
	}
	if err == nil {
		if target.Session.TopicID != "" {
			var topicClient TopicClientFactory
			if len(topics) != 0 {
				topicClient = topics[0]
			}
			if topicClient == nil {
				err = errors.New("topic focus needs a valid Browser session; run tt login web --same-account")
			} else if resolved, resolveErr := topicClient(); resolveErr != nil {
				err = resolveErr
			} else {
				err = uploadTopicOneChecked(ctx, st, resolved, target, retry, target.Version)
			}
		} else {
			err = uploadOneChecked(ctx, st, client, target.Session.ID, retry, target.Version)
		}
	}
	if err != nil {
		out.Held = 1
		out.Errors = []error{err}
	} else {
		out.Uploaded = 1
	}
	return out
}
