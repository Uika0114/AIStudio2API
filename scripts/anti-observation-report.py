#!/usr/bin/env python3
"""Read only the body-free anti observations in an AIStudio2API request ledger.

Window uses root completion time, [from, to). Does not join bodies, auth, errors,
prompts, tool arguments or account identifiers. No network or writes.
"""
import argparse
from collections import Counter, defaultdict
from contextlib import closing
from datetime import datetime, timezone
import json
from pathlib import Path
import sqlite3

VERSION = 1


def timestamp(value):
    dt = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if dt.tzinfo is None:
        raise argparse.ArgumentTypeError('time must include timezone')
    return int(dt.timestamp() * 1000)


def ratio(n, d):
    return n / d if d else None


def measures():
    return dict(T=0, H=0, N=0, U=0, C=0, not_sent=0, send_unknown=0, not_injected=0,
                unknown_version=0, calls=0, legal=0, empty=0, invalid=0,
                rejected=0, unevaluated=0, physical_attempts=0, sent_attempts=0, fallback=0,
                pure_real_tools=0, other_output=0, no_output=0, output_classification_unknown=0)


def root_send_state(row, observations):
    known = [o for o in observations or [] if o.get("version") == VERSION]
    if any(o.get("sent") for o in known):
        return "sent"
    if (row.get("sent_attempt_count") or 0) > 0:
        return "sent"
    if known and len(known) == len(observations) and all(o.get("end") == "not_sent" for o in known):
        return "not_sent"
    return "send_unknown"


def summarize(rows, details=False):
    roots = dict(readable_completed=0, http_200=0, http_200_blocked=0,
                 anti=0, non_anti=0, selection_unknown=0, anti_observation_missing=0, anti_no_lifecycle=0,
                 sent=0, not_sent=0, send_unknown=0)
    strata = defaultdict(measures)
    cross = Counter()
    evidence = []
    requests = []
    seen_roots = set()
    earliest = latest = None
    for row in rows:
        at = row.get("time")
        if at is not None:
            earliest = at if earliest is None else min(earliest, at)
            latest = at if latest is None else max(latest, at)
        if details:
            requests.append(dict(row))
        roots['readable_completed'] += 1
        roots['http_200'] += row['status'] == 200
        blocked = row['status'] == 200 and row['state'] == 'blocked'
        roots['http_200_blocked'] += blocked
        raw = row.get("anti_observations")
        observations = json.loads(raw) if isinstance(raw, str) else raw
        if row.get("id") in seen_roots:
            raise ValueError("duplicate root row")
        seen_roots.add(row.get("id"))
        if observations is not None and not isinstance(observations, list):
            raise ValueError("observation column must be a list or NULL")
        roots[root_send_state(row, observations)] += 1
        anti = row.get('anti_truncation')
        if anti is None:
            roots['selection_unknown'] += 1
            continue
        if not anti:
            roots['non_anti'] += 1
            continue
        roots['anti'] += 1
        if observations is None:
            roots['anti_observation_missing'] += 1
            if blocked:
                cross[('200+blocked', 'root_missing_observation')] += 1
            continue
        roots["anti_no_lifecycle"] += not observations
        root_states = set()
        ids = set()
        for o in observations:
            # Persisted observations are unique by lifetime; duplicates are
            # invalid evidence, not another candidate or something to sum.
            if o['id'] in ids:
                raise ValueError('duplicate observation lifetime')
            ids.add(o['id'])
            key = (o.get('base_model') or row.get('base_model') or row['model'],
                   o.get('channel') or row['channel'], row['protocol'],
                   o.get('purpose', 'unknown'), o.get('system_prompt'), o.get('behavior', 'unknown'))
            group = strata[key]
            if o.get('version') != VERSION:
                group['unknown_version'] += 1
                root_states.add('unknown_version')
                continue
            rounds = o.get('rounds') or []
            group["physical_attempts"] += len(rounds)
            group["sent_attempts"] += sum(bool(r.get("sent")) for r in rounds)
            if not o.get('sent'):
                state = "not_sent" if o.get("end") == "not_sent" else "send_unknown"
                group[state] += 1
                root_states.add(state)
                continue
            if not o.get('injected'):
                group['not_injected'] += 1
                root_states.add('not_injected')
                continue
            group['T'] += 1
            calls = sum(r.get('calls', 0) for r in rounds)
            complete = bool(o.get('complete'))
            verdict = 'H' if calls else ('N' if complete else 'U')
            group[verdict] += 1
            group['C'] += complete
            root_states.add(o.get('purpose', 'unknown') + ':' + verdict)
            for r in rounds:
                if r.get('calls', 0) < sum(r.get(k, 0) for k in ('legal', 'invalid', 'rejected')):
                    raise ValueError('processed count exceeds raw observations')
                for name in ('calls', 'legal', 'empty', 'invalid', 'rejected'):
                    group[name] += r.get(name, 0)
                group['unevaluated'] += r.get('calls', 0) - sum(r.get(k, 0) for k in ('legal', 'invalid', 'rejected'))
                group['fallback'] += bool(r.get('fallback'))
                cross[('lifetime_round', o.get('purpose', 'unknown'), verdict, str(row['status']), row['state'], r.get('finish_reason', ''), r.get('end', 'unknown'))] += 1
            if not calls and complete:
                text = any(r.get('ordinary_text') for r in rounds)
                real = sum(r.get('real_tools', 0) for r in rounds)
                other = any(r.get("other_output") for r in rounds)
                group["other_output"] += bool(other)
                output_known = all("other_output" in r for r in rounds)
                if not text and not other:
                    if not output_known:
                        group["output_classification_unknown"] += 1
                    else:
                        group["pure_real_tools"] += bool(real)
                        group["no_output"] += not real
            if details:
                evidence.append(dict(root_id=row['id'], lifecycle_id=o['id'], purpose=o.get('purpose'),
                                     verdict=verdict, complete=complete, calls=calls,
                                     original_finishes=[r.get('finish_reason', '') for r in rounds], end=o.get('end')))
        if blocked:
            # One root per observed combination, never one root per attempt.
            cross[('200+blocked', '|'.join(sorted(root_states)) or 'no_lifecycle')] += 1
    groups = []
    for key, counts in sorted(strata.items(), key=lambda pair: str(pair[0])):
        counts['determined_fraction'] = ratio(counts['H'] + counts['N'], counts['T'])
        counts['complete_fraction'] = ratio(counts['C'], counts['T'])
        counts['observed_fraction_among_determined'] = ratio(counts['H'], counts['H'] + counts['N'])
        counts['legal_fraction_among_observed_calls'] = ratio(counts['legal'], counts['calls'])
        groups.append(dict(zip(('base_model', 'channel', 'protocol', 'purpose', 'system_prompt', 'behavior'), key), **counts))
    result = dict(format_version=VERSION, roots=roots, groups=groups,
                  blocked_rate_all_completed=ratio(roots['http_200_blocked'], roots['readable_completed']),
                  blocked_rate_http200=ratio(roots['http_200_blocked'], roots['http_200']),
                  cross=[dict(dimensions=list(k), count=v) for k, v in sorted(cross.items())],
                  limitations=[
                      'Based on readable database rows only; whole-row persistence loss cannot be verified by this source.',
                      'H includes incomplete positive observations; H/(H+N) is selected-sample observation, not population invocation or compliance rate.',
                      'Field availability, determined outcomes, complete event coverage and persistence coverage are distinct.',
                      'Auxiliary compaction and reply lifetimes are separate. Root IDs are not physical attempts.',
                      'Natural input/model/load and behavior versions differ; no causal efficacy claim.',
                      "Zero observed sends alone do not prove a final not-sent outcome; unfinished lifetimes remain unknown.",
                      "Older observations without other_output cannot prove no other visible/server-tool output.",
                  ])
    result["readable_completion_bounds_ms"] = dict(earliest=earliest, latest=latest)
    result["observation_field_fraction_among_anti_roots"] = ratio(roots["anti"] - roots["anti_observation_missing"], roots["anti"])
    if details:
        result['evidence'] = evidence
        result["requests"] = requests
    return result


def read_rows(path, start, end):
    uri = Path(path).resolve().as_uri() + '?mode=ro'
    with closing(sqlite3.connect(uri, uri=True)) as db:
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA query_only=ON')
        db.execute('BEGIN')
        metadata = bool(db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='request_metadata'").fetchone())
        columns = {r[1] for r in db.execute("PRAGMA table_info('request_metadata')")} if metadata else set()
        select = 'r.id,r.time,r.model,r.channel,r.protocol,r.status,r.state'
        select += ",m.anti_truncation,m.base_model,m.requested_model" if metadata else ",NULL AS anti_truncation,NULL AS base_model,NULL AS requested_model"
        for name in ("attempt_count", "sent_attempt_count"):
            select += ",m." + name if name in columns else ",NULL AS " + name
        select += ',m.anti_observations' if 'anti_observations' in columns else ',NULL AS anti_observations'
        join = ' LEFT JOIN request_metadata m ON m.id=r.id' if metadata else ''
        for row in db.execute('SELECT ' + select + ' FROM requests r' + join + ' WHERE r.time>=? AND r.time<? ORDER BY r.time,r.id', (start, end)):
            yield dict(row)


def filter_rows(rows, args):
    for row in rows:
        if any(getattr(args, arg, None) is not None and row.get(key) != getattr(args, arg)
               for arg, key in (("model", "requested_model"), ("base_model", "base_model"),
                                ("channel", "channel"), ("protocol", "protocol"),
                                ("status", "status"), ("state", "state"))):
            continue
        if args.anti != "all" and row.get("anti_truncation") != (args.anti == "true"):
            continue
        yield row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--db', required=True)
    parser.add_argument('--from', dest='start', type=timestamp, required=True)
    parser.add_argument('--to', dest='end', type=timestamp, required=True)
    parser.add_argument("--model", help="exact requested model, no prefix inference")
    parser.add_argument("--base-model")
    parser.add_argument("--channel")
    parser.add_argument("--protocol")
    parser.add_argument("--status", type=int)
    parser.add_argument("--state")
    parser.add_argument("--anti", choices=("all", "true", "false"), default="all")
    parser.add_argument('--details', action='store_true', help='include body-free request/lifetime evidence')
    args = parser.parse_args()
    if args.start >= args.end:
        parser.error('--from must precede --to')
    result = summarize(filter_rows(read_rows(args.db, args.start, args.end), args), args.details)
    result["source"] = dict(database=str(Path(args.db).resolve()), mode="read-only",
                          retention="request retention; this source cannot enumerate already-pruned or dropped rows")
    result["filters"] = {name: getattr(args, name) for name in ("model", "base_model", "channel", "protocol", "status", "state", "anti")}
    result['window'] = dict(from_inclusive=datetime.fromtimestamp(args.start/1000, timezone.utc).isoformat(),
                            to_exclusive=datetime.fromtimestamp(args.end/1000, timezone.utc).isoformat(), basis='root completion time')
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
