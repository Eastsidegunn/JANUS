package audit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// DefaultRetryLoopDenyThreshold is K: the minimum number of same-domain
// collector/egress denies after ready, with no model-visible event, before a
// retry-loop warning is raised (T27(c)).
const DefaultRetryLoopDenyThreshold = 4

// RetryLoopSuspicion is one observation-only finding (T27(c), FR-AUD-01/
// FR-COL-03): after subagent/ready the session produced no model-visible event
// (subagent/message, subagent/tool_call) while collector/egress denied the same
// domain at least K times with non-decreasing at_ms gaps that strictly grow at
// least once (a backoff pattern).
// It is a pure projection of the event log: it never stops, limits, or alters
// a session — the spec owner's decision forbids runtime backstops here.
type RetryLoopSuspicion struct {
	Domain   string
	Methods  []string // sorted, distinct
	Denies   int
	FirstGap int64 // ms between the first two denies
	LastGap  int64 // ms between the last two denies
}

// Line renders the single human-facing warning line.
func (s RetryLoopSuspicion) Line() string {
	return fmt.Sprintf("경고: 모델 재시도 루프 의심 — %s %s deny %d건(간격 %s→%s), 모델 가시 이벤트 0",
		safeField(s.Domain), safeField(strings.Join(s.Methods, "/")), s.Denies, seconds(s.FirstGap), seconds(s.LastGap))
}

// DetectRetryLoops evaluates the T27(c) condition over a session event
// snapshot. It depends only on the given events (log order = seq order), so a
// replay of the same log always yields the same result. k <= 1 is treated as
// the default threshold. Results are sorted by domain.
func DetectRetryLoops(events []gen.EventRecord, k int) ([]RetryLoopSuspicion, error) {
	if k <= 1 {
		k = DefaultRetryLoopDenyThreshold
	}
	ordered := append([]gen.EventRecord(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })

	ready := false
	type denyGroup struct {
		atMs    []int64
		methods map[string]bool
	}
	groups := map[string]*denyGroup{}
	for _, event := range ordered {
		switch event.Kind {
		case gen.KindSubagentReady:
			ready = true
		case gen.KindSubagentMessage, gen.KindSubagentToolCall:
			if ready {
				// A model-visible event after ready rules the session out.
				return nil, nil
			}
		case gen.KindCollectorEgress:
			if !ready {
				continue
			}
			var payload gen.EgressPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("audit: collector/egress seq %d: %w", event.Seq, err)
			}
			if payload.Decision != gen.EgressPayloadDecisionDeny || payload.Domain == "" {
				continue
			}
			g := groups[payload.Domain]
			if g == nil {
				g = &denyGroup{methods: map[string]bool{}}
				groups[payload.Domain] = g
			}
			g.atMs = append(g.atMs, payload.AtMs)
			g.methods[payload.Method] = true
		}
	}
	if !ready {
		return nil, nil
	}
	domains := make([]string, 0, len(groups))
	for domain := range groups {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	var out []RetryLoopSuspicion
	for _, domain := range domains {
		g := groups[domain]
		if len(g.atMs) < k {
			continue
		}
		if !backoffGaps(g.atMs) {
			continue
		}
		methods := make([]string, 0, len(g.methods))
		for m := range g.methods {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		n := len(g.atMs)
		out = append(out, RetryLoopSuspicion{
			Domain: domain, Methods: methods, Denies: n,
			FirstGap: g.atMs[1] - g.atMs[0], LastGap: g.atMs[n-1] - g.atMs[n-2],
		})
	}
	return out, nil
}

// RetryLoopWarnings returns the rendered warning lines (empty when none).
func RetryLoopWarnings(events []gen.EventRecord, k int) ([]string, error) {
	found, err := DetectRetryLoops(events, k)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(found))
	for _, s := range found {
		lines = append(lines, s.Line())
	}
	return lines, nil
}

// backoffGaps reports whether the at_ms sequence (in log order) has the
// backoff shape: monotonic, every gap at least the previous one, and at least
// one strict increase (last gap > first gap), so zero-gap bursts and
// fixed-interval polling are not flagged.
// Jitter that shrinks a gap at the backoff cap is a known miss, accepted
// because this is an observation-only warning.
func backoffGaps(atMs []int64) bool {
	prev := int64(-1)
	for i := 1; i < len(atMs); i++ {
		gap := atMs[i] - atMs[i-1]
		if gap < 0 || gap < prev {
			return false
		}
		prev = gap
	}
	n := len(atMs)
	return atMs[n-1]-atMs[n-2] > atMs[1]-atMs[0]
}

func seconds(ms int64) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', -1, 64) + "s"
}
