package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

type workflowSQLStoreCandidate struct {
	info workflowStoreInfo
	path string
}

type workflowSQLTableSet struct {
	beads  string
	labels string
	deps   string
}

var (
	workflowSQLIssueTables = workflowSQLTableSet{beads: "issues", labels: "labels", deps: "dependencies"}
	workflowSQLWispTables  = workflowSQLTableSet{beads: "wisps", labels: "wisp_labels", deps: "wisp_dependencies"}
)

// workflowSQLQuerier resolves the bd SQL capability of a workflow store. bd owns
// the dolt endpoint in proxied-server mode, so the fast path issues a few bulk
// reads through `bd sql --json` (the store's own runner) instead of opening a
// direct connection. A store whose backing cannot serve SQL (file/postgres, or
// a nil store) yields ok == false and the caller falls back to the slow
// Store-interface path.
func workflowSQLQuerier(store beads.Store) (beads.SQLQuerier, bool) {
	if store == nil {
		return nil, false
	}
	q, ok := store.(beads.SQLQuerier)
	return q, ok
}

// workflowSQLQueryRows runs one SQL statement through bd and decodes the
// `--json` result into a slice of T. An empty result decodes to nil.
func workflowSQLQueryRows[T any](q beads.SQLQuerier, query string) ([]T, error) {
	out, err := q.QueryJSON(query)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var rows []T
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parse bd sql json: %w", err)
	}
	return rows, nil
}

// sqlQuote renders a string as a safe single-quoted SQL literal. bd sql takes a
// literal statement (no bound parameters), so every interpolated value — bead
// ids, workflow ids — is escaped here; the only other interpolations are
// table/column names drawn from fixed allow-lists in this file.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// workflowSQLBeadColumns is the aliased projection shared by every bead read, so
// the `--json` keys are stable regardless of how dolt labels qualified columns.
const workflowSQLBeadColumns = `i.id AS id, i.title AS title, i.status AS status, ` +
	`i.issue_type AS issue_type, i.assignee AS assignee, i.description AS description, ` +
	`i.created_at AS created_at, i.updated_at AS updated_at, i.metadata AS metadata`

type workflowSQLBeadRow struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Status      string          `json:"status"`
	IssueType   string          `json:"issue_type"`
	Assignee    *string         `json:"assignee"`
	Description *string         `json:"description"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Metadata    json.RawMessage `json:"metadata"`
}

type workflowSQLDepRow struct {
	IssueID   string `json:"issue_id"`
	DependsOn string `json:"depends_on"`
	DepType   string `json:"dep_type"`
}

type workflowSQLLabelRow struct {
	IssueID string `json:"issue_id"`
	Label   string `json:"label"`
}

var workflowSQLTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// workflowSQLParseTime parses a dolt datetime string from bd sql --json. The
// value feeds only stable sort ordering, so an unparseable timestamp degrades
// to the zero time rather than failing the snapshot.
func workflowSQLParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range workflowSQLTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// workflowSQLDecodeMetadata converts a bd sql --json metadata value into the
// bead metadata map. The dolt JSON column may arrive as an embedded object or
// as a JSON-encoded string; both decode to the same map, with non-string values
// re-encoded to their JSON text (matching the pre-bd direct-SQL behavior).
func workflowSQLDecodeMetadata(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	inner := raw
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		inner = []byte(asString)
	}
	var obj map[string]any
	if json.Unmarshal(inner, &obj) != nil || len(obj) == 0 {
		return nil
	}
	md := make(map[string]string, len(obj))
	for k, v := range obj {
		if s, ok := v.(string); ok {
			md[k] = s
		} else if encoded, err := json.Marshal(v); err == nil {
			md[k] = string(encoded)
		}
	}
	return md
}

func workflowSQLBeadFromRow(r workflowSQLBeadRow) beads.Bead {
	b := beads.Bead{
		ID:        r.ID,
		Title:     r.Title,
		Status:    r.Status,
		Type:      r.IssueType,
		CreatedAt: workflowSQLParseTime(r.CreatedAt),
		UpdatedAt: workflowSQLParseTime(r.UpdatedAt),
	}
	if r.Assignee != nil {
		b.Assignee = *r.Assignee
	}
	if r.Description != nil {
		b.Description = *r.Description
	}
	b.Metadata = workflowSQLDecodeMetadata(r.Metadata)
	return b
}

func workflowSQLCandidatesForWorkflowID(
	state State,
	workflowID, requestedScopeKind, requestedScopeRef string,
) []workflowSQLStoreCandidate {
	requestedScopeKind = strings.TrimSpace(requestedScopeKind)
	requestedScopeRef = strings.TrimSpace(requestedScopeRef)
	if requestedScopeKind != "" && requestedScopeRef != "" {
		return workflowSQLStoreCandidates(state, requestedScopeKind, requestedScopeRef)
	}

	if prefix := workflowSQLWorkflowIDPrefix(state.Config(), workflowID); prefix != "" {
		if candidate, ok := workflowSQLRouteCandidate(state, prefix); ok {
			return []workflowSQLStoreCandidate{candidate}
		}
		return nil
	}

	return workflowSQLStoreCandidates(state, "", "")
}

// workflowSQLSnapshot fetches all workflow beads and deps through bd's proxied
// server, bypassing the N+1 bd subprocess calls with a few bulk `bd sql --json`
// reads. Returns beads, a bead index, and a pre-fetched dep map.
func workflowSQLSnapshot(q beads.SQLQuerier, rootID string) ([]beads.Bead, map[string]beads.Bead, map[string][]beads.Dep, error) {
	tableSets, err := workflowSQLAvailableTableSets(q)
	if err != nil {
		return nil, nil, nil, err
	}

	workflowBeads, beadIndex, err := workflowSQLQueryWorkflowBeads(q, tableSets, rootID)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(workflowBeads) == 0 {
		return nil, nil, nil, fmt.Errorf("no beads found for workflow %s", rootID)
	}

	depMap, err := workflowSQLQueryWorkflowDeps(q, tableSets, rootID)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := workflowSQLHydrateWorkflowLabels(q, tableSets, rootID, workflowBeads, beadIndex); err != nil {
		return workflowBeads, beadIndex, depMap, nil
	}

	return workflowBeads, beadIndex, depMap, nil
}

func workflowSQLQueryWorkflowBeads(q beads.SQLQuerier, tableSets []workflowSQLTableSet, rootID string) ([]beads.Bead, map[string]beads.Bead, error) {
	workflowBeads := make([]beads.Bead, 0, 100)
	beadIndex := make(map[string]beads.Bead)
	quoted := sqlQuote(rootID)
	rootPath := beadmeta.JSONPath(beadmeta.RootBeadIDMetadataKey)
	for _, tables := range tableSets {
		query := `SELECT ` + workflowSQLBeadColumns + `
			FROM ` + tables.beads + ` i
			WHERE i.id = ` + quoted + `
			   OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '` + rootPath + `')) = ` + quoted + `
			ORDER BY i.created_at`
		rows, err := workflowSQLQueryRows[workflowSQLBeadRow](q, query)
		if err != nil {
			return nil, nil, fmt.Errorf("beads query %s: %w", tables.beads, err)
		}
		for _, row := range rows {
			bead := workflowSQLBeadFromRow(row)
			if bead.ID == "" {
				continue
			}
			if _, seen := beadIndex[bead.ID]; seen {
				continue
			}
			workflowBeads = append(workflowBeads, bead)
			beadIndex[bead.ID] = bead
		}
	}
	sort.SliceStable(workflowBeads, func(i, j int) bool {
		return workflowBeads[i].CreatedAt.Before(workflowBeads[j].CreatedAt)
	})
	return workflowBeads, beadIndex, nil
}

func workflowSQLQueryWorkflowDeps(q beads.SQLQuerier, tableSets []workflowSQLTableSet, rootID string) (map[string][]beads.Dep, error) {
	depMap := make(map[string][]beads.Dep)
	subquery := workflowSQLWorkflowIDsSubquery(tableSets, rootID)
	for _, tables := range tableSets {
		exists, err := workflowSQLTableExists(q, tables.deps)
		if err != nil {
			return nil, fmt.Errorf("check dep table %s: %w", tables.deps, err)
		}
		if !exists {
			continue
		}
		dependsOnExpr, err := workflowSQLDependsOnExpr(q, tables.deps, "d")
		if err != nil {
			return nil, err
		}
		query := `SELECT d.issue_id AS issue_id, ` + dependsOnExpr + ` AS depends_on,
				COALESCE(NULLIF(d.type, ''), 'blocks') AS dep_type
			FROM ` + tables.deps + ` d
			WHERE d.issue_id IN (` + subquery + `)
			  AND ` + dependsOnExpr + ` IN (` + subquery + `)`
		rows, err := workflowSQLQueryRows[workflowSQLDepRow](q, query)
		if err != nil {
			return nil, fmt.Errorf("deps query %s: %w", tables.deps, err)
		}
		for _, row := range rows {
			dep := workflowSQLDepFromRow(row)
			depMap[dep.IssueID] = append(depMap[dep.IssueID], dep)
		}
	}
	return depMap, nil
}

func workflowSQLHydrateWorkflowLabels(q beads.SQLQuerier, tableSets []workflowSQLTableSet, rootID string, workflowBeads []beads.Bead, beadIndex map[string]beads.Bead) error {
	labelMap := make(map[string][]string)
	subquery := workflowSQLWorkflowIDsSubquery(tableSets, rootID)
	for _, tables := range tableSets {
		exists, err := workflowSQLTableExists(q, tables.labels)
		if err != nil {
			return fmt.Errorf("check label table %s: %w", tables.labels, err)
		}
		if !exists {
			continue
		}
		query := `SELECT l.issue_id AS issue_id, l.label AS label
			FROM ` + tables.labels + ` l
			WHERE l.issue_id IN (` + subquery + `)`
		rows, err := workflowSQLQueryRows[workflowSQLLabelRow](q, query)
		if err != nil {
			return fmt.Errorf("labels query %s: %w", tables.labels, err)
		}
		for _, row := range rows {
			if row.IssueID == "" {
				continue
			}
			labelMap[row.IssueID] = append(labelMap[row.IssueID], row.Label)
		}
	}
	for i := range workflowBeads {
		if labels, ok := labelMap[workflowBeads[i].ID]; ok {
			workflowBeads[i].Labels = labels
			beadIndex[workflowBeads[i].ID] = workflowBeads[i]
		}
	}
	return nil
}

func workflowSQLAvailableTableSets(q beads.SQLQuerier) ([]workflowSQLTableSet, error) {
	issueExists, err := workflowSQLTableExists(q, workflowSQLIssueTables.beads)
	if err != nil {
		return nil, fmt.Errorf("check table %s: %w", workflowSQLIssueTables.beads, err)
	}
	wispExists, err := workflowSQLTableExists(q, workflowSQLWispTables.beads)
	if err != nil {
		return nil, fmt.Errorf("check table %s: %w", workflowSQLWispTables.beads, err)
	}
	tableSets := make([]workflowSQLTableSet, 0, 2)
	if issueExists {
		tableSets = append(tableSets, workflowSQLIssueTables)
	}
	if wispExists {
		tableSets = append(tableSets, workflowSQLWispTables)
	}
	if len(tableSets) == 0 {
		return nil, fmt.Errorf("no workflow bead SQL tables")
	}
	return tableSets, nil
}

type workflowSQLCountRow struct {
	N int `json:"n"`
}

func workflowSQLTableExists(q beads.SQLQuerier, table string) (bool, error) {
	query := `SELECT COUNT(*) AS n
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		  AND table_name = ` + sqlQuote(table)
	rows, err := workflowSQLQueryRows[workflowSQLCountRow](q, query)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	return rows[0].N > 0, nil
}

type workflowSQLColumnRow struct {
	ColumnName string `json:"column_name"`
}

func workflowSQLExistingColumns(q beads.SQLQuerier, table string, candidates []string) (map[string]bool, error) {
	if len(candidates) == 0 {
		return map[string]bool{}, nil
	}
	quotedCols := make([]string, 0, len(candidates))
	for _, column := range candidates {
		quotedCols = append(quotedCols, sqlQuote(column))
	}
	query := `SELECT column_name AS column_name
		FROM information_schema.columns
		WHERE table_schema = DATABASE()
		  AND table_name = ` + sqlQuote(table) + `
		  AND column_name IN (` + strings.Join(quotedCols, ", ") + `)`
	rows, err := workflowSQLQueryRows[workflowSQLColumnRow](q, query)
	if err != nil {
		return nil, err
	}
	columns := make(map[string]bool, len(candidates))
	for _, row := range rows {
		columns[strings.ToLower(strings.TrimSpace(row.ColumnName))] = true
	}
	return columns, nil
}

func workflowSQLDependsOnExpr(q beads.SQLQuerier, table, alias string) (string, error) {
	candidates := []string{"depends_on_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}
	columns, err := workflowSQLExistingColumns(q, table, candidates)
	if err != nil {
		return "", fmt.Errorf("read dep columns %s: %w", table, err)
	}
	expr, err := workflowSQLDependsOnExprFromColumns(alias, columns)
	if err != nil {
		return "", fmt.Errorf("dependency target columns %s: %w", table, err)
	}
	return expr, nil
}

func workflowSQLDependsOnExprFromColumns(alias string, columns map[string]bool) (string, error) {
	prefix := ""
	if strings.TrimSpace(alias) != "" {
		prefix = alias + "."
	}
	parts := make([]string, 0, 4)
	for _, column := range []string{"depends_on_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
		if columns[column] {
			parts = append(parts, "NULLIF("+prefix+column+", '')")
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no dependency target columns")
	}
	return "COALESCE(" + strings.Join(parts, ", ") + ", '')", nil
}

func workflowSQLWorkflowIDsSubquery(tableSets []workflowSQLTableSet, rootID string) string {
	quoted := sqlQuote(rootID)
	rootPath := beadmeta.JSONPath(beadmeta.RootBeadIDMetadataKey)
	parts := make([]string, 0, len(tableSets))
	for _, tables := range tableSets {
		parts = append(parts, `
			SELECT i.id FROM `+tables.beads+` i
			WHERE i.id = `+quoted+` OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '`+rootPath+`')) = `+quoted+`
		`)
	}
	return strings.Join(parts, " UNION ")
}

func workflowSQLDepFromRow(row workflowSQLDepRow) beads.Dep {
	typ := strings.TrimSpace(row.DepType)
	if typ == "" {
		typ = "blocks"
	}
	return beads.Dep{
		IssueID:     row.IssueID,
		DependsOnID: row.DependsOn,
		Type:        typ,
	}
}

// tryFullWorkflowSQL does the entire workflow snapshot via bd SQL — root
// discovery, bead fetch, dep fetch, and graph build. Returns nil error only on
// full success so the caller can use the slow path on any failure.
// errNoSQLWorkflowStores is the benign "this deployment has no SQL-backed
// workflow store to consult" outcome — distinct from a SQL store that exists
// but could not be reached or did not contain the workflow. The caller
// (buildWorkflowSnapshot) uses errors.Is to keep the routine no-SQL fallback
// quiet while still surfacing genuine fast-path failures (gascity#2940).
var errNoSQLWorkflowStores = errors.New("no sql workflow stores")

func (s *Server) tryFullWorkflowSQL(workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	candidates := workflowSQLCandidatesForWorkflowID(
		s.state,
		workflowID,
		fallbackScopeKind,
		fallbackScopeRef,
	)
	if len(candidates) == 0 {
		return nil, errNoSQLWorkflowStores
	}

	type sqlWorkflowRootMatch struct {
		candidate workflowSQLStoreCandidate
		root      beads.Bead
	}
	matches := make([]sqlWorkflowRootMatch, 0, len(candidates))
	// Retain the first genuine probe failure (bd SQL unreachable, query error)
	// so a fully-failed sweep surfaces the real cause rather than a synthetic
	// "not found" — that real cause is exactly what the #2940 fallback log
	// needs to be actionable. A clean miss (ok == false, err == nil) is not a
	// failure: the workflow simply isn't in that store, and a store with no SQL
	// capability is a benign skip, not a probe error.
	var firstProbeErr error
	sawSQLStore := false
	for _, candidate := range candidates {
		q, ok := workflowSQLQuerier(candidate.info.store)
		if !ok {
			continue
		}
		sawSQLStore = true
		root, ok, err := workflowSQLFindRoot(s.state.Config(), q, workflowID)
		if err != nil {
			if firstProbeErr == nil {
				firstProbeErr = fmt.Errorf("sql find root in %s: %w", candidate.info.ref, err)
			}
			continue
		}
		if !ok {
			continue
		}
		matches = append(matches, sqlWorkflowRootMatch{candidate: candidate, root: root})
	}
	if !sawSQLStore {
		return nil, errNoSQLWorkflowStores
	}
	if len(matches) == 0 {
		if firstProbeErr != nil {
			return nil, firstProbeErr
		}
		return nil, fmt.Errorf("workflow %q not found in sql stores", workflowID)
	}

	cityScopeRef := workflowCityScopeRef(s.state.CityName())
	workflowMatches := make([]workflowRootMatch, 0, len(matches))
	for _, match := range matches {
		workflowMatches = append(workflowMatches, workflowRootMatch{
			info: match.candidate.info,
			root: match.root,
		})
	}
	selected, ok := selectWorkflowRootMatch(workflowMatches, fallbackScopeKind, fallbackScopeRef, cityScopeRef)
	if !ok {
		return nil, fmt.Errorf("sql root match selection failed")
	}

	var chosen workflowSQLStoreCandidate
	foundCandidate := false
	for _, match := range matches {
		if match.root.ID == selected.root.ID && match.candidate.info.ref == selected.info.ref {
			chosen = match.candidate
			foundCandidate = true
			break
		}
	}
	if !foundCandidate {
		return nil, fmt.Errorf("sql root match candidate missing")
	}

	q, ok := workflowSQLQuerier(chosen.info.store)
	if !ok {
		return nil, fmt.Errorf("chosen sql store lost SQL capability")
	}

	workflowBeads, beadIndex, depMap, err := workflowSQLSnapshot(q, selected.root.ID)
	if err != nil {
		return nil, err
	}
	if len(workflowBeads) == 0 {
		return nil, fmt.Errorf("no beads found")
	}

	root, ok := beadIndex[selected.root.ID]
	if !ok {
		return nil, fmt.Errorf("root bead not found in SQL results")
	}

	store := &prefetchedDepStore{deps: depMap}

	// Collect physical deps only — logical nodes are computed by real-world app.
	workflowDeps, partial := collectWorkflowDeps(store, beadIndex)

	scopeKind, scopeRef := workflowSQLSnapshotScope(root, chosen.info, fallbackScopeKind, fallbackScopeRef)

	storeRef := chosen.info.ref
	beadResponses := make([]workflowBeadResponse, 0, len(workflowBeads))
	for _, bead := range workflowBeads {
		beadResponses = append(beadResponses, workflowBeadResponseFromBead(bead))
	}

	snapshot := &workflowSnapshotResponse{
		WorkflowID:        resolvedWorkflowID(root),
		RootBeadID:        root.ID,
		RootStoreRef:      storeRef,
		ScopeKind:         scopeKind,
		ScopeRef:          scopeRef,
		Beads:             beadResponses,
		Deps:              workflowDeps,
		LogicalNodes:      []LogicalNode{},
		LogicalEdges:      []workflowDepResponse{},
		ScopeGroups:       []ScopeGroup{},
		Partial:           partial,
		ResolvedRootStore: storeRef,
		StoresScanned:     []string{storeRef},
		SnapshotVersion:   snapshotIndex,
	}
	if snapshotIndex > 0 {
		snapshot.SnapshotEventSeq = &snapshotIndex
	}
	return snapshot, nil
}

func workflowSQLSnapshotScope(root beads.Bead, info workflowStoreInfo, fallbackScopeKind, fallbackScopeRef string) (string, string) {
	scopeKind := strings.TrimSpace(fallbackScopeKind)
	scopeRef := strings.TrimSpace(fallbackScopeRef)
	if scopeKind == "" {
		scopeKind = strings.TrimSpace(info.scopeKind)
	}
	if scopeRef == "" {
		scopeRef = strings.TrimSpace(info.scopeRef)
	}
	if sk := strings.TrimSpace(root.Metadata[beadmeta.ScopeKindMetadataKey]); sk != "" {
		scopeKind = sk
	}
	if sr := strings.TrimSpace(root.Metadata[beadmeta.ScopeRefMetadataKey]); sr != "" {
		scopeRef = sr
	}
	return scopeKind, scopeRef
}

// tryWorkflowSQL fetches the workflow snapshot for a resolved store through bd
// SQL. Returns a non-nil error if SQL is not available (caller should fall back
// to the bd subprocess Store path).
func (s *Server) tryWorkflowSQL(info workflowStoreInfo, rootID string) ([]beads.Bead, map[string]beads.Bead, map[string][]beads.Dep, error) {
	q, ok := workflowSQLQuerier(info.store)
	if !ok {
		return nil, nil, nil, fmt.Errorf("no sql capability for %s", info.ref)
	}
	return workflowSQLSnapshot(q, rootID)
}

func workflowSQLStoreCandidates(state State, requestedScopeKind, requestedScopeRef string) []workflowSQLStoreCandidate {
	requestedScopeKind = strings.TrimSpace(requestedScopeKind)
	requestedScopeRef = strings.TrimSpace(requestedScopeRef)
	if requestedScopeKind != "" && requestedScopeRef != "" {
		if info, ok := workflowStoreByRef(state, requestedScopeKind+":"+requestedScopeRef); ok {
			path, _ := workflowStorePath(state, info)
			return []workflowSQLStoreCandidate{{info: info, path: path}}
		}
		return nil
	}

	stores := workflowStores(state)
	candidates := make([]workflowSQLStoreCandidate, 0, len(stores))
	for _, info := range stores {
		path, _ := workflowStorePath(state, info)
		candidates = append(candidates, workflowSQLStoreCandidate{info: info, path: path})
	}
	return candidates
}

func workflowSQLRouteCandidate(state State, prefix string) (workflowSQLStoreCandidate, bool) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return workflowSQLStoreCandidate{}, false
	}
	cfg := state.Config()
	if cfg == nil {
		return workflowSQLStoreCandidate{}, false
	}
	candidates := workflowSQLStoreCandidates(state, "", "")
	if len(candidates) == 0 {
		return workflowSQLStoreCandidate{}, false
	}

	for _, rig := range cfg.Rigs {
		rigPath := resolveScopeRoot(state.CityPath(), rig.Path)
		if rigPath == "" {
			continue
		}
		storePath, ok := resolveRoutePrefix(rigPath, prefix)
		if !ok {
			continue
		}
		cleanStorePath := filepath.Clean(storePath)
		for _, candidate := range candidates {
			if candidate.path != "" && filepath.Clean(candidate.path) == cleanStorePath {
				return candidate, true
			}
		}
	}

	return workflowSQLStoreCandidate{}, false
}

func workflowStorePath(state State, info workflowStoreInfo) (string, bool) {
	// The dedicated graph store lives at its own legacy .gc/ location (or a gcg
	// Postgres schema), not at a rig/city path derivable here, so it has no
	// rig-path-derived route candidate. Skip it; the slow store-scan in
	// buildWorkflowSnapshot consults the graph store directly.
	if strings.HasPrefix(strings.TrimSpace(info.ref), workflowGraphStoreRefPrefix+":") {
		return "", false
	}
	switch strings.TrimSpace(info.scopeKind) {
	case beadmeta.ScopeKindCity:
		cityPath := strings.TrimSpace(state.CityPath())
		return cityPath, cityPath != ""
	case beadmeta.ScopeKindRig:
		cfg := state.Config()
		if cfg == nil {
			return "", false
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) != info.scopeRef {
				continue
			}
			rigPath := resolveScopeRoot(state.CityPath(), rig.Path)
			if rigPath == "" {
				return "", false
			}
			return rigPath, true
		}
	}
	return "", false
}

func workflowSQLFindRoot(cfg *config.City, q beads.SQLQuerier, workflowID string) (beads.Bead, bool, error) {
	tableSets, err := workflowSQLAvailableTableSets(q)
	if err != nil {
		return beads.Bead{}, false, err
	}

	hasWorkflowIDPrefix := workflowSQLWorkflowIDPrefix(cfg, workflowID) != ""
	if root, ok, err := workflowSQLGetBeadFromTables(q, tableSets, workflowID); err != nil {
		return beads.Bead{}, false, err
	} else if ok {
		if isWorkflowRoot(root) && matchesWorkflowID(root, workflowID) {
			return root, true, nil
		}
		if hasWorkflowIDPrefix {
			return beads.Bead{}, false, nil
		}
	}
	if hasWorkflowIDPrefix {
		return beads.Bead{}, false, nil
	}

	return workflowSQLFindRootByWorkflowID(q, tableSets, workflowID)
}

func workflowSQLWorkflowIDPrefix(cfg *config.City, workflowID string) string {
	return sling.BeadPrefixForCity(cfg, strings.TrimSpace(workflowID))
}

func workflowSQLGetBeadFromTables(q beads.SQLQuerier, tableSets []workflowSQLTableSet, id string) (beads.Bead, bool, error) {
	quoted := sqlQuote(id)
	for _, tables := range tableSets {
		query := `SELECT ` + workflowSQLBeadColumns + `
			FROM ` + tables.beads + ` i
			WHERE i.id = ` + quoted + `
			LIMIT 1`
		rows, err := workflowSQLQueryRows[workflowSQLBeadRow](q, query)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("get bead %s from %s: %w", id, tables.beads, err)
		}
		if len(rows) > 0 {
			return workflowSQLBeadFromRow(rows[0]), true, nil
		}
	}
	return beads.Bead{}, false, nil
}

func workflowSQLFindRootByWorkflowID(q beads.SQLQuerier, tableSets []workflowSQLTableSet, workflowID string) (beads.Bead, bool, error) {
	kindPath := beadmeta.JSONPath(beadmeta.KindMetadataKey)
	workflowIDPath := beadmeta.JSONPath(beadmeta.WorkflowIDMetadataKey)
	quoted := sqlQuote(workflowID)
	matches := make([]beads.Bead, 0, len(tableSets))
	for _, tables := range tableSets {
		query := `SELECT ` + workflowSQLBeadColumns + `
			FROM ` + tables.beads + ` i
			WHERE JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '` + kindPath + `')) = '` + beadmeta.KindWorkflow + `'
			  AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '` + workflowIDPath + `')) = ` + quoted + `
			ORDER BY i.created_at
			LIMIT 1`
		rows, err := workflowSQLQueryRows[workflowSQLBeadRow](q, query)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("find workflow %s in %s: %w", workflowID, tables.beads, err)
		}
		if len(rows) > 0 {
			matches = append(matches, workflowSQLBeadFromRow(rows[0]))
		}
	}
	if len(matches) == 0 {
		return beads.Bead{}, false, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].CreatedAt.Before(matches[j].CreatedAt)
	})
	return matches[0], true, nil
}

// prefetchedDepStore wraps a pre-fetched dep map to satisfy the beads.Store
// interface for collectWorkflowDeps, which calls store.DepList().
type prefetchedDepStore struct {
	beads.Store // embed nil Store — only DepList is called
	deps        map[string][]beads.Dep
}

func (s *prefetchedDepStore) DepList(id, direction string) ([]beads.Dep, error) {
	if direction == "down" {
		return s.deps[id], nil
	}
	// "up" direction — reverse lookup
	var result []beads.Dep
	for _, deps := range s.deps {
		for _, d := range deps {
			if d.DependsOnID == id {
				result = append(result, d)
			}
		}
	}
	return result, nil
}
