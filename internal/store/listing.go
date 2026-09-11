package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (s *Store) SetListing(ctx context.Context, taskIDs []string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM listing`); err != nil {
			return fmt.Errorf("set listing: %w", err)
		}
		now := time.Now().Unix()
		for i, id := range taskIDs {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO listing (pos, task_id, created_at) VALUES (?, ?, ?)`, i+1, id, now)
			if err != nil {
				return fmt.Errorf("set listing: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) Listing(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM listing ORDER BY pos`)
	if err != nil {
		return nil, fmt.Errorf("read listing: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read listing: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read listing: %w", err)
	}
	return out, nil
}

func (s *Store) ResolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var numbered []string
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	byPos := func(n int) (string, error) {
		if numbered == nil {
			list, err := s.Listing(ctx)
			if err != nil {
				return "", err
			}
			if len(list) == 0 {
				return "", ErrNoListing
			}
			numbered = list
		}
		if n < 1 || n > len(numbered) {
			return "", fmt.Errorf("no task numbered %d in the last listing (1-%d)", n, len(numbered))
		}
		return numbered[n-1], nil
	}

	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		from, to, numeric, err := parseRange(ref)
		if err != nil {
			return nil, err
		}
		if !numeric {
			add(ref)
			continue
		}
		for n := from; n <= to; n++ {
			id, err := byPos(n)
			if err != nil {
				return nil, err
			}
			add(id)
		}
	}
	return out, nil
}

func parseRange(ref string) (from, to int, numeric bool, err error) {
	lo, hi, isRange := strings.Cut(ref, "-")
	if !isRange {
		hi = lo
	}
	from, loErr := strconv.Atoi(lo)
	to, hiErr := strconv.Atoi(hi)
	if loErr != nil || hiErr != nil {
		return 0, 0, false, nil
	}
	if from < 1 || to < from {
		return 0, 0, true, fmt.Errorf("bad task range %q", ref)
	}
	return from, to, true, nil
}
