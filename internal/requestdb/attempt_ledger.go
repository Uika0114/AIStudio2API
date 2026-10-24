package requestdb

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Keep requests at the release's 20 columns: its positional INSERT must remain
// valid after a rollback. Historical/old-writer rows have no metadata row.
const requestMetadataSchema = `CREATE TABLE IF NOT EXISTS request_metadata (
 id TEXT PRIMARY KEY NOT NULL,
 requested_model TEXT, base_model TEXT, requested_channel TEXT, anti_truncation INTEGER,
 usage_known INTEGER, usage_source TEXT, attempt_count INTEGER, sent_attempt_count INTEGER)`

// Old releases can prune requests without knowing this table. Only remove the
// orphan metadata, never a request; a newly committed request+metadata is atomic.
const deleteOrphanRequestMetadata = `DELETE FROM request_metadata
 WHERE NOT EXISTS (SELECT 1 FROM requests WHERE requests.id = request_metadata.id)`

func addAttemptLedger(db *sql.DB) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if _, err := tx.Exec(requestMetadataSchema); err != nil {
		return err
	}
	var hasObservation int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_metadata') WHERE name='anti_observations'`).Scan(&hasObservation); err != nil {
		return err
	}
	if hasObservation == 0 {
		if _, err := tx.Exec(`ALTER TABLE request_metadata ADD COLUMN anti_observations TEXT`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(deleteOrphanRequestMetadata); err != nil {
		return err
	}
	return tx.Commit()
}

// Called only after a new logical request was inserted, inside the same
// transaction. A duplicate request must not overwrite its original metadata.
func writeRequestMetadata(tx *sql.Tx, row *Row) error {
	var observations any
	if row.AntiObservations != nil {
		encoded, err := json.Marshal(row.AntiObservations)
		if err != nil {
			return err
		}
		observations = string(encoded)
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO request_metadata
	 (id,requested_model,base_model,requested_channel,anti_truncation,usage_known,usage_source,attempt_count,sent_attempt_count,anti_observations)
	 VALUES (?,NULLIF(?,''),NULLIF(?,''),?,?,?,NULLIF(?,''),?,?,?)`,
		row.ID, row.RequestedModel, row.BaseModel, row.RequestedChannel, row.AntiTruncation,
		row.UsageKnown, row.UsageSource, row.AttemptCount, row.SentAttemptCount, observations)
	return err
}
