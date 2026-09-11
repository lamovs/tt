package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

func cmdTimerTopic(inv *invocation) int {
	if len(inv.data) != 2 || inv.data[0] != "topic" || inv.data[1] != "ls" {
		return inv.misuse("topic takes ls")
	}
	remote := false
	for _, option := range inv.refinements {
		if option != "--remote" || remote {
			return inv.misuse("topic ls accepts only one --remote option")
		}
		remote = true
	}
	st, err := inv.openStore()
	if err != nil {
		return timerFailure(inv, err)
	}
	defer st.Close()
	if remote {
		client, fingerprint, err := webClient(inv.ctx)
		if err != nil {
			return timerFailure(inv, err)
		}
		topics, err := client.ListTopics(inv.ctx)
		if err != nil {
			return timerFailure(inv, err)
		}
		if err := replaceWebTopics(inv.ctx, st, topics, fingerprint, time.Now()); err != nil {
			return timerFailure(inv, err)
		}
	}
	topics, err := st.FocusTopics(inv.ctx)
	if err != nil {
		return timerFailure(inv, err)
	}
	if inv.jsonOutput {
		rows := make([]map[string]any, 0, len(topics))
		for _, topic := range topics {
			rows = append(rows, map[string]any{"id": topic.ID, "name": topic.Name, "type": topic.Type, "status": topic.Status,
				"sort_order": topic.SortOrder, "pomodoro_time": topic.PomodoroTime, "refreshed_at": topic.RefreshedAt})
		}
		return inv.output(rows, app.ResultMeta{Source: map[bool]string{true: "remote", false: "cache"}[remote]})
	}
	if len(topics) == 0 {
		fmt.Fprintln(inv.stdout, "No cached Timer topics. Run tt login web --same-account.")
		return exitOK
	}
	for _, topic := range topics {
		state := "active"
		if topic.Status != 0 {
			state = "archived"
		}
		timerField(inv, "Topic: ", topic.Name+" ["+topic.ID+"] "+state)
	}
	return exitOK
}

func resolveTimerTopic(inv *invocation, st *store.Store, reference string) (store.FocusTopic, int, bool) {
	topic, err := st.ResolveFocusTopic(inv.ctx, reference)
	if err != nil {
		return topic, timerFailure(inv, err), false
	}
	if topic.Status != 0 {
		return topic, timerFailure(inv, errors.New("Timer topic is archived; refresh the catalog and choose an active topic")), false
	}
	_, fingerprint, err := webClient(inv.ctx)
	if err != nil {
		return topic, timerFailure(inv, err), false
	}
	if topic.CredentialFingerprint != fingerprint {
		return topic, timerFailure(inv, fmt.Errorf("cached Timer topic belongs to an older Browser credential; refresh the catalog")), false
	}
	return topic, exitOK, true
}
