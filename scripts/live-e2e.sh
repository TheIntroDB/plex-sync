#!/usr/bin/env bash
#
# Run the live tests in internal/plexdb against a copy of a real Plex database.
#
# The tests are skipped unless TIDB_LIVE_PLEX_DB names a database, because a
# production library is not something to commit. This script prepares one.
#
# It never touches the source database: everything happens on a copy under a
# temporary directory.
#
# Usage:
#   scripts/live-e2e.sh                       # find Plex's database automatically
#   scripts/live-e2e.sh /path/to/library.db   # or name one
#
# What it does to the copy, and why:
#
#   1. Plex's database uses FTS4 tables with Plex's own ICU tokenizer
#      ("tokenize=collating") and an "icu_root" collation. No SQLite outside
#      Plex can parse that schema, and SQLite refuses to run a statement against
#      a table whose triggers it cannot prepare. The tags table has such a
#      trigger, so nothing outside Plex can create a marker tag. The copy has
#      those objects removed so the tests can exercise the write path; the
#      source is untouched.
#   2. A marker tag is created in the copy, because a server without Plex Pass
#      never has one. That is also the situation the tests need: a database with
#      a tag and no markers yet.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

source_db="${1:-}"

if [[ -z "$source_db" ]]; then
	candidates=(
		"$HOME/Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db"
		"/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db"
		"/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db"
	)
	for candidate in "${candidates[@]}"; do
		if [[ -f "$candidate" ]]; then
			source_db="$candidate"
			break
		fi
	done
fi

if [[ -z "$source_db" || ! -f "$source_db" ]]; then
	echo "No Plex database found. Pass one as the first argument." >&2
	exit 1
fi

if ! command -v sqlite3 >/dev/null 2>&1; then
	echo "sqlite3 is needed to prepare the copy." >&2
	exit 1
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/tidb-plex-live.XXXXXX")"
trap 'rm -rf "$work"' EXIT

copy="$work/library.db"
echo "copying $source_db"
echo "     to $copy"
cp "$source_db" "$copy"

echo "removing the schema objects only Plex's own SQLite can parse"
sqlite3 "$copy" "PRAGMA writable_schema=ON; DELETE FROM sqlite_master WHERE name LIKE '%fts4%' OR sql LIKE '%icu_root%'; PRAGMA writable_schema=OFF;"

echo "creating the marker tag Plex would have created (needs Plex Pass normally)"
sqlite3 "$copy" "INSERT INTO tags(tag_type, tag, created_at, updated_at) VALUES (12, 'Intro', strftime('%s','now'), strftime('%s','now'));"

if [[ "$(sqlite3 "$copy" "PRAGMA integrity_check;" | head -1)" != "ok" ]]; then
	echo "the prepared copy fails an integrity check; refusing to test against it" >&2
	sqlite3 "$copy" "PRAGMA integrity_check;" | head -5 >&2
	exit 1
fi

echo
echo "running the live tests"
TIDB_LIVE_PLEX_DB="$copy" go test -count=1 -v ./internal/plexdb/ -run Live

echo
echo "done. the source database was never opened for writing."