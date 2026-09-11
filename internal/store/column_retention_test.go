package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAliasedColumnRetentionSurvivesParkPullAndDrop(t *testing.T) {
	for _, op := range []string{OpTaskUpdate, OpTaskMove} {
		for _, phase := range []string{"armed", "rejected"} {
			for _, alias := range []string{"COLUMN_ID", "Column_Id"} {
				for _, location := range []string{"root", "snapshot"} {
					t.Run(op+"/"+phase+"/"+alias+"/"+location, func(t *testing.T) {
						ctx := context.Background()
						st := testStore(t)
						task := columnMoveFixture(t, st, "a")
						preview, err := st.PreviewTaskColumn(ctx, task, "b")
						if err != nil {
							t.Fatal(err)
						}
						if _, err := st.ApplyTaskColumn(ctx, preview); err != nil {
							t.Fatal(err)
						}
						claimed, _, err := st.Claim(ctx, 1, time.Minute)
						if err != nil || len(claimed) != 1 {
							t.Fatalf("claim: %+v %v", claimed, err)
						}
						item := claimed[0]
						send, present, post, err := st.PrepareFeatureSend(ctx, item)
						if err != nil || !present || !post {
							t.Fatalf("arm: %t %t %v", present, post, err)
						}
						if phase == "rejected" {
							if err := st.RejectFeature(ctx, item); err != nil {
								t.Fatal(err)
							}
							send.Metadata.Phase = FeatureRejected
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(mustColumnPayload(t, send), &payload); err != nil {
							t.Fatal(err)
						}
						column := payload["column_id"]
						delete(payload, "column_id")
						if location == "root" {
							payload[alias] = column
						} else {
							// A structurally valid v4 envelope can retain a different
							// extension in its armed snapshot; it must not be discarded.
							payload["estimated_pomo"] = json.RawMessage(`3`)
							var metadata map[string]json.RawMessage
							if err := json.Unmarshal(payload["_tt"], &metadata); err != nil {
								t.Fatal(err)
							}
							metadata["extension_baseline"] = json.RawMessage(`{"estimated_pomo":2}`)
							delete(metadata, "snapshot")
							metadata["Snapshot"] = json.RawMessage(`{"Extensions":{"` + alias + `":"b"}}`)
							payload["_tt"], err = json.Marshal(metadata)
							if err != nil {
								t.Fatal(err)
							}
						}
						raw, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						edit, metadata, err := DecodeTaskEditPayload(raw)
						if err != nil || metadata == nil || metadata.Version != 4 {
							t.Fatalf("fixture is not a decodable v4 payload: %+v %v", metadata, err)
						}
						if location == "root" && (edit.ColumnId == nil || *edit.ColumnId != "b") || location == "snapshot" && (edit.ColumnId != nil || metadata.Snapshot.Extensions.ColumnId == nil || *metadata.Snapshot.Extensions.ColumnId != "b") {
							t.Fatal("fixture lost the retained column field")
						}
						if _, err := st.DB().Exec(`UPDATE outbox SET op=?,payload=? WHERE seq=?`, op, string(raw), item.Seq); err != nil {
							t.Fatal(err)
						}
						if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "retained fixture"); err != nil {
							t.Fatal(err)
						}
						var dirty int64
						var beforeRaw string
						if err := st.DB().QueryRow(`SELECT dirty,raw FROM tasks WHERE id=?`, task.Id).Scan(&dirty, &beforeRaw); err != nil || dirty != item.Rev {
							t.Fatalf("parking cleared retained revision: %d != %d: %v", dirty, item.Rev, err)
						}
						if _, err := st.SyncProject(ctx, task.ProjectId, []ServerTask{{Task: task, Raw: json.RawMessage(beforeRaw)}}); err != nil {
							t.Fatal(err)
						}
						if current, err := st.Task(ctx, task.Id); err != nil || current.ColumnId != "b" {
							t.Fatalf("pull erased retained column: %+v %v", current, err)
						}
						if dropped, err := st.DropParked(ctx); err != nil || len(dropped) != 0 {
							t.Fatalf("drop discarded column evidence: %+v %v", dropped, err)
						}
						var retained string
						if err := st.DB().QueryRow(`SELECT payload FROM outbox WHERE seq=?`, item.Seq).Scan(&retained); err != nil || retained != string(raw) {
							t.Fatalf("retained payload changed: %v", err)
						}
						if counts, err := st.OutboxCounts(ctx); err != nil || counts.Failed != 1 {
							t.Fatalf("parked operation disappeared: %+v %v", counts, err)
						}
					})
				}
			}
		}
	}
}
