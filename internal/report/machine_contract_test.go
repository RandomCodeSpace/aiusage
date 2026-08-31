package report

import (
	"bytes"
	"testing"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// TestVersion1MachineFixtures pins complete serialized examples. The narrower
// tests in report_test.go explain individual rules; these fixtures catch a
// spelling, order, presence, or formatting change that a structural assertion
// could accidentally overlook.
func TestVersion1MachineFixtures(t *testing.T) {
	events := sampleEvents()
	events[0].Provider = model.ProviderAnthropic
	events[0].ServiceTier = "standard"
	events[0].Raw = `{"input_tokens":100}`
	events[0].SetCost(1234, "embedded-2026-08-09")

	providerSummary := &store.Summary{
		GroupBy: []string{"provider"},
		Buckets: []store.Bucket{{
			Keys:               map[string]string{"provider": ""},
			OrderedKeys:        []string{"provider"},
			Events:             1,
			Sessions:           1,
			Input:              7,
			Output:             3,
			Reasoning:          2,
			Total:              10,
			UnpricedEvents:     1,
			ComputedCostEvents: 0,
		}},
		Totals: store.Bucket{
			Events:         1,
			Sessions:       1,
			Input:          7,
			Output:         3,
			Reasoning:      2,
			Total:          10,
			UnpricedEvents: 1,
		},
	}
	emptyGroupedSummary := &store.Summary{GroupBy: []string{"tool"}}
	emptyUngroupedSummary := &store.Summary{
		GroupBy: []string{},
		Buckets: []store.Bucket{{}},
	}

	fixtures := []struct {
		name  string
		want  string
		write func(*bytes.Buffer) error
	}{
		{
			name: "summary-json-v1",
			want: wantSummaryJSONV1,
			write: func(buf *bytes.Buffer) error {
				return WriteSummaryJSON(buf, providerSummary, nil)
			},
		},
		{
			name: "summary-json-v1-empty-grouped",
			want: wantSummaryJSONV1EmptyGrouped,
			write: func(buf *bytes.Buffer) error {
				return WriteSummaryJSON(buf, emptyGroupedSummary, nil)
			},
		},
		{
			name: "summary-json-v1-empty-ungrouped",
			want: wantSummaryJSONV1EmptyUngrouped,
			write: func(buf *bytes.Buffer) error {
				return WriteSummaryJSON(buf, emptyUngroupedSummary, nil)
			},
		},
		{
			name: "events-json-v1",
			want: wantEventsJSONV1,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsJSON(buf, events)
			},
		},
		{
			name: "events-json-v1-with-raw",
			want: wantEventsJSONV1WithRaw,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsJSONWithRaw(buf, events)
			},
		},
		{
			name: "events-csv-v1",
			want: wantEventsCSVV1,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsCSV(buf, events)
			},
		},
		{
			name: "events-csv-v1-with-raw",
			want: wantEventsCSVV1WithRaw,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsCSVWithRaw(buf, events)
			},
		},
		{
			name: "events-json-v1-empty",
			want: wantEventsJSONV1Empty,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsJSON(buf, nil)
			},
		},
		{
			name: "events-csv-v1-empty",
			want: wantEventsCSVV1Empty,
			write: func(buf *bytes.Buffer) error {
				return WriteEventsCSV(buf, nil)
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := fixture.write(&buf); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			if got := buf.String(); got != fixture.want {
				t.Errorf("machine fixture changed:\n--- want\n%s--- got\n%s", fixture.want, got)
			}
		})
	}
}

const (
	wantSummaryJSONV1EmptyGrouped = `{
  "GroupBy": [
    "tool"
  ],
  "Buckets": [],
  "Totals": {
    "Keys": null,
    "OrderedKeys": null,
    "Events": 0,
    "Sessions": 0,
    "Input": 0,
    "Output": 0,
    "CacheCreation": 0,
    "CacheRead": 0,
    "Reasoning": 0,
    "Total": 0,
    "CostMicroUSD": 0,
    "UnpricedEvents": 0,
    "ComputedCostEvents": 0,
    "DisplayCostMicroUSD": 0,
    "CostApproximate": false,
    "CostKnown": false
  }
}
`
	wantSummaryJSONV1EmptyUngrouped = `{
  "GroupBy": [],
  "Buckets": [
    {
      "Keys": null,
      "OrderedKeys": null,
      "Events": 0,
      "Sessions": 0,
      "Input": 0,
      "Output": 0,
      "CacheCreation": 0,
      "CacheRead": 0,
      "Reasoning": 0,
      "Total": 0,
      "CostMicroUSD": 0,
      "UnpricedEvents": 0,
      "ComputedCostEvents": 0,
      "DisplayCostMicroUSD": 0,
      "CostApproximate": false,
      "CostKnown": false
    }
  ],
  "Totals": {
    "Keys": null,
    "OrderedKeys": null,
    "Events": 0,
    "Sessions": 0,
    "Input": 0,
    "Output": 0,
    "CacheCreation": 0,
    "CacheRead": 0,
    "Reasoning": 0,
    "Total": 0,
    "CostMicroUSD": 0,
    "UnpricedEvents": 0,
    "ComputedCostEvents": 0,
    "DisplayCostMicroUSD": 0,
    "CostApproximate": false,
    "CostKnown": false
  }
}
`
	wantSummaryJSONV1 = `{
  "GroupBy": [
    "provider"
  ],
  "Buckets": [
    {
      "Keys": {
        "provider": ""
      },
      "OrderedKeys": [
        "provider"
      ],
      "Events": 1,
      "Sessions": 1,
      "Input": 7,
      "Output": 3,
      "CacheCreation": 0,
      "CacheRead": 0,
      "Reasoning": 2,
      "Total": 10,
      "CostMicroUSD": 0,
      "UnpricedEvents": 1,
      "ComputedCostEvents": 0,
      "provider_label": "unknown",
      "DisplayCostMicroUSD": 0,
      "CostApproximate": false,
      "CostKnown": false
    }
  ],
  "Totals": {
    "Keys": null,
    "OrderedKeys": null,
    "Events": 1,
    "Sessions": 1,
    "Input": 7,
    "Output": 3,
    "CacheCreation": 0,
    "CacheRead": 0,
    "Reasoning": 2,
    "Total": 10,
    "CostMicroUSD": 0,
    "UnpricedEvents": 1,
    "ComputedCostEvents": 0,
    "DisplayCostMicroUSD": 0,
    "CostApproximate": false,
    "CostKnown": false
  }
}
`
	wantEventsJSONV1 = `[
  {
    "Tool": "claude-code",
    "Model": "claude-3",
    "Provider": "anthropic",
    "ServiceTier": "standard",
    "SessionID": "sess-1",
    "Project": "/home/dev/projects/aiusage",
    "EventTime": "2026-05-29T12:00:00Z",
    "ObservedTime": "2026-05-29T12:05:00Z",
    "InputTokens": 100,
    "OutputTokens": 50,
    "CacheCreationTokens": 10,
    "CacheReadTokens": 20,
    "ReasoningTokens": 5,
    "TotalTokens": 180,
    "CostMicroUSD": 1234,
    "PriceSource": "embedded-2026-08-09",
    "RequestID": "req-1",
    "MessageID": "msg-1",
    "SourcePath": "/tmp/a.jsonl",
    "DedupKey": "claude-code|msg-1",
    "Kind": "usage"
  },
  {
    "Tool": "codex",
    "Model": "gpt-5",
    "Provider": "",
    "ServiceTier": "",
    "SessionID": "",
    "Project": "",
    "EventTime": "2026-05-29T12:00:00Z",
    "ObservedTime": "2026-05-29T12:05:00Z",
    "InputTokens": 7,
    "OutputTokens": 3,
    "CacheCreationTokens": 0,
    "CacheReadTokens": 0,
    "ReasoningTokens": 0,
    "TotalTokens": 10,
    "CostMicroUSD": null,
    "PriceSource": "",
    "RequestID": "",
    "MessageID": "",
    "SourcePath": "",
    "DedupKey": "",
    "Kind": "usage"
  }
]
`
	wantEventsJSONV1WithRaw = `[
  {
    "Tool": "claude-code",
    "Model": "claude-3",
    "Provider": "anthropic",
    "ServiceTier": "standard",
    "SessionID": "sess-1",
    "Project": "/home/dev/projects/aiusage",
    "EventTime": "2026-05-29T12:00:00Z",
    "ObservedTime": "2026-05-29T12:05:00Z",
    "InputTokens": 100,
    "OutputTokens": 50,
    "CacheCreationTokens": 10,
    "CacheReadTokens": 20,
    "ReasoningTokens": 5,
    "TotalTokens": 180,
    "CostMicroUSD": 1234,
    "PriceSource": "embedded-2026-08-09",
    "RequestID": "req-1",
    "MessageID": "msg-1",
    "SourcePath": "/tmp/a.jsonl",
    "DedupKey": "claude-code|msg-1",
    "Kind": "usage",
    "Raw": "{\"input_tokens\":100}"
  },
  {
    "Tool": "codex",
    "Model": "gpt-5",
    "Provider": "",
    "ServiceTier": "",
    "SessionID": "",
    "Project": "",
    "EventTime": "2026-05-29T12:00:00Z",
    "ObservedTime": "2026-05-29T12:05:00Z",
    "InputTokens": 7,
    "OutputTokens": 3,
    "CacheCreationTokens": 0,
    "CacheReadTokens": 0,
    "ReasoningTokens": 0,
    "TotalTokens": 10,
    "CostMicroUSD": null,
    "PriceSource": "",
    "RequestID": "",
    "MessageID": "",
    "SourcePath": "",
    "DedupKey": "",
    "Kind": "usage",
    "Raw": ""
  }
]
`
	wantEventsCSVV1 = `tool,model,session,project,event_time,observed_time,input,output,cache_creation,cache_read,reasoning,total,request_id,message_id,source_path,kind,provider,service_tier,cost_micro_usd,cost_usd,price_source
claude-code,claude-3,sess-1,/home/dev/projects/aiusage,2026-05-29T12:00:00Z,2026-05-29T12:05:00Z,100,50,10,20,5,180,req-1,msg-1,/tmp/a.jsonl,usage,anthropic,standard,1234,0.001234,embedded-2026-08-09
codex,gpt-5,,,2026-05-29T12:00:00Z,2026-05-29T12:05:00Z,7,3,0,0,0,10,,,,usage,,,,,
`
	wantEventsCSVV1WithRaw = `tool,model,session,project,event_time,observed_time,input,output,cache_creation,cache_read,reasoning,total,request_id,message_id,source_path,kind,provider,service_tier,cost_micro_usd,cost_usd,price_source,raw
claude-code,claude-3,sess-1,/home/dev/projects/aiusage,2026-05-29T12:00:00Z,2026-05-29T12:05:00Z,100,50,10,20,5,180,req-1,msg-1,/tmp/a.jsonl,usage,anthropic,standard,1234,0.001234,embedded-2026-08-09,"{""input_tokens"":100}"
codex,gpt-5,,,2026-05-29T12:00:00Z,2026-05-29T12:05:00Z,7,3,0,0,0,10,,,,usage,,,,,,
`
	wantEventsJSONV1Empty = `[]
`
	wantEventsCSVV1Empty = `tool,model,session,project,event_time,observed_time,input,output,cache_creation,cache_read,reasoning,total,request_id,message_id,source_path,kind,provider,service_tier,cost_micro_usd,cost_usd,price_source
`
)
