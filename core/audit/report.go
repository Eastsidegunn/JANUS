package audit

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ObservationStatus is deliberately separate from the row classifications:
// an incomplete collector observation is evidence failure, not a fourth kind
// of successful audit result.
type ObservationStatus string

const (
	ObservationComplete   ObservationStatus = "complete"
	ObservationIncomplete ObservationStatus = "incomplete"
)

// Summary is derived solely from the report rows. It is a value type so a
// caller cannot mutate renderer state or make the totals disagree with rows.
type Summary struct {
	Total              int
	Matched            int
	ReportedUnobserved int
	ObservedUnreported int
}

// UsageTotals is the contracts-independent cost value passed by the surface
// from logd.Replay. Keeping this small type here preserves audit's boundary.
type UsageTotals struct {
	In  int64
	Out int64
}

// ContextEntry is a normalized parent-visible message. It deliberately carries
// a summary rather than an event payload, so renderer output never reproduces
// raw tool arguments or credentials.
type ContextEntry struct {
	Seq     int64
	Role    string
	SpanID  string
	Summary string
}

// RenderOptions contains optional query projections assembled by surfaces.
// Maps are copied into deterministic key order by the renderer.
type RenderOptions struct {
	IncludeCost  bool
	UsageIn      int64
	UsageOut     int64
	UsageByActor map[string]UsageTotals
	AtSeqContext []ContextEntry
}

// Summary computes deterministic classification totals from a report.
func (r Report) Summary() Summary {
	var summary Summary
	for _, row := range r.Rows {
		summary.Total++
		switch row.Classification {
		case Matched:
			summary.Matched++
		case ReportedUnobserved:
			summary.ReportedUnobserved++
		case ObservedUnreported:
			summary.ObservedUnreported++
		}
	}
	return summary
}

// Observation reports whether the effect plane is complete enough for a
// successful comparison. Empty changes are intentionally incomplete because
// they do not prove that no transient effect occurred.
func (r Report) Observation() ObservationStatus {
	if r.EffectComplete && r.NetChangesKnown {
		return ObservationComplete
	}
	return ObservationIncomplete
}

// Render emits the stable, human-readable audit table. It returns no bytes on
// incomplete observation: callers must not accidentally print a successful
// table when the effect plane was absent, truncated, or otherwise incomplete.
// Fields are tab-separated and control characters are escaped; raw payloads,
// call arguments, and credentials are never part of a rendered row.
func Render(r Report) ([]byte, error) {
	return RenderWithOptions(r, RenderOptions{})
}

// RenderWithOptions emits the report and optional cost/context projections.
func RenderWithOptions(r Report, opts RenderOptions) ([]byte, error) {
	if r.Observation() != ObservationComplete {
		return nil, fmt.Errorf("%w: effect_observation=%s", ErrIncompleteObservation, r.Observation())
	}

	rows := append([]Row(nil), r.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rowLess(rows[i], rows[j]) })
	summary := r.Summary()
	var out bytes.Buffer
	fmt.Fprintln(&out, "effect_observation: complete")
	fmt.Fprintln(&out, "classification\tspan_id\tparent_span_id\tpath\treported_change\tobserved_change\tintent_seq\teffect_seq\treason")
	for _, row := range rows {
		fmt.Fprintf(&out, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			safeField(string(row.Classification)),
			safeField(row.SpanID),
			safeField(row.ParentSpanID),
			safeField(row.Path),
			safeField(string(row.ReportedType)),
			safeField(string(row.ObservedType)),
			row.IntentSeq,
			row.EffectSeq,
			safeField(row.Reason),
		)
	}
	fmt.Fprintln(&out, "summary")
	fmt.Fprintf(&out, "total\t%d\nmatched\t%d\nreported_unobserved\t%d\nobserved_unreported\t%d\n",
		summary.Total, summary.Matched, summary.ReportedUnobserved, summary.ObservedUnreported)
	if opts.IncludeCost {
		fmt.Fprintf(&out, "usage_in\t%d\nusage_out\t%d\n", opts.UsageIn, opts.UsageOut)
		actors := make([]string, 0, len(opts.UsageByActor))
		for actor := range opts.UsageByActor {
			actors = append(actors, actor)
		}
		sort.Strings(actors)
		fmt.Fprintln(&out, "usage_by_actor")
		for _, actor := range actors {
			u := opts.UsageByActor[actor]
			fmt.Fprintf(&out, "%s\t%d\t%d\n", safeField(actor), u.In, u.Out)
		}
	}
	if opts.AtSeqContext != nil {
		fmt.Fprintln(&out, "parent_context")
		contextEntries := append([]ContextEntry(nil), opts.AtSeqContext...)
		sort.SliceStable(contextEntries, func(i, j int) bool {
			if contextEntries[i].Seq != contextEntries[j].Seq {
				return contextEntries[i].Seq < contextEntries[j].Seq
			}
			return contextEntries[i].Role < contextEntries[j].Role
		})
		for _, entry := range contextEntries {
			fmt.Fprintf(&out, "%d\t%s\t%s\t%s\n", entry.Seq, safeField(entry.Role), safeField(entry.SpanID), safeField(entry.Summary))
		}
	}
	return out.Bytes(), nil
}

// RenderReport is the descriptive alias used by surfaces that want to make
// the value-to-bytes boundary explicit.
func RenderReport(r Report) ([]byte, error) { return Render(r) }

func rowLess(a, b Row) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.SpanID != b.SpanID {
		return a.SpanID < b.SpanID
	}
	if a.ParentSpanID != b.ParentSpanID {
		return a.ParentSpanID < b.ParentSpanID
	}
	if a.IntentSeq != b.IntentSeq {
		return a.IntentSeq < b.IntentSeq
	}
	if a.EffectSeq != b.EffectSeq {
		return a.EffectSeq < b.EffectSeq
	}
	if a.Classification != b.Classification {
		return a.Classification < b.Classification
	}
	if a.ReportedType != b.ReportedType {
		return a.ReportedType < b.ReportedType
	}
	if a.ObservedType != b.ObservedType {
		return a.ObservedType < b.ObservedType
	}
	return a.Reason < b.Reason
}

func safeField(value string) string {
	// strconv.Quote gives a deterministic, unambiguous representation for
	// control characters without reproducing arbitrary raw diagnostic text.
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return strconv.Quote(value)
	}
	return value
}
