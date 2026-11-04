#!/bin/sh
# The guard behind `make check-test-database-url` (and so `make
# test-integration`): refuses to let integration tests touch anything but a
# disposable database. TEST_DATABASE_URL must be set, be a postgres:// or
# postgresql:// URL, name a database ending in _test, and not name the same
# database as DATABASE_URL.
#
# "Same database" compares the parsed identity, not the raw string: user, host
# (localhost, 127.0.0.1, ::1 and an empty host are one host; case-insensitive),
# port (5432 when absent) and database name. A dbname= or database= query
# parameter overrides the name in the path, and host=/port=/user= query
# parameters override the authority, as libpq reads them.
#
# It never prints either URL, which may hold a password; only the rule that
# failed and the parsed database name. Proven by scripts/test-db-guard.sh.
set -u

# parse <url-or-keyword-dsn>: sets p_user, p_host, p_port, p_db.
parse() {
	s=$1
	p_user='' p_host='' p_port='' p_db='' q=''
	case $s in
	postgres://* | postgresql://*)
		s=${s#*://}
		case $s in *\?*) q=${s#*\?}; s=${s%%\?*} ;; esac
		case $s in
		*/*) authority=${s%%/*}; p_db=${s##*/} ;;
		*) authority=$s ;;
		esac
		case $authority in
		*@*) userinfo=${authority%@*}; hostport=${authority##*@} ;;
		*) userinfo=; hostport=$authority ;;
		esac
		p_user=${userinfo%%:*}
		case $hostport in
		\[*\]:*) p_host=${hostport%\]:*}; p_host=${p_host#\[}; p_port=${hostport##*\]:} ;;
		\[*\]) p_host=${hostport#\[}; p_host=${p_host%\]} ;;
		*:*) p_host=${hostport%%:*}; p_port=${hostport#*:} ;;
		*) p_host=$hostport ;;
		esac
		pairs=$q sep='&'
		;;
	*)
		# keyword DSN (only DATABASE_URL may be one): key=value pairs.
		pairs=$s sep=' '
		;;
	esac
	set -f
	old_ifs=$IFS
	IFS=$sep
	for kv in $pairs; do
		case $kv in
		dbname=* | database=*) p_db=${kv#*=} ;;
		host=*) p_host=${kv#host=} ;;
		port=*) p_port=${kv#port=} ;;
		user=*) p_user=${kv#user=} ;;
		esac
	done
	IFS=$old_ifs
	set +f
	p_host=$(printf '%s' "$p_host" | tr '[:upper:]' '[:lower:]')
	case $p_host in
	"" | localhost | 127.0.0.1 | ::1) p_host=localhost ;;
	esac
	[ -n "$p_port" ] || p_port=5432
}

u=${TEST_DATABASE_URL:-}
if [ -z "$u" ]; then
	echo "check-test-database-url: TEST_DATABASE_URL is not set — run 'make test-integration-local', or 'make test-db-up' and set the TEST_DATABASE_URL it prints" >&2
	exit 1
fi
case $u in
postgres://* | postgresql://*) ;;
*)
	echo "check-test-database-url: refused — TEST_DATABASE_URL must start with postgres:// or postgresql:// (keyword DSNs are refused; database name: not parsed)" >&2
	exit 1
	;;
esac

parse "$u"
t_user=$p_user t_host=$p_host t_port=$p_port db=$p_db
case $db in
*[!A-Za-z0-9_.-]*) shown="(not shown: unexpected characters)" ;;
"") shown="(empty)" ;;
*) shown=$db ;;
esac

case $db in
*_test) ;;
*)
	echo "check-test-database-url: refused — the database name must end in _test (database name: $shown)" >&2
	exit 1
	;;
esac

d=${DATABASE_URL:-}
if [ -n "$d" ]; then
	parse "$d"
	if [ "$t_user" = "$p_user" ] && [ "$t_host" = "$p_host" ] &&
		[ "$t_port" = "$p_port" ] && [ "$db" = "$p_db" ]; then
		echo "check-test-database-url: refused — TEST_DATABASE_URL names the same database as DATABASE_URL (same user, host, port and database name: $shown)" >&2
		exit 1
	fi
fi
