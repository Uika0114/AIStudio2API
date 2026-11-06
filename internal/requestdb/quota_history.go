package requestdb

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// importQuotaHistory covers only completed HTTP records before the first live
// observation. That fixed cutoff keeps restart/replay separate from live epochs.
// Historical StartedAt is a preparation-time proxy, never an exact send timestamp.
func importQuotaHistory(ctx context.Context, conn *sql.Conn, since, now time.Time) error {
	from := QuotaWindow(now)
	if !since.After(from) {
		return nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT r.id,
 COALESCE(json_extract(j.value,'$.id'),''),COALESCE(json_extract(j.value,'$.revision'),0),
 COALESCE(json_extract(j.value,'$.account_id'),''),COALESCE(json_extract(j.value,'$.started_at'),''),
 COALESCE(json_extract(j.value,'$.usage_source'),''),json_extract(j.value,'$.usage.total_tokens'),
 COALESCE(json_extract(j.value,'$.status_code'),0)
 FROM requests r,json_each(CASE WHEN json_valid(r.attempts) THEN r.attempts ELSE '[]' END) j
 WHERE r.time>=? AND r.time<? AND json_extract(j.value,'$.kind')='generation'
 AND json_extract(j.value,'$.sent')=1 AND json_extract(j.value,'$.channel')='playground'
 AND json_extract(j.value,'$.base_model') IN ('gemini-4-argon','models/gemini-4-argon')`, from.UnixMilli(), since.UnixMilli())
	if err != nil {
		return err
	}
	type historical struct {
		root, id, account string
		revision          int64
		at                time.Time
		tokens            int64
		reported, limited bool
	}
	items := make(map[[2]string]historical)
	var skipped int64
	for rows.Next() {
		var item historical
		var at, source string
		var tokens sql.NullInt64
		var status int
		if err = rows.Scan(&item.root, &item.id, &item.revision, &item.account, &at, &source, &tokens, &status); err != nil {
			_ = rows.Close()
			return err
		}
		item.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil || item.root == "" || item.id == "" || item.account == "" || item.revision < 0 {
			skipped++
			continue
		}
		if item.at.Before(from) || !item.at.Before(since) {
			continue
		}
		item.reported = source == "upstream" && tokens.Valid && tokens.Int64 >= 0 && tokens.Int64 < 1<<53
		if item.reported {
			item.tokens = tokens.Int64
		}
		item.limited = status == 429
		key := [2]string{item.root, item.id}
		if old, exists := items[key]; !exists || item.revision > old.revision {
			items[key] = item
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range items {
		_, err = tx.ExecContext(ctx, `INSERT INTO quota17_argon_attempts
 (epoch,id,revision,account,window,sent_at,tokens,reported,ended,limited) VALUES(?,?,?,?,?,?,?,?,1,?)
 ON CONFLICT(epoch,id) DO UPDATE SET revision=excluded.revision,
 tokens=CASE WHEN excluded.reported=1 THEN excluded.tokens ELSE quota17_argon_attempts.tokens END,
 reported=MAX(quota17_argon_attempts.reported,excluded.reported),limited=MAX(quota17_argon_attempts.limited,excluded.limited)
 WHERE excluded.revision>quota17_argon_attempts.revision AND excluded.account=quota17_argon_attempts.account AND excluded.sent_at=quota17_argon_attempts.sent_at`,
			"history/"+item.root, item.id, item.revision, item.account, from.UnixMilli(), item.at.UnixMilli(), item.tokens, item.reported, item.limited)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO quota17_history(window,until,attempts,skipped) VALUES(?,?,?,?)
 ON CONFLICT(window) DO UPDATE SET until=excluded.until,attempts=excluded.attempts,skipped=excluded.skipped`, from.UnixMilli(), since.UnixMilli(), len(items), skipped)
	if err != nil {
		return err
	}
	return tx.Commit()
}
