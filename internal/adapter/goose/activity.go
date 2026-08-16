package goose

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// activityQuery reads the tool calls goose recorded above the message
// watermark. Two restrictions carry the whole design:
//
// role = 'assistant' — a tool REQUEST is the model's own output and is stored on
// the assistant message (verified on a live call: the `toolRequest` block sits
// on the assistant row, the matching `toolResponse` on the following user row).
// The response rows are the ones holding the tool's OUTPUT — command output,
// file contents, whole documents — and this adapter never reads them at all.
// That is a privacy property first and a cost saving second: what is not
// selected cannot leak.
//
// m.id > ? — messages are append-only with an AUTOINCREMENT id, so a rowid
// watermark reads each row exactly once. It is deliberately a SECOND watermark:
// usage comes from a different table that advances at its own pace.
const activityQuery = `SELECT m.id, m.message_id, m.session_id, m.created_timestamp,
	m.content_json, s.working_dir
	FROM messages m
	LEFT JOIN sessions s ON s.id = m.session_id
	WHERE m.id > ? AND m.role = 'assistant'
	ORDER BY m.id`

// contentBlock is the ALLOW-LIST decode of one block of messages.content_json.
// It is names and counts only, enforced by the shape rather than by a filter:
// there is no field for a tool's `arguments`, none for a text block's `text`,
// none for a thinking block, so encoding/json DISCARDS every one of them while
// parsing and the content never becomes a value in this process.
//
// The `_meta` object is the sharpest case. Alongside `goose_extension` (a name)
// it carries `goose.toolSummary.title` and `goose.toolChain.summary` — both
// LLM-GENERATED PROSE about what the call was doing. blockMeta has one field, so
// those are dropped at the decode and could not reach an emitted row even if a
// later change forgot to exclude them.
type contentBlock struct {
	Type     string     `json:"type"`
	ToolCall *toolCall  `json:"toolCall"`
	Meta     *blockMeta `json:"_meta"`
}

// toolCall is goose's tool_result_serde envelope reduced to the name. A failed
// call serialises as {"status":"error","error":...} and carries no name; there
// is no field here for the error text either.
type toolCall struct {
	Status string `json:"status"`
	Value  *struct {
		Name string `json:"name"`
	} `json:"value"`
}

// blockMeta is the one key of a tool request's `_meta` that is a NAME.
type blockMeta struct {
	Extension string `json:"goose_extension"`
}

// collectActivity reads tool calls from the messages above the watermark and
// returns them with the highest message rowid consumed. The final result
// reports whether the read completed: activity is a secondary observation and
// must never cost a cycle its usage events, so a failure here is reported by
// holding the checkpoint back rather than by failing the source.
func collectActivity(ctx context.Context, db *sql.DB, path string, watermark int64) ([]model.ActivityEvent, int64, bool) {
	rows, err := db.QueryContext(ctx, activityQuery, watermark)
	if err != nil {
		return nil, watermark, false
	}
	defer rows.Close()

	var (
		calls    []model.ActivityEvent
		consumed = watermark
		clean    = true
	)
	for rows.Next() {
		if ctx.Err() != nil {
			return calls, consumed, false
		}
		var (
			rowID      int64
			messageID  sql.NullString
			sessionID  string
			createdTS  int64
			content    sql.NullString
			workingDir sql.NullString
		)
		if err := rows.Scan(&rowID, &messageID, &sessionID, &createdTS, &content, &workingDir); err != nil {
			clean = false
			continue
		}
		if content.Valid && content.String != "" {
			calls = append(calls, messageCalls(
				[]byte(content.String), rowID, messageID.String, sessionID,
				createdTS, strings.TrimSpace(workingDir.String), path)...)
		}
		if clean {
			consumed = rowID
		}
	}
	if rows.Err() != nil {
		clean = false
	}
	return calls, consumed, clean
}

// messageCalls turns one assistant message's content into activity rows.
//
// The names are resolved for the WHOLE message before any row is built, because
// TurnSeq and CallsInTurn describe the message's calls as a set: a goose
// assistant message is one turn, and a turn that requested three tools must say
// so on all three rows.
func messageCalls(content []byte, rowID int64, messageID, sessionID string, createdTS int64, project, path string) []model.ActivityEvent {
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var names []string
	for _, b := range blocks {
		if name, ok := callName(b); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}

	ts := createdTS
	if ts > msTimestampThreshold {
		ts /= 1000
	}
	when := time.Time{}
	if ts > 0 {
		when = time.Unix(ts, 0).UTC()
	}

	out := make([]model.ActivityEvent, 0, len(names))
	for i, name := range names {
		out = append(out, model.ActivityEvent{
			Tool:      ToolID,
			Kind:      model.ActivityTool,
			Name:      name,
			SessionID: sessionID,
			Project:   project,
			// Model is left empty deliberately. usage_ledger and messages
			// disagree about it — the ledger records the model the provider
			// answered as ("gemma4:31b"), the session records the one that was
			// configured ("gemma4:31b-cloud") — and stamping a call with a model
			// no record ties to that call would be a guess wearing a field.
			EventTime: when,
			// NEVER attributed. usage_ledger carries no message id, and two of
			// its rows commonly share one created_timestamp (both rows of the
			// live tool-call session did), so the only available join is a
			// timestamp guess. An empty key makes the call unattributed, which
			// the store reports as unknown cost rather than as free.
			UsageDedupKey: "",
			MessageID:     messageID,
			TurnSeq:       i,
			CallsInTurn:   len(names),
			SourcePath:    path,
			DedupKey:      callKey(sessionID, rowID, i),
		})
	}
	return out
}

// callName resolves the name of one content block, reporting whether the block
// is a tool call at all.
//
// Goose's own convention is "<extension>__<tool>", which the provider sometimes
// sees whole and sometimes as a bare tool name with the extension in `_meta`
// (ToolRequest::tool_name_parts). Composing the two halves keeps `shell` from
// the developer extension distinct from `shell` from anything else; when the
// name already carries the prefix it is left exactly as goose wrote it.
func callName(b contentBlock) (string, bool) {
	switch b.Type {
	case "toolRequest", "frontendToolRequest":
	default:
		return "", false
	}
	if b.ToolCall == nil || b.ToolCall.Value == nil {
		return "", false // {"status":"error"}: the model never made a valid call
	}
	name := strings.TrimSpace(b.ToolCall.Value.Name)
	if name == "" {
		return "", false
	}
	ext := ""
	if b.Meta != nil {
		ext = strings.TrimSpace(b.Meta.Extension)
	}
	if ext == "" || strings.HasPrefix(name, ext+"__") {
		return name, true
	}
	return ext + "__" + name, true
}

// callKey is the stable identity of one tool call: the message row it was
// written on plus its position among that message's calls.
//
// The position is an index within the RECORD, never a read position — the
// message row keeps its rowid forever, so the key is the same on every poll.
// The provider's own call id ("call_srht34n0") is deliberately not used: it is
// short and provider-generated, and nothing has counted it for uniqueness
// across a whole install, which is the standard of evidence an id has to meet
// before it becomes a dedup key.
func callKey(sessionID string, rowID int64, idx int) string {
	return ToolID + "|call|" + sessionID + "|" + strconv.FormatInt(rowID, 10) + "|" + strconv.Itoa(idx)
}
