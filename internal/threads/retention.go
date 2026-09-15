package threads

import "time"

// PurgeDeletedBefore permanently deletes expired trash and its message/FTS
// records. Active conversations are never selected by this operation.
func (s *Store) PurgeDeletedBefore(cutoff time.Time) (int64, error) {
	result, err := s.db.Exec("DELETE FROM threads WHERE deleted_at IS NOT NULL AND deleted_at < ?", cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
