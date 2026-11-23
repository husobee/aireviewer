// SPDX-License-Identifier: MIT

// Package issueindex keeps a local, incrementally synced full-text index of
// a repository's issues for /pr-create issue linking (SPEC §8.2 v2,
// ADR 0013). Ranking is BM25 over SQLite FTS5 with titles weighted above
// bodies; embeddings can be added later behind the same Search.
package issueindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pure-Go SQLite with FTS5

	"github.com/husobee/aireviewer/internal/forge"
)

// Index is one repository's issue index.
type Index struct {
	db *sql.DB
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS issues (
  number     INTEGER PRIMARY KEY,
  ref        TEXT NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL,
  labels     TEXT NOT NULL,
  state      TEXT NOT NULL,
  url        TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  closed_at  INTEGER
);
CREATE VIRTUAL TABLE IF NOT EXISTS issues_fts USING fts5(
  title, body, labels, content='issues', content_rowid='number', tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS issues_ai AFTER INSERT ON issues BEGIN
  INSERT INTO issues_fts(rowid, title, body, labels) VALUES (new.number, new.title, new.body, new.labels);
END;
CREATE TRIGGER IF NOT EXISTS issues_ad AFTER DELETE ON issues BEGIN
  INSERT INTO issues_fts(issues_fts, rowid, title, body, labels) VALUES ('delete', old.number, old.title, old.body, old.labels);
END;
CREATE TRIGGER IF NOT EXISTS issues_au AFTER UPDATE ON issues BEGIN
  INSERT INTO issues_fts(issues_fts, rowid, title, body, labels) VALUES ('delete', old.number, old.title, old.body, old.labels);
  INSERT INTO issues_fts(rowid, title, body, labels) VALUES (new.number, new.title, new.body, new.labels);
END;
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// Path returns the index file for a repository under cacheDir.
func Path(cacheDir string, repo forge.Repo) string {
	return filepath.Join(cacheDir, "issues", repo.Host, filepath.FromSlash(repo.Project)+".db")
}

// Open opens or creates the index at path.
func Open(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("issue index %s: %w", path, err)
	}
	// Issue text from private repositories: owner-only, like the rest of
	// the cache (SQLite creates its files with the process umask).
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			_ = os.Chmod(p, 0o600)
		}
	}
	return &Index{db: db}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

func (ix *Index) meta(key string) string {
	var v string
	_ = ix.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

func (ix *Index) setMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Stats describes the index.
type Stats struct {
	Issues   int       `json:"issues"`
	Open     int       `json:"open"`
	Cursor   time.Time `json:"cursor" jsonschema:"latest issue update seen; the next sync fetches newer updates only"`
	SyncedAt time.Time `json:"synced_at"`
}

// Stats returns counts and sync times.
func (ix *Index) Stats() Stats {
	var s Stats
	_ = ix.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(state = 'open'), 0) FROM issues`).Scan(&s.Issues, &s.Open)
	s.Cursor, _ = time.Parse(time.RFC3339Nano, ix.meta("cursor"))
	s.SyncedAt, _ = time.Parse(time.RFC3339Nano, ix.meta("synced_at"))
	return s
}

// Lister pages through a repository's issues updated after a time, oldest
// update first. It is implemented by the forge adapters.
type Lister interface {
	ListIssuesUpdatedSince(ctx context.Context, repo forge.Repo, since time.Time, page int) (issues []forge.Issue, updated []time.Time, closed []*time.Time, more bool, err error)
}

// SyncResult reports one sync.
type SyncResult struct {
	Fetched int  `json:"fetched"`
	Full    bool `json:"full" jsonschema:"first sync of this repository"`
	Capped  bool `json:"capped" jsonschema:"stopped at the page limit; the next sync continues from the cursor"`
	Stats
}

// MaxPages caps one sync (100 issues per page).
const MaxPages = 50

// Sync fetches issues updated since the cursor and upserts them.
func (ix *Index) Sync(ctx context.Context, l Lister, repo forge.Repo, now time.Time) (SyncResult, error) {
	cur := ix.Stats().Cursor
	res := SyncResult{Full: cur.IsZero()}
	cursor := cur
	for page := 1; page <= MaxPages; page++ {
		issues, updated, closed, more, err := l.ListIssuesUpdatedSince(ctx, repo, cur, page)
		if err != nil {
			return res, err
		}
		tx, err := ix.db.BeginTx(ctx, nil)
		if err != nil {
			return res, err
		}
		for i, is := range issues {
			var closedAt any
			if closed[i] != nil {
				closedAt = closed[i].Unix()
			}
			if _, err := tx.Exec(`INSERT INTO issues(number, ref, title, body, labels, state, url, updated_at, closed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(number) DO UPDATE SET ref=excluded.ref, title=excluded.title, body=excluded.body, labels=excluded.labels,
				  state=excluded.state, url=excluded.url, updated_at=excluded.updated_at, closed_at=excluded.closed_at`,
				is.Number, is.Ref, is.Title, is.Body, strings.Join(is.Labels, " "), is.State, is.URL, updated[i].Unix(), closedAt); err != nil {
				_ = tx.Rollback()
				return res, err
			}
			if updated[i].After(cursor) {
				cursor = updated[i]
			}
		}
		if err := ix.setMeta(tx, "cursor", cursor.UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
		res.Fetched += len(issues)
		if !more {
			break
		}
		if page == MaxPages {
			res.Capped = true
		}
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err == nil {
		_ = ix.setMeta(tx, "synced_at", now.UTC().Format(time.RFC3339Nano))
		_ = tx.Commit()
	}
	res.Stats = ix.Stats()
	return res, nil
}

// Hit is one ranked result. Score is BM25 (higher is better here).
type Hit struct {
	Issue forge.Issue `json:"issue"`
	Score float64     `json:"score" jsonschema:"BM25 relevance, higher is better; comparable only within one query"`
}

// MatchQuery turns free text into a safe FTS5 query: each word becomes a
// quoted term (so FTS operators and column filters in the input are inert),
// joined with OR so partial matches still rank.
func MatchQuery(q string) string {
	var terms []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if len([]rune(w)) < 2 || seen[w] || len(terms) >= 32 {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
	}
	return strings.Join(terms, " OR ")
}

// ErrEmptyQuery means the query had no searchable words.
var ErrEmptyQuery = errors.New("query has no searchable words")

// Search ranks issues for q: open issues, plus closed ones closed within
// closedWithin of now (0 = open only).
func (ix *Index) Search(ctx context.Context, q string, closedWithin time.Duration, now time.Time, limit int) ([]Hit, error) {
	mq := MatchQuery(q)
	if mq == "" {
		return nil, ErrEmptyQuery
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	cutoff := int64(1 << 62)
	if closedWithin > 0 {
		cutoff = now.Add(-closedWithin).Unix()
	}
	rows, err := ix.db.QueryContext(ctx, `
		SELECT i.number, i.ref, i.title, i.body, i.labels, i.state, i.url, -bm25(issues_fts, 10.0, 1.0, 3.0) AS score
		FROM issues_fts JOIN issues i ON i.number = issues_fts.rowid
		WHERE issues_fts MATCH ? AND (i.state = 'open' OR (i.closed_at IS NOT NULL AND i.closed_at >= ?))
		ORDER BY bm25(issues_fts, 10.0, 1.0, 3.0) LIMIT ?`, mq, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Hit
	for rows.Next() {
		var h Hit
		var labels string
		if err := rows.Scan(&h.Issue.Number, &h.Issue.Ref, &h.Issue.Title, &h.Issue.Body, &labels, &h.Issue.State, &h.Issue.URL, &h.Score); err != nil {
			return nil, err
		}
		h.Issue.Labels = strings.Fields(labels)
		if h.Issue.Labels == nil {
			h.Issue.Labels = []string{}
		}
		h.Issue.Source = "index"
		out = append(out, h)
	}
	return out, rows.Err()
}
