package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

const (
	longLedgerProfile       = "long-ledger-v1"
	longLedgerSeed          = int64(0x5eed82)
	longLedgerUsageEvents   = 1_000_000
	longLedgerActivityRows  = 250_000
	longLedgerTurnContexts  = 100_000
	fixtureBatchSize        = 4096
	fixtureEventsPerSession = 200
	fixtureRollupCheckRows  = 20_000
)

var longLedgerClock = time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

var fixtureTools = []string{
	model.ToolClaudeCode, model.ToolCodex, model.ToolCopilot, model.ToolOpenCode,
	model.ToolHermes, model.ToolAgy, model.ToolCline, model.ToolCrush,
	model.ToolDSH, model.ToolGoose, model.ToolKimiCode, model.ToolPi,
	model.ToolOpenClaw, model.ToolQwenCode, model.ToolReasonix,
}

var fixtureProviders = []string{
	"", model.ProviderAnthropic, model.ProviderOpenAI, model.ProviderGoogle,
	model.ProviderGitHub, "azure", "aws-bedrock", "vertex", "openrouter",
}

var fixtureTiers = []string{"", "standard", "priority"}

var fixtureTurnDimensions = model.TurnDimensions()

var fixtureRecentBoundaries = [...]time.Time{
	time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	time.Date(2026, 8, 30, 12, 14, 59, 0, time.UTC),
	time.Date(2026, 8, 30, 12, 15, 0, 0, time.UTC),
	time.Date(2026, 8, 30, 12, 59, 59, 0, time.UTC),
	time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC),
	time.Date(2026, 8, 30, 18, 29, 59, 0, time.UTC), // Kolkata local midnight - 1s
	time.Date(2026, 8, 30, 18, 30, 0, 0, time.UTC),
}

var fixtureHistoricalBoundaries = [...]time.Time{
	time.Date(2025, 11, 2, 5, 59, 59, 0, time.UTC), // New York DST fold
	time.Date(2025, 11, 2, 6, 0, 0, 0, time.UTC),
	time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC), // New York DST gap
	time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
}

type longLedgerManifest struct {
	Profile          string    `json:"profile"`
	Seed             int64     `json:"seed"`
	Clock            time.Time `json:"clock"`
	SchemaVersion    int       `json:"schema_version"`
	UsageEvents      int       `json:"usage_events"`
	RecentUsage      int       `json:"recent_usage_events"`
	ActivityRows     int       `json:"activity_rows"`
	RecentActivity   int       `json:"recent_activity_rows"`
	UnattributedRows int       `json:"unattributed_activity_rows"`
	TurnContexts     int       `json:"turn_context_rows"`
	RecentContexts   int       `json:"recent_turn_context_rows"`
	Tools            int       `json:"tools"`
	Models           int       `json:"models"`
	Projects         int       `json:"projects"`
	Sessions         int       `json:"sessions"`
	Providers        int       `json:"providers"`
	ServiceTiers     int       `json:"service_tiers"`
	UnpricedEvents   int       `json:"unpriced_events"`
	RawEvents        int       `json:"raw_events"`
	RawBytesPerEvent int       `json:"raw_bytes_per_event"`
	RollupRows       int64     `json:"rollup_rows"`
	EventsPerRollup  float64   `json:"usage_events_per_rollup_row"`
	DatabaseBytes    int64     `json:"database_bytes"`
	DatabaseSHA256   string    `json:"database_sha256"`
	BoundaryInstants []string  `json:"boundary_instants"`
}

func generateLongLedger(dbPath, manifestPath string, usageCount, activityCount, contextCount int) error {
	if dbPath == "" || manifestPath == "" {
		return fmt.Errorf("generate-long-ledger requires --db and --manifest")
	}
	if usageCount <= 0 || activityCount < 0 || contextCount < 0 {
		return fmt.Errorf("fixture counts must be positive usage and non-negative activity/context")
	}
	if activityCount > usageCount || contextCount > usageCount {
		return fmt.Errorf("activity/context counts cannot exceed usage count")
	}
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("refusing to replace existing fixture %s", dbPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect fixture path %s: %w", dbPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("create fixture directory: %w", err)
	}

	ledger, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open fixture store: %w", err)
	}
	ctx := context.Background()
	raw := fixtureRawPayload()
	unpriced := 0
	rawEvents := 0
	recentUsage := 0
	recentActivity := 0
	recentContexts := 0
	for start := 0; start < usageCount; start += fixtureBatchSize {
		end := min(start+fixtureBatchSize, usageCount)
		batch := make([]model.UsageEvent, 0, end-start)
		for i := start; i < end; i++ {
			e := fixtureUsageEvent(i, usageCount, raw)
			if fixtureIsRecent(e.EventTime) {
				recentUsage++
			}
			if _, ok := e.Cost(); !ok {
				unpriced++
			}
			if e.Raw != "" {
				rawEvents++
			}
			batch = append(batch, e)
		}
		n, insertErr := ledger.InsertEvents(ctx, batch)
		if insertErr != nil || n != len(batch) {
			ledger.Close()
			return fmt.Errorf("insert usage [%d,%d): inserted=%d: %w", start, end, n, insertErr)
		}
		progress("usage", end, usageCount)
	}

	for start := 0; start < max(activityCount, contextCount); start += fixtureBatchSize {
		endActivity := min(start+fixtureBatchSize, activityCount)
		endContexts := min(start+fixtureBatchSize, contextCount)
		batch := store.ObservationBatch{}
		if start < activityCount {
			batch.Activity = make([]model.ActivityEvent, 0, endActivity-start)
			for i := start; i < endActivity; i++ {
				e := fixtureActivityEvent(i, activityCount, usageCount)
				if fixtureIsRecent(e.EventTime) {
					recentActivity++
				}
				batch.Activity = append(batch.Activity, e)
			}
		}
		if start < contextCount {
			batch.TurnContexts = make([]model.TurnContext, 0, endContexts-start)
			for i := start; i < endContexts; i++ {
				turnContext := fixtureTurnContext(i, contextCount, usageCount)
				if fixtureIsRecent(turnContext.EventTime) {
					recentContexts++
				}
				batch.TurnContexts = append(batch.TurnContexts, turnContext)
			}
		}
		applied, applyErr := ledger.ApplyBatch(ctx, batch)
		if applyErr != nil || applied.Activity != len(batch.Activity) || applied.TurnContexts != len(batch.TurnContexts) {
			ledger.Close()
			return fmt.Errorf("apply derived fixture rows at %d: applied=%+v: %w", start, applied, applyErr)
		}
		if start < activityCount {
			progress("activity", endActivity, activityCount)
		}
		if start < contextCount {
			progress("contexts", endContexts, contextCount)
		}
	}
	if err := ledger.RebuildRollup(ctx); err != nil {
		ledger.Close()
		return fmt.Errorf("rebuild fixture rollup: %w", err)
	}
	if err := ledger.Close(); err != nil {
		return fmt.Errorf("close fixture store: %w", err)
	}
	rollupRows, err := fixtureRollupRows(ctx, dbPath)
	if err != nil {
		return err
	}
	if err := validateFixtureRollup(usageCount, rollupRows, store.SchemaVersion); err != nil {
		return err
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		return fmt.Errorf("stat fixture: %w", err)
	}
	digest, err := fileSHA256(dbPath)
	if err != nil {
		return err
	}
	sessions := min(5000, (usageCount+fixtureEventsPerSession-1)/fixtureEventsPerSession)
	manifest := longLedgerManifest{
		Profile:          longLedgerProfile,
		Seed:             longLedgerSeed,
		Clock:            longLedgerClock,
		SchemaVersion:    store.SchemaVersion,
		UsageEvents:      usageCount,
		RecentUsage:      recentUsage,
		ActivityRows:     activityCount,
		RecentActivity:   recentActivity,
		UnattributedRows: activityCount / 2,
		TurnContexts:     contextCount,
		RecentContexts:   recentContexts,
		Tools:            min(len(fixtureTools), sessions),
		Models:           min(32, sessions),
		Projects:         min(256, sessions),
		Sessions:         sessions,
		Providers:        min(len(fixtureProviders), sessions),
		ServiceTiers:     min(len(fixtureTiers), sessions),
		UnpricedEvents:   unpriced,
		RawEvents:        rawEvents,
		RawBytesPerEvent: len(raw),
		RollupRows:       rollupRows,
		EventsPerRollup:  float64(usageCount) / float64(rollupRows),
		DatabaseBytes:    info.Size(),
		DatabaseSHA256:   digest,
		BoundaryInstants: fixtureBoundaryStrings(),
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}

func fixtureUsageEvent(i, count int, raw string) model.UsageEvent {
	at := fixtureEventTime(i, count)
	identity := fixtureUsageIdentity(i)
	input := int64(100 + i%10_000)
	output := int64(20 + i%2_000)
	cacheCreate := int64(i % 97)
	cacheRead := int64(i % 389)
	reasoning := int64(i % 43)
	e := model.UsageEvent{
		Tool:                identity.tool,
		Model:               identity.model,
		Provider:            identity.provider,
		ServiceTier:         identity.serviceTier,
		SessionID:           identity.session,
		Project:             identity.project,
		EventTime:           at,
		ObservedTime:        longLedgerClock.Add(time.Duration(i%3600) * time.Second),
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheCreate,
		CacheReadTokens:     cacheRead,
		ReasoningTokens:     reasoning,
		TotalTokens:         input + output + cacheCreate + cacheRead,
		RequestID:           fmt.Sprintf("request-%07d", i),
		MessageID:           fmt.Sprintf("message-%07d", i),
		SourcePath:          fmt.Sprintf("/fixture/source-%04d.jsonl", i%2500),
		DedupKey:            fixtureUsageKey(i),
		Kind:                model.KindUsage,
	}
	if i%100 != 0 {
		source := "embedded-perf-v1"
		if i%2 == 0 {
			source = "goose-provider_reported"
		}
		e.SetCost(int64(10_000+i%1_000_000), source)
	}
	if i%10 == 0 {
		e.Raw = raw
	}
	return e
}

func fixtureActivityEvent(i, activityCount, usageCount int) model.ActivityEvent {
	linked := i%2 == 0
	usageKey := ""
	seq, calls := 0, 1
	usageIndex := scaledFixtureIndex(i, activityCount, usageCount)
	if linked {
		linkedIndex := i / 2
		turns := ((activityCount + 1) / 2) + 1
		turns /= 2
		usageIndex = scaledFixtureIndex(linkedIndex/2, turns, usageCount)
		usageKey = fixtureUsageKey(usageIndex)
		seq, calls = linkedIndex%2, 2
	}
	identity := fixtureUsageIdentity(usageIndex)
	at := fixtureEventTime(usageIndex, usageCount)
	kinds := []model.ActivityKind{model.ActivityTool, model.ActivitySkill, model.ActivityHook}
	return model.ActivityEvent{
		Tool:          identity.tool,
		Kind:          kinds[i%len(kinds)],
		Name:          fmt.Sprintf("activity-%03d", i%128),
		SessionID:     identity.session,
		Project:       identity.project,
		Model:         identity.model,
		EventTime:     at,
		ObservedTime:  longLedgerClock.Add(time.Duration(i%3600) * time.Second),
		UsageDedupKey: usageKey,
		MessageID:     fmt.Sprintf("activity-message-%07d", i),
		RequestID:     fmt.Sprintf("activity-request-%07d", i),
		TurnSeq:       seq,
		CallsInTurn:   calls,
		SourcePath:    fmt.Sprintf("/fixture/source-%04d.jsonl", i%2500),
		DedupKey:      fmt.Sprintf("activity-row-%07d", i),
	}
}

func fixtureTurnContext(i, contextCount, usageCount int) model.TurnContext {
	usageIndex := scaledFixtureIndex(i, contextCount, usageCount)
	identity := fixtureUsageIdentity(usageIndex)
	at := fixtureEventTime(usageIndex, usageCount)
	return model.TurnContext{
		UsageDedupKey: fixtureUsageKey(usageIndex),
		Tool:          identity.tool,
		Dimension:     fixtureTurnDimensions[i%len(fixtureTurnDimensions)],
		Value:         fmt.Sprintf("context-%03d", i%128),
		SessionID:     identity.session,
		Project:       identity.project,
		Model:         identity.model,
		EventTime:     at,
		ObservedTime:  longLedgerClock.Add(time.Duration(i%3600) * time.Second),
		SourcePath:    fmt.Sprintf("/fixture/source-%04d.jsonl", i%2500),
	}
}

func fixtureUsageKey(i int) string { return fmt.Sprintf("usage-row-%07d", i) }

type fixtureIdentity struct {
	tool        string
	model       string
	provider    string
	serviceTier string
	session     string
	project     string
}

func fixtureUsageIdentity(i int) fixtureIdentity {
	session := (i / fixtureEventsPerSession) % 5000
	return fixtureIdentity{
		tool:        fixtureTools[session%len(fixtureTools)],
		model:       fmt.Sprintf("model-%02d", session%32),
		provider:    fixtureProviders[session%len(fixtureProviders)],
		serviceTier: fixtureTiers[session%len(fixtureTiers)],
		session:     fmt.Sprintf("session-%04d", session),
		project:     fmt.Sprintf("/fixture/project-%03d", session%256),
	}
}

func scaledFixtureIndex(i, count, usageCount int) int {
	if count <= 0 || usageCount <= 0 {
		return 0
	}
	return i * usageCount / count
}

func fixtureEventTime(i, count int) time.Time {
	if i < len(fixtureHistoricalBoundaries) {
		return fixtureHistoricalBoundaries[i]
	}
	half := count / 2
	if i >= half && i-half < len(fixtureRecentBoundaries) {
		return fixtureRecentBoundaries[i-half]
	}
	start := longLedgerClock.AddDate(-1, 0, 0)
	end := longLedgerClock.AddDate(0, 0, -30)
	eraIndex := i
	eraCount := half
	if i >= half {
		start = end
		end = longLedgerClock
		eraIndex = i - half
		eraCount = count - half
	}
	session := eraIndex / fixtureEventsPerSession
	sessions := max(1, (eraCount+fixtureEventsPerSession-1)/fixtureEventsPerSession)
	spanSeconds := int64(end.Sub(start) / time.Second)
	anchor := start.Add(time.Duration(spanSeconds*int64(session)/int64(sessions)) * time.Second)
	anchor = time.Unix(anchor.Unix()-anchor.Unix()%900, 0).UTC()
	return anchor.Add(time.Duration(eraIndex%fixtureEventsPerSession) * time.Second)
}

func fixtureBoundaryTimes() []time.Time {
	out := make([]time.Time, 0, len(fixtureRecentBoundaries)+len(fixtureHistoricalBoundaries))
	out = append(out, fixtureRecentBoundaries[:]...)
	out = append(out, fixtureHistoricalBoundaries[:]...)
	return out
}

func fixtureIsRecent(at time.Time) bool {
	return !at.Before(longLedgerClock.AddDate(0, 0, -30)) && at.Before(longLedgerClock)
}

func fixtureRollupRows(ctx context.Context, path string) (int64, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, fmt.Errorf("open fixture rollup: %w", err)
	}
	defer db.Close()
	var rows int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_rollup`).Scan(&rows); err != nil {
		return 0, fmt.Errorf("count fixture rollup: %w", err)
	}
	return rows, nil
}

func validateFixtureRollup(usageCount int, rollupRows int64, schemaVersion int) error {
	if schemaVersion < 8 || usageCount < fixtureRollupCheckRows {
		return nil
	}
	usage := int64(usageCount)
	if rollupRows*100 < usage || rollupRows*100 > usage*2 {
		return fmt.Errorf("fixture rollup has %d rows for %d events; want 1-2%% representative density", rollupRows, usageCount)
	}
	return nil
}

func fixtureBoundaryStrings() []string {
	times := fixtureBoundaryTimes()
	out := make([]string, len(times))
	for i := range times {
		out[i] = times[i].Format(time.RFC3339)
	}
	return out
}

func fixtureRawPayload() string {
	const target = 400
	prefix := `{"usage":{"input_tokens":1},"audit":"`
	suffix := `"}`
	return prefix + strings.Repeat("r", target-len(prefix)-len(suffix)) + suffix
}

func progress(kind string, done, total int) {
	if done == total || done%100_000 < fixtureBatchSize {
		fmt.Fprintf(os.Stderr, "%s: %d/%d\n", kind, done, total)
	}
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	encErr := json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if encErr != nil {
		return fmt.Errorf("encode %s: %w", path, encErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open fixture for digest: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash fixture: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
