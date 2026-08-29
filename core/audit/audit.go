// Package audit compares intent-plane tool calls with effect-plane collector
// records. It only depends on the generated contracts; it does not import
// collector, world, seams, or a storage implementation.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

var (
	ErrUnmatchableIntent     = errors.New("audit: intent path를 정규화할 수 없음")
	ErrAmbiguousMatch        = errors.New("audit: intent/effect 매칭이 모호함")
	ErrIncompleteObservation = errors.New("audit: 효과 관측이 불완전함")
)

// Classification is the result of one-to-one intent/effect comparison.
type Classification string

const (
	ReportedUnobserved Classification = "reported_unobserved"
	ObservedUnreported Classification = "observed_unreported"
	Matched            Classification = "matched"
)

// IntentAction is the contracts-only representation of a filesystem claim
// extracted from a tool call. Path is canonical and relative to the workspace.
type IntentAction struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	CallID       string
	Name         string
	Path         string
	ChangeType   gen.FsChangedPayloadChangesItemChangeType
	Seq          int64
}

// EffectAction is one collector fs_changed leaf. The collector event's span
// and seq are retained so a report is reproducible from the event log.
type EffectAction struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Path         string
	ChangeType   gen.FsChangedPayloadChangesItemChangeType
	Hash         string
	Seq          int64
}

// Row is a deterministic report row. A zero seq means that side was absent.
type Row struct {
	Classification Classification
	TraceID        string
	SpanID         string
	ParentSpanID   string
	Path           string
	IntentSeq      int64
	EffectSeq      int64
	CallID         string
	Name           string
	ReportedType   gen.FsChangedPayloadChangesItemChangeType
	ObservedType   gen.FsChangedPayloadChangesItemChangeType
	Reason         string
}

// Report is the pure comparison result. Complete means a successful
// collector/fs_changed event was present and no ambiguity or normalization
// error occurred. Complete does not claim that transient create/delete work
// was observed; upper-based fsdiff only describes final net changes.
type Report struct {
	Rows            []Row
	EffectComplete  bool
	NetChangesKnown bool
}

// CanonicalPath converts an adapter/tool path into a relative POSIX workspace
// path. It never follows symlinks and never case-folds Linux paths.
func CanonicalPath(raw, workspaceTarget string) (string, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return "", ErrUnmatchableIntent
	}
	if strings.Contains(raw, "\\") {
		return "", ErrUnmatchableIntent
	}
	if workspaceTarget == "" || !utf8.ValidString(workspaceTarget) || !strings.HasPrefix(workspaceTarget, "/") {
		return "", fmt.Errorf("%w: workspace target", ErrUnmatchableIntent)
	}
	mount := path.Clean(workspaceTarget)
	if mount == "." || !strings.HasPrefix(mount, "/") {
		return "", fmt.Errorf("%w: workspace target", ErrUnmatchableIntent)
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", ErrUnmatchableIntent
	}
	if strings.HasPrefix(clean, "/") {
		if clean != mount && !strings.HasPrefix(clean, mount+"/") {
			return "", ErrUnmatchableIntent
		}
		clean = strings.TrimPrefix(clean, mount)
		clean = strings.TrimPrefix(clean, "/")
	}
	if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrUnmatchableIntent
	}
	return clean, nil
}

// DecodeEvents extracts filesystem intents and effects from a validated event
// snapshot. The caller supplies the mount target used by the task (normally
// /workspace). A missing fs_changed event is incomplete; an empty changes list
// is a completed scan with no net effects, not proof of no filesystem activity.
func DecodeEvents(events []gen.EventRecord, workspaceTarget string) ([]IntentAction, []EffectAction, Report, error) {
	var intents []IntentAction
	var effects []EffectAction
	report := Report{NetChangesKnown: true}
	var traceID string
	for _, event := range events {
		if event.TraceID == "" {
			return nil, nil, Report{}, fmt.Errorf("audit: trace_id가 비어 있음 (seq %d)", event.Seq)
		}
		if traceID == "" {
			traceID = event.TraceID
		} else if event.TraceID != traceID {
			return nil, nil, Report{}, fmt.Errorf("audit: 복수 trace_id (%s, %s)", traceID, event.TraceID)
		}
		switch event.Kind {
		case gen.KindSubagentToolCall:
			var payload gen.SubagentToolCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, nil, Report{}, fmt.Errorf("audit: subagent tool_call seq %d: %w", event.Seq, err)
			}
			if p, ok := pathArgument(payload.Args); ok && filesystemTool(payload.Name) {
				if event.SpanID == "" || payload.CallID == "" {
					return nil, nil, Report{}, fmt.Errorf("audit: subagent tool_call 식별자 위반 (seq %d)", event.Seq)
				}
				canonical, err := CanonicalPath(p, workspaceTarget)
				if err != nil {
					return nil, nil, Report{}, fmt.Errorf("%w: seq %d", err, event.Seq)
				}
				intents = append(intents, IntentAction{TraceID: event.TraceID, SpanID: event.SpanID, ParentSpanID: parentSpanID(event), CallID: payload.CallID, Name: payload.Name, Path: canonical, ChangeType: changeTypeForTool(payload.Name), Seq: event.Seq})
			}
		case gen.KindToolCall:
			var payload gen.ToolCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, nil, Report{}, fmt.Errorf("audit: tool/call seq %d: %w", event.Seq, err)
			}
			if p, ok := pathArgument(payload.Args); ok && filesystemTool(payload.Name) {
				if event.SpanID == "" {
					return nil, nil, Report{}, fmt.Errorf("audit: tool/call span_id가 비어 있음 (seq %d)", event.Seq)
				}
				canonical, err := CanonicalPath(p, workspaceTarget)
				if err != nil {
					return nil, nil, Report{}, fmt.Errorf("%w: seq %d", err, event.Seq)
				}
				intents = append(intents, IntentAction{TraceID: event.TraceID, SpanID: event.SpanID, ParentSpanID: parentSpanID(event), CallID: fmt.Sprintf("seq-%d", event.Seq), Name: payload.Name, Path: canonical, ChangeType: changeTypeForTool(payload.Name), Seq: event.Seq})
			}
		case gen.KindCollectorFsChanged:
			if event.Actor != "collector" || event.SpanID == "" || event.TraceID == "" {
				return nil, nil, Report{}, fmt.Errorf("audit: collector event envelope 위반 (seq %d)", event.Seq)
			}
			var payload gen.FsChangedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, nil, Report{}, fmt.Errorf("audit: fs_changed seq %d: %w", event.Seq, err)
			}
			report.EffectComplete = true
			if len(payload.Changes) == 0 {
				report.NetChangesKnown = false
			}
			for _, change := range payload.Changes {
				canonical, err := canonicalEffectPath(change.Path)
				if err != nil {
					return nil, nil, Report{}, fmt.Errorf("audit: effect path seq %d: %w", event.Seq, err)
				}
				effects = append(effects, EffectAction{TraceID: event.TraceID, SpanID: event.SpanID, ParentSpanID: parentSpanID(event), Path: canonical, ChangeType: change.ChangeType, Hash: change.Hash, Seq: event.Seq})
			}
		}
	}
	if !report.EffectComplete {
		return intents, effects, report, ErrIncompleteObservation
	}
	rows, err := Match(intents, effects, report.NetChangesKnown)
	if err != nil {
		return nil, nil, Report{}, err
	}
	report.Rows = rows
	return intents, effects, report, nil
}

func parentSpanID(event gen.EventRecord) string {
	if event.ParentSpanID == nil {
		return ""
	}
	return *event.ParentSpanID
}

func canonicalEffectPath(raw string) (string, error) {
	// collector/fs_changed paths are already relative POSIX paths. An absolute
	// path is malformed rather than another spelling of /workspace/foo; treating
	// it as relative would allow an intent/effect false match.
	if strings.HasPrefix(raw, "/") {
		return "", ErrUnmatchableIntent
	}
	return CanonicalPath(raw, "/workspace")
}

func pathArgument(raw json.RawMessage) (string, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "", false
	}
	for _, key := range []string{"path", "file_path", "filename"} {
		value, ok := object[key]
		if !ok {
			continue
		}
		var pathValue string
		if json.Unmarshal(value, &pathValue) == nil {
			return pathValue, true
		}
	}
	return "", false
}

func filesystemTool(name string) bool {
	switch strings.ToLower(name) {
	case "write", "create", "touch", "write_file", "edit", "modify", "update", "delete", "remove", "rm":
		return true
	default:
		return false
	}
}

func changeTypeForTool(name string) gen.FsChangedPayloadChangesItemChangeType {
	switch strings.ToLower(name) {
	case "delete", "remove", "rm":
		return gen.FsChangedPayloadChangesItemChangeTypeDeleted
	case "edit", "modify", "update":
		return gen.FsChangedPayloadChangesItemChangeTypeModified
	default:
		return gen.FsChangedPayloadChangesItemChangeTypeAdded
	}
}

// Match performs deterministic one-to-one matching. It rejects duplicate
// identity rows instead of silently consuming one side twice. Equal path/span
// groups are ordered by seq, making repeated operations reproducible.
func Match(intents []IntentAction, effects []EffectAction, netChangesKnown bool) ([]Row, error) {
	if !netChangesKnown && len(intents) > 0 {
		return nil, ErrIncompleteObservation
	}
	key := func(span, p string) string { return span + "\x00" + p }
	seenIntent := map[string]bool{}
	seenEffect := map[string]bool{}
	groupsI := map[string][]IntentAction{}
	groupsE := map[string][]EffectAction{}
	for _, item := range intents {
		identity := fmt.Sprintf("%s\x00%d", key(item.SpanID, item.Path), item.Seq)
		if seenIntent[identity] {
			return nil, fmt.Errorf("%w: intent seq %d", ErrAmbiguousMatch, item.Seq)
		}
		seenIntent[identity] = true
		groupsI[key(item.SpanID, item.Path)] = append(groupsI[key(item.SpanID, item.Path)], item)
	}
	for _, item := range effects {
		identity := fmt.Sprintf("%s\x00%d", key(item.SpanID, item.Path), item.Seq)
		if seenEffect[identity] {
			return nil, fmt.Errorf("%w: effect seq %d", ErrAmbiguousMatch, item.Seq)
		}
		seenEffect[identity] = true
		groupsE[key(item.SpanID, item.Path)] = append(groupsE[key(item.SpanID, item.Path)], item)
	}
	keys := make([]string, 0, len(groupsI)+len(groupsE))
	seenKeys := map[string]bool{}
	for k := range groupsI {
		seenKeys[k] = true
		keys = append(keys, k)
	}
	for k := range groupsE {
		if !seenKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var rows []Row
	for _, k := range keys {
		is, es := groupsI[k], groupsE[k]
		sort.Slice(is, func(i, j int) bool { return is[i].Seq < is[j].Seq })
		sort.Slice(es, func(i, j int) bool { return es[i].Seq < es[j].Seq })
		n := len(is)
		if len(es) > n {
			n = len(es)
		}
		for i := 0; i < n; i++ {
			if i < len(is) && i < len(es) && compatible(is[i].ChangeType, es[i].ChangeType) {
				rows = append(rows, Row{
					Classification: Matched, TraceID: is[i].TraceID, SpanID: is[i].SpanID, ParentSpanID: firstNonEmpty(is[i].ParentSpanID, es[i].ParentSpanID), Path: is[i].Path,
					IntentSeq: is[i].Seq, EffectSeq: es[i].Seq, CallID: is[i].CallID, Name: is[i].Name,
					ReportedType: is[i].ChangeType, ObservedType: es[i].ChangeType,
				})
				continue
			}
			if i < len(is) {
				row := Row{Classification: ReportedUnobserved, TraceID: is[i].TraceID, SpanID: is[i].SpanID, ParentSpanID: is[i].ParentSpanID, Path: is[i].Path, IntentSeq: is[i].Seq, CallID: is[i].CallID, Name: is[i].Name, ReportedType: is[i].ChangeType, Reason: "effect not observed"}
				rows = append(rows, row)
			}
			if i < len(es) {
				row := Row{Classification: ObservedUnreported, TraceID: es[i].TraceID, SpanID: es[i].SpanID, ParentSpanID: es[i].ParentSpanID, Path: es[i].Path, EffectSeq: es[i].Seq, ObservedType: es[i].ChangeType, Reason: "intent not reported"}
				if i < len(is) {
					row.Reason = "reported change incompatible with observed change"
				}
				rows = append(rows, row)
			}
		}
	}
	// rows inherit the deterministic key order above and seq order within each
	// group; a second global sort would make the key-order guard unobservable.
	return rows, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func compatible(reported, observed gen.FsChangedPayloadChangesItemChangeType) bool {
	if reported == gen.FsChangedPayloadChangesItemChangeTypeDeleted {
		return observed == gen.FsChangedPayloadChangesItemChangeTypeDeleted
	}
	return observed == gen.FsChangedPayloadChangesItemChangeTypeAdded || observed == gen.FsChangedPayloadChangesItemChangeTypeModified
}
