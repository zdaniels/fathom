package scheduler

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Routine is a named, reusable task: a prompt plus an optional skill pin.
// Routines are run on demand (`fathom routine run NAME`) and scheduled by name
// (`fathom schedule add NAME --every …`), which snapshots the prompt + skill
// into a job.
type Routine struct {
	Name      string
	Prompt    string
	Skill     string
	CreatedAt time.Time
}

// ValidRoutineName reports whether a name is usable: non-empty and a single
// token, so it reads cleanly as one CLI argument.
func ValidRoutineName(name string) bool {
	return name != "" && !strings.ContainsAny(name, " \t\n")
}

// SaveRoutine creates or replaces a routine by name (upsert). existed reports
// whether a routine with that name was already present, so the caller can say
// "created" vs "updated". created_at is preserved across updates.
func (s *Store) SaveRoutine(name, prompt, skill string, now time.Time) (existed bool, err error) {
	if !ValidRoutineName(name) {
		return false, fmt.Errorf("routine name %q must be a single word with no spaces", name)
	}
	if strings.TrimSpace(prompt) == "" {
		return false, fmt.Errorf("a routine needs a prompt")
	}
	_, getErr := s.GetRoutine(name)
	existed = getErr == nil
	_, err = s.db.Exec(
		`INSERT INTO routines (name, prompt, skill, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET prompt = excluded.prompt, skill = excluded.skill`,
		name, prompt, skill, now.UTC().Format(time.RFC3339Nano),
	)
	return existed, err
}

// GetRoutine returns one routine by name. Returns sql.ErrNoRows when absent.
func (s *Store) GetRoutine(name string) (Routine, error) {
	row := s.db.QueryRow(
		`SELECT name, prompt, skill, created_at FROM routines WHERE name = ?`, name,
	)
	return scanRoutine(row)
}

// ListRoutines returns all routines ordered by name.
func (s *Store) ListRoutines() ([]Routine, error) {
	rows, err := s.db.Query(`SELECT name, prompt, skill, created_at FROM routines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Routine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoutine removes a routine by name. Returns whether a row was removed.
func (s *Store) DeleteRoutine(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM routines WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func scanRoutine(sc interface {
	Scan(dest ...any) error
}) (Routine, error) {
	var r Routine
	var createdStr string
	var skillStr sql.NullString
	if err := sc.Scan(&r.Name, &r.Prompt, &skillStr, &createdStr); err != nil {
		return Routine{}, err
	}
	r.Skill = skillStr.String
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return r, nil
}
