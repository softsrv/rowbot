#!/bin/sh
# Proves `make check-test-database-url` refuses every TEST_DATABASE_URL that is
# not a disposable *_test Postgres database, says how to get one when it is
# unset, and never prints the URL (which may hold a password).
# Run with: make test-db-guard. Needs no database.
set -u

repo_root=$(cd "$(dirname "$0")/.." && pwd)
fails=0
out=

# run_guard <env assignments...>
# Runs the guard with only PATH, HOME and the given variables; sets $out, $rc.
run_guard() {
	out=$(env -i PATH="$PATH" HOME="$HOME" "$@" make -s -C "$repo_root" check-test-database-url 2>&1)
	rc=$?
}

# run_guard_cli <make command-line assignments...>
# Same, but the variables are passed on the make command line.
run_guard_cli() {
	out=$(env -i PATH="$PATH" HOME="$HOME" make -s -C "$repo_root" check-test-database-url "$@" 2>&1)
	rc=$?
}

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "  output: $2"; fails=$((fails + 1)); }

# check_refused <name> <required substring>: judges the last run.
check_refused() {
	name=$1 want=$2
	if [ "$rc" -eq 0 ]; then
		fail "$name (guard accepted; it must refuse)" "$out"
	elif ! printf '%s' "$out" | grep -qF -- "$want"; then
		fail "$name (refusal does not contain '$want')" "$out"
	else
		pass "$name"
	fi
}

# expect_refused <name> <required substring> <env assignments...>
expect_refused() {
	name=$1 want=$2
	shift 2
	run_guard "$@"
	check_refused "$name" "$want"
}

url_test='postgres://u:p@localhost:5432/rowbot_test?sslmode=disable'

# 1. Unset: refused, and says how to get a test database.
expect_refused "unset is refused and names make test-db-up" "make test-db-up"

# 2. Equal to DATABASE_URL, even though the name ends in _test.
expect_refused "equal to DATABASE_URL is refused" "equals DATABASE_URL" \
	"TEST_DATABASE_URL=$url_test" "DATABASE_URL=$url_test"

# 3. A database name not ending in _test.
expect_refused "non-_test name is refused" "database name: rowbot)" \
	'TEST_DATABASE_URL=postgres://u:p@localhost:5432/rowbot?sslmode=disable'

# 4. A keyword DSN, even one naming a _test database.
expect_refused "keyword DSN is refused" "postgres:// or postgresql://" \
	'TEST_DATABASE_URL=host=localhost dbname=rowbot_test'

# 5. An empty database name.
expect_refused "empty name is refused" "database name: (empty)" \
	'TEST_DATABASE_URL=postgres://u:p@localhost:5432/?sslmode=disable'

# 6. Host with no path at all.
expect_refused "URL with no database path is refused" "must end in _test" \
	'TEST_DATABASE_URL=postgres://u:p@localhost:5432'

# 7. A value passed on the make command line is checked the same way.
run_guard_cli 'TEST_DATABASE_URL=postgres://u:p@localhost:5432/rowbot'
check_refused "command-line value is checked too" "must end in _test"

# 8. A refused URL never leaks its password.
marker='S3cretMarkerPw9'
run_guard "TEST_DATABASE_URL=postgres://u:$marker@localhost:5432/prod_db?sslmode=disable" \
	"DATABASE_URL=postgres://u:$marker@localhost:5432/app?sslmode=disable"
if [ "$rc" -eq 0 ]; then
	fail "marker password case (guard accepted; it must refuse)" "$out"
elif printf '%s' "$out" | grep -qF -- "$marker"; then
	fail "refusal leaks the password from the URL" "$out"
elif ! printf '%s' "$out" | grep -qF -- "must end in _test"; then
	fail "refusal does not name the rule that failed" "$out"
else
	pass "refusal names the rule and never prints the password"
fi
# The DATABASE_URL-equality refusal must not leak it either.
run_guard "TEST_DATABASE_URL=postgres://u:$marker@localhost:5432/x_test" \
	"DATABASE_URL=postgres://u:$marker@localhost:5432/x_test"
if [ "$rc" -eq 0 ] || printf '%s' "$out" | grep -qF -- "$marker"; then
	fail "equality refusal must refuse without printing the password" "$out"
else
	pass "equality refusal never prints the password"
fi

# 9. Accepted: a *_test postgres URL, DATABASE_URL unset.
for u in "$url_test" 'postgresql://u:p@localhost:55432/rowbot_test'; do
	run_guard "TEST_DATABASE_URL=$u"
	if [ "$rc" -ne 0 ]; then
		fail "a *_test postgres URL is accepted" "$out"
	else
		pass "a *_test postgres URL is accepted"
	fi
done

if [ "$fails" -ne 0 ]; then
	echo "test-db-guard: $fails case(s) failed"
	exit 1
fi
echo "test-db-guard: all cases passed"
